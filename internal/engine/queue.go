package engine

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/harness"
	"dp-agt-dsh/internal/store"
)

// ═══════════════════════════════════════════════════════════════════════════
// 任务队列状态机（与 dp-agt 完全对齐）
//
//  提交 ──→ 同 linkid 有 active 任务？ ──是──→ 拒绝 2003（不入队，不写 DB）
//   │               │否
//   │   pending+running >= max_pending_tasks？ ──是──→ 拒绝 2002
//   │               │否
//   │         写入 DB（pending）+ 加入队列
//   │               │
//   pump ──→ running < max_concurrent？ ──是──→ 出队 → running++ → processItem
//   │               │否
//   │          等待（下一个 goroutine 完成时 pump 再次被调用）
//
// pending 只表示「并发槽位已满，等待执行」，不表示「等待同 linkid 的前序任务」。
// 同一 linkid 同一时刻只允许一个 active 任务，提交侧直接拒绝，不排队。
// ═══════════════════════════════════════════════════════════════════════════

// Queue 异步任务队列
type Queue struct {
	cfg   *config.Config
	store *store.Store

	mu      sync.Mutex
	pending []*QueueItem
	running int

	// dsMu 保护 ds 的懒启动/重建，避免并发任务同时启动多个 dsh 子进程。
	dsMu sync.Mutex
	ds   *harness.Client

	// runningSessions 记录运行中任务的 task_id → sessionID，供管理员取消 processing 任务。
	runningSessions sync.Map

	// inflight 记录「已从 pending 出队、但尚未注册 session」的过渡期任务。
	// 解决 pump 弹出到 runningSessions.Store 之间取消失败的竞态窗口（Bug #3）。
	inflight sync.Map

	// cancelRequested 记录「处理中」任务被管理员请求取消、但 ds.Run 尚未返回的标记。
	// 取消落库延后到 processItem 中 ds.Run 返回后：若在 ds.Run 阻塞期间就改 DB 状态为
	// cancelled，HasActiveByLinkid 会放行同 linkid 的新任务，而旧 session 尚未释放，
	// 造成两个同 linkid 任务重叠。
	cancelRequested sync.Map
}

type QueueItem struct {
	TaskID string
	Path   string
	Iface  *store.AgentInterface
	Body   map[string]interface{}
}

func NewQueue(cfg *config.Config, s *store.Store) *Queue {
	q := &Queue{
		cfg:   cfg,
		store: s,
	}
	q.recoverOrphanedTasks()
	return q
}

// TryEnqueue 原子地完成「同 linkid 拒绝 + 队列上限检查 + 写 DB + 入队」，
// 不可拆分：HasActiveByLinkid 与 Enqueue 在同一锁内完成，消除竞态窗口。
//
// 返回值：
//
//	code==0     → 入队成功，item 非 nil
//	code==2002  → 队列已满，msg 含详情
//	code==2003  → 同 linkid 有未完成任务，msg 含详情
func (q *Queue) TryEnqueue(iface *store.AgentInterface, body map[string]interface{}) (code int, msg string, item *QueueItem) {
	q.mu.Lock()
	defer q.mu.Unlock()

	linkID, _ := body["linkid"].(string)

	// 1) 同 linkid 有活跃任务 → 直接拒绝（与 dp-agt 一致）
	if q.store.HasActiveByLinkid(linkID) {
		return 2003, "该公文尚有任务正在处理中，请等待完成后再次提交", nil
	}

	// 2) 队列上限（pending + running）
	maxPending := q.cfg.Sys.MaxPendingTasks
	activeCount := q.running + len(q.pending)
	if activeCount >= maxPending {
		return 2002, "任务队列已满，当前任务数 " + itoa(activeCount) + "/" + itoa(maxPending) + "，请稍后重试", nil
	}

	// 3) 写入 DB + 入队
	taskID := newUUID()
	bodyJSON, _ := json.Marshal(body)

	resp := fmt.Sprintf(`{"code":0,"timestamp":"%s","message":"success","task_id":"%s","linkid":"%s"}`,
		store.TimeNowFmt(), taskID, linkID)

	t := &store.TaskLog{
		TaskID:          taskID,
		LinkID:          linkID,
		Path:            iface.Path,
		InterfaceID:     iface.ID,
		Status:          store.StatusPending,
		RequestBody:     string(bodyJSON),
		RequestResponse: resp,
		CreatedAt:       store.TimeNowFmt(),
		CreatedTs:       store.TimeNowMs(),
	}
	_ = q.store.CreateTaskLog(t)

	// 任务提交即创建/复用 session，保证「会话管理」「会话记录」对 pending 任务也立即可见。
	if existing, _ := q.store.GetSessionByLinkid(linkID); existing != nil {
		_ = q.store.TouchSession(existing.SessionID)
		t.SessionID = existing.SessionID
	} else {
		sessionID := "s-" + taskID
		_ = q.store.UpsertSession(sessionID, linkID)
		t.SessionID = sessionID
	}
	if t.SessionID != "" {
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": t.SessionID})
	}

	item = &QueueItem{TaskID: taskID, Path: iface.Path, Iface: iface, Body: body}
	q.pending = append(q.pending, item)

	// 入队快照：记录提交瞬间的并发槽位占用，用于定位「排队中」是否由槽位满导致。
	slog.Info("enqueue_snapshot",
		"task", taskID,
		"link", linkID,
		"running", q.running,
		"pending", len(q.pending),
		"max_concurrent_tasks", q.cfg.Sys.MaxConcurrentTasks,
		"max_pending_tasks", q.cfg.Sys.MaxPendingTasks)

	return 0, "success", item
}

// Enqueue 兼容旧调用方的简单入队（不复核 HasActiveByLinkid/队列上限，
// 仅用于 recoverOrphanedTasks 等内部场景）。
func (q *Queue) Enqueue(iface *store.AgentInterface, body map[string]interface{}) *QueueItem {
	taskID := newUUID()
	linkID, _ := body["linkid"].(string)
	bodyJSON, _ := json.Marshal(body)

	resp := fmt.Sprintf(`{"code":0,"timestamp":"%s","message":"success","task_id":"%s","linkid":"%s"}`,
		store.TimeNowFmt(), taskID, linkID)

	t := &store.TaskLog{
		TaskID:          taskID,
		LinkID:          linkID,
		Path:            iface.Path,
		InterfaceID:     iface.ID,
		Status:          store.StatusPending,
		RequestBody:     string(bodyJSON),
		RequestResponse: resp,
		CreatedAt:       store.TimeNowFmt(),
		CreatedTs:       store.TimeNowMs(),
	}
	_ = q.store.CreateTaskLog(t)

	if existing, _ := q.store.GetSessionByLinkid(linkID); existing != nil {
		_ = q.store.TouchSession(existing.SessionID)
		t.SessionID = existing.SessionID
	} else {
		sessionID := "s-" + taskID
		_ = q.store.UpsertSession(sessionID, linkID)
		t.SessionID = sessionID
	}
	if t.SessionID != "" {
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": t.SessionID})
	}

	item := &QueueItem{TaskID: taskID, Path: iface.Path, Iface: iface, Body: body}
	q.mu.Lock()
	q.pending = append(q.pending, item)
	q.mu.Unlock()

	go q.pump()
	return item
}

// recoverOrphanedTasks 启动时恢复上次未完成的任务
func (q *Queue) recoverOrphanedTasks() {
	active, err := q.store.ListActiveTasks()
	if err != nil {
		return
	}
	resumed := 0
	killed := 0
	for _, task := range active {
		switch task.Status {
		case store.StatusPending:
			iface, ifaceErr := q.store.GetInterfaceByPath(task.Path)
			if ifaceErr != nil || iface == nil {
				_ = q.store.UpdateTaskResult(task.TaskID, map[string]interface{}{
					"status":        string(store.StatusFailed),
					"error_message": "服务重启后接口不存在，任务无法恢复: " + task.Path,
					"finished_at":   store.TimeNowFmt(),
					"finished_ts":   store.TimeNowMs(),
				})
				killed++
				continue
			}
			var body map[string]interface{}
			if json.Unmarshal([]byte(task.RequestBody), &body) == nil {
				q.pending = append(q.pending, &QueueItem{
					TaskID: task.TaskID,
					Path:   task.Path,
					Iface:  iface,
					Body:   body,
				})
				resumed++
			} else {
				_ = q.store.UpdateTaskResult(task.TaskID, map[string]interface{}{
					"status":        string(store.StatusFailed),
					"error_message": "服务重启后请求体解析失败，任务无法恢复",
					"finished_at":   store.TimeNowFmt(),
					"finished_ts":   store.TimeNowMs(),
				})
				killed++
			}
		case store.StatusProcessing:
			_ = q.store.UpdateTaskResult(task.TaskID, map[string]interface{}{
				"status":        string(store.StatusFailed),
				"error_message": "服务重启：任务被中断（原进程已退出）",
				"finished_at":   store.TimeNowFmt(),
				"finished_ts":   store.TimeNowMs(),
			})
			killed++
		}
	}
	if resumed > 0 || killed > 0 {
		slog.Info("recovered", "resumed", resumed, "killed", killed)
		if resumed > 0 {
			go q.pump()
		}
	}
}

// Pump 对外暴露的队列消费入口（内部 goroutine 结束后也会自行调用 pump）。
func (q *Queue) Pump() {
	q.pump()
}

// pump 消费 pending 队列，受 max_concurrent_tasks 限制。
// 不同 linkid 之间可以并发；同一 linkid 的并发由提交侧 HasActiveByLinkid 保证。
// 不再使用 linkChains 串行化——提交侧已阻止同 linkid 并发入队。
func (q *Queue) pump() {
	maxConc := q.cfg.Sys.MaxConcurrentTasks

	q.mu.Lock()
	for q.running < maxConc && len(q.pending) > 0 {
		item := q.pending[0]
		q.pending = q.pending[1:]
		q.running++
		q.mu.Unlock()

		q.inflight.Store(item.TaskID, false)

		go func(it *QueueItem) {
			defer q.inflight.Delete(it.TaskID)

			q.processItem(it)

			q.mu.Lock()
			q.running--
			q.mu.Unlock()
			q.pump()
		}(item)

		q.mu.Lock()
	}

	// 出队循环结束后仍有任务停留在 pending：说明并发槽位已满（running >= max_concurrent_tasks），
	// 这些任务需等待某个 running 任务结束后再次触发 pump 才能执行。
	if len(q.pending) > 0 {
		queued := make([]string, 0, len(q.pending))
		for _, it := range q.pending {
			queued = append(queued, it.TaskID)
		}
		slog.Info("task queued: no free concurrent slot",
			"queued_tasks", queued,
			"running", q.running,
			"pending", len(q.pending),
			"max_concurrent_tasks", q.cfg.Sys.MaxConcurrentTasks,
			"max_pending_tasks", q.cfg.Sys.MaxPendingTasks)
	}
	q.mu.Unlock()
}

func (q *Queue) processItem(item *QueueItem) {
	taskID := item.TaskID
	linkID, _ := item.Body["linkid"].(string)

	// 覆盖 inflight 窗口：任务从 pending 弹出后、注册 runningSessions 前被取消。
	if v, ok := q.inflight.Load(taskID); ok && v == true {
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":        string(store.StatusCancelled),
			"error_message": "任务已被管理员取消",
			"finished_at":   store.TimeNowFmt(),
			"finished_ts":   store.TimeNowMs(),
		})
		slog.Info("task cancelled in inflight window", "task", taskID, "link", linkID)
		return
	}

	// 标记开始执行：start_at 只在这里写一次，代表「真正开始跑」的时刻；
	// 排队等待的时间（pending → processing 之间的间隔）由此与 created_at 区分开。
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"status":    string(store.StatusProcessing),
		"start_at":  store.TimeNowFmt(),
		"start_ts":  store.TimeNowMs(),
	})
	slog.Info("processing", "task", taskID, "link", linkID, "path", item.Path)

	// 读提示词模板
	template := LoadPromptFromDisk(item.Path)
	if template == "" {
		q.failTask(taskID, "提示词文件不存在: "+item.Path)
		return
	}

	// 渲染
	rendered := RenderPrompt(template, item.Body, taskID)
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"prompt": rendered})

	// 查 session
	existingSession, _ := q.store.GetSessionByLinkid(linkID)
	var resumeID string
	if existingSession != nil {
		resumeID = existingSession.SessionID
		// 有新任务命中该 session 时刷新「更新时间」，避免被定期清理误删
		q.store.TouchSession(resumeID)
	}

	sessionID := resumeID
	if sessionID == "" {
		sessionID = "s-" + taskID
	}
	_ = q.store.UpsertSession(sessionID, linkID)
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": sessionID})

	// 创建/复用 workdir（按 sessionID 命名，位于 runtime/tasks，不落在 data/ 挂载卷内）
	runtimeTasksDir := q.cfg.RuntimeTasksDir()
	if err := os.MkdirAll(runtimeTasksDir, 0755); err != nil {
		q.failTask(taskID, "创建运行时目录失败: "+err.Error())
		return
	}
	workDir := SessionWorkDir(runtimeTasksDir, sessionID)
	_, err := BuildTaskDir(workDir, q.store, q.cfg.RawConfig(), q.cfg.Sys.DataDir)
	if err != nil {
		q.failTask(taskID, "创建工作目录失败: "+err.Error())
		return
	}

	// 启动 dsh
	if err := q.ensureDSH(); err != nil {
		q.failTask(taskID, "启动 dsh 运行时失败: "+err.Error())
		return
	}

	// 获取 stable 的 ds 引用（ensureDSH 成功后，ds 在 dsMu 保护下为非 nil）。
	q.dsMu.Lock()
	ds := q.ds
	q.dsMu.Unlock()

	// resume 或新建
	if resumeID != "" {
		if err := ds.ResumeSession(resumeID); err != nil {
			slog.Warn("resume failed, starting fresh", "session", resumeID, "err", err)
			sessionID = "s-" + taskID
			_ = q.store.UpsertSession(sessionID, linkID)
			_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": sessionID})
		}
	} else {
		// 新 session：显式用 session/new + cwd 创建，让工作目录一开始就正确。
		realID, err := ds.NewSession(workDir)
		if err != nil {
			slog.Warn("session/new failed, falling back to prompt-time create", "session", sessionID, "err", err)
		} else {
			sessionID = realID
			_ = q.store.UpsertSession(sessionID, linkID)
			_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": sessionID})
		}
	}

	q.runningSessions.Store(taskID, sessionID)
	defer q.runningSessions.Delete(taskID)
	defer q.cancelRequested.Delete(taskID)

	if v, ok := q.inflight.Load(taskID); ok && v == true {
		_ = ds.CancelSession(sessionID)
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":        string(store.StatusCancelled),
			"error_message": "任务已被管理员取消",
			"finished_at":   store.TimeNowFmt(),
			"finished_ts":   store.TimeNowMs(),
		})
		PublishStream(sessionID, harness.Notification{Method: "end"})
		slog.Info("task cancelled just before execution", "task", taskID, "link", linkID)
		return
	}

	// 兜底切换 session 的工作目录
	if err := ds.SetWorkingDirectory(sessionID, workDir); err != nil {
		q.failTask(taskID, "设置工作目录失败: "+err.Error())
		return
	}

	var allEvents []harness.SessionEvent
	var lastPersist time.Time

	result, err := ds.Run(sessionID, rendered, func(n harness.Notification) {
		PublishStream(sessionID, n)

		if n.Method == "session.event" {
			if ev, ok := n.Payload["event"].(map[string]interface{}); ok {
				allEvents = append(allEvents, harness.SessionEvent{
					Type: getStr(ev, "type"),
					Data: getMap(ev, "data"),
					Time: getInt64(ev, "time"),
				})
				if time.Since(lastPersist) >= time.Second {
					if trace, err := json.Marshal(allEvents); err == nil {
						_ = q.store.UpsertSdkTrace(taskID, string(trace))
					}
					lastPersist = time.Now()
				}
			}
		}
	})

	// 检查任务是否在运行期间被管理员取消。
	// 取消标记来自内存 cancelRequested（CancelTask 置位），而非 DB 状态：
	// 取消期间 DB 仍需保持 processing，否则 HasActiveByLinkid 会放行同 linkid 新任务。
	if _, ok := q.cancelRequested.Load(taskID); ok {
		trace, _ := json.Marshal(allEvents)
		_ = q.store.UpsertSdkTrace(taskID, string(trace))
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":        string(store.StatusCancelled),
			"session_id":    sessionID,
			"sdk_output":    "",
			"error_message": "任务已被管理员停止",
			"finished_at":   store.TimeNowFmt(),
			"finished_ts":   store.TimeNowMs(),
		})
		PublishStream(sessionID, harness.Notification{Method: "end"})
		slog.Info("task cancelled", "task", taskID, "link", linkID)
		return
	}

	if err != nil {
		trace, _ := json.Marshal(allEvents)
		_ = q.store.UpsertSdkTrace(taskID, string(trace))
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":        string(store.StatusFailed),
			"session_id":    sessionID,
			"sdk_output":    "",
			"error_message": err.Error(),
			"finished_at":   store.TimeNowFmt(),
			"finished_ts":   store.TimeNowMs(),
		})
		PublishStream(sessionID, harness.Notification{Method: "end"})
		slog.Error("task failed", "task", taskID, "link", linkID, "err", err)
		return
	}

	// 成功
	_ = q.store.UpsertSession(sessionID, linkID)
	trace, _ := json.Marshal(result.Events)
	_ = q.store.UpsertSdkTrace(taskID, string(trace))
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"session_id":  sessionID,
		"sdk_output":  result.FinalResponse,
		"status":      string(store.StatusCompleted),
		"finished_at": store.TimeNowFmt(),
		"finished_ts": store.TimeNowMs(),
	})

	PublishStream(sessionID, harness.Notification{Method: "end"})
	slog.Info("task completed", "task", taskID, "link", linkID)
}

func (q *Queue) ensureDSH() error {
	q.dsMu.Lock()
	defer q.dsMu.Unlock()

	if q.ds != nil && q.ds.IsAlive() {
		return nil
	}

	if q.ds != nil {
		slog.Warn("dsh process exited unexpectedly, restarting", "task_affected", "recovering")
		_ = q.ds.Close()
		q.ds = nil
	}

	ds := harness.NewClient(q.cfg.LLM)
	ds.SetExtraEnv(BuildBusinessEnv(q.store.GetAllConfigPlain()))
	if err := ds.Start(); err != nil {
		return err
	}
	q.ds = ds
	return nil
}

// DSH 返回共享的 dsh 客户端（懒启动），供会话续聊复用，
// 避免另起一个 harness.Client 去 resume 已被任务队列持有的 session。
func (q *Queue) DSH() (*harness.Client, error) {
	if err := q.ensureDSH(); err != nil {
		return nil, err
	}
	q.dsMu.Lock()
	ds := q.ds
	q.dsMu.Unlock()
	return ds, nil
}

func (q *Queue) failTask(taskID, errMsg string) {
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"status":        string(store.StatusFailed),
		"error_message": errMsg,
		"finished_at":   store.TimeNowFmt(),
		"finished_ts":   store.TimeNowMs(),
	})
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		for i := range b {
			b[i] = byte(time.Now().UnixNano() >> (i * 4))
		}
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	if v, ok := m[key].(map[string]interface{}); ok {
		return v
	}
	return nil
}

func getInt64(m map[string]interface{}, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ═══════════════════════════════════════════════════════════════════════════
// SSE 事件总线
// ═══════════════════════════════════════════════════════════════════════════

type sseSub struct {
	ch chan harness.Notification
}

var (
	sseMu   sync.Mutex
	sseSubs = make(map[string][]*sseSub)
)

func SubscribeStream(sessionID string) (<-chan harness.Notification, func()) {
	ch := make(chan harness.Notification, 512)
	sub := &sseSub{ch: ch}
	sseMu.Lock()
	sseSubs[sessionID] = append(sseSubs[sessionID], sub)
	sseMu.Unlock()
	return ch, func() {
		sseMu.Lock()
		subs := sseSubs[sessionID]
		for i, s := range subs {
			if s == sub {
				sseSubs[sessionID] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		sseMu.Unlock()
	}
}

func PublishStream(sessionID string, msg harness.Notification) {
	sseMu.Lock()
	subs := sseSubs[sessionID]
	sseMu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- msg:
		default:
		}
	}
}

func GetActiveSessions() []string {
	sseMu.Lock()
	defer sseMu.Unlock()
	out := make([]string, 0, len(sseSubs))
	for k := range sseSubs {
		out = append(out, k)
	}
	return out
}

func (q *Queue) GetRunningCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

func (q *Queue) GetPendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// CancelTask 取消一个任务（支持 pending、inflight 过渡期和 processing）。
func (q *Queue) CancelTask(taskID string) string {
	// 1) 处理中的任务：置取消标记并中断 SDK。
	// 不立即写 DB 为 cancelled——那样会在 ds.Run 尚未返回、session 尚未释放前就
	// 让 HasActiveByLinkid 放行同 linkid 的新任务，造成重叠。真正的 cancelled 落库
	// 在 processItem 里 ds.Run 返回后完成。
	if sessionID, ok := q.runningSessions.Load(taskID); ok {
		q.cancelRequested.Store(taskID, true)
		q.dsMu.Lock()
		ds := q.ds
		q.dsMu.Unlock()
		if ds != nil {
			if err := ds.CancelSession(sessionID.(string)); err != nil {
				slog.Warn("cancel session failed", "task", taskID, "session", sessionID, "err", err)
			}
		}
		return "processing"
	}

	// 2) inflight 过渡期：置取消标记，processItem 启动时会检查并直接标记 cancelled。
	if _, ok := q.inflight.Load(taskID); ok {
		q.inflight.Store(taskID, true)
		return "pending"
	}

	// 3) 排队中的任务：从 pending 数组移除
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, it := range q.pending {
		if it.TaskID == taskID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return "pending"
		}
	}
	return ""
}