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

// Queue 异步任务队列
type Queue struct {
	cfg   *config.Config
	store *store.Store
	ds    *harness.Client

	mu      sync.Mutex
	pending []*QueueItem
	running int

	linkMu     sync.Mutex
	linkChains map[string]chan struct{}
}

type QueueItem struct {
	TaskID string
	Path   string
	Iface  *store.AgentInterface
	Body   map[string]interface{}
}

func NewQueue(cfg *config.Config, s *store.Store) *Queue {
	q := &Queue{
		cfg:        cfg,
		store:      s,
		linkChains: make(map[string]chan struct{}),
	}
	q.recoverOrphanedTasks()
	return q
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
			// pending 任务重新入队
			if iface, err := q.store.GetInterfaceByPath(task.Path); err == nil && iface != nil {
				var body map[string]interface{}
				if json.Unmarshal([]byte(task.RequestBody), &body) == nil {
					q.pending = append(q.pending, &QueueItem{
						TaskID: task.TaskID,
						Path:   task.Path,
						Iface:  iface,
						Body:   body,
					})
					resumed++
				}
			}
		case store.StatusProcessing:
			// processing 任务（进程中断）标记失败
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

	// 任务提交即创建/复用 session，保证「会话管理」「会话记录」对 pending 任务也立即可见。
	// session_id 采用 "s-"+taskID，后续处理阶段如复用旧 session 会再更新。
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

func (q *Queue) pump() {
	maxConc := q.cfg.Sys.MaxConcurrentTasks

	q.mu.Lock()
	for q.running < maxConc && len(q.pending) > 0 {
		item := q.pending[0]
		q.pending = q.pending[1:]
		q.running++
		q.mu.Unlock()

		linkID, _ := item.Body["linkid"].(string)
		q.linkMu.Lock()
		prev, exists := q.linkChains[linkID]
		next := make(chan struct{})
		q.linkChains[linkID] = next
		q.linkMu.Unlock()

		go func(it *QueueItem, prevCh chan struct{}, nextCh chan struct{}, lid string) {
			if exists && prevCh != nil {
				<-prevCh
			}
			q.processItem(it)
			close(nextCh)

			q.linkMu.Lock()
			if cur, ok := q.linkChains[lid]; ok && cur == nextCh {
				delete(q.linkChains, lid)
			}
			q.linkMu.Unlock()

			q.mu.Lock()
			q.running--
			q.mu.Unlock()
			q.pump()
		}(item, prev, next, linkID)

		q.mu.Lock()
	}
	q.mu.Unlock()
}

func (q *Queue) processItem(item *QueueItem) {
	taskID := item.TaskID
	linkID, _ := item.Body["linkid"].(string)

	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"status": string(store.StatusProcessing)})
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

	// 立即写入 session 和 task_logs 的 session_id，保证任务提交后「会话管理」立即可见、
	// 且后续无论 dsh 启动/执行是否失败，「会话记录」都可点击查看。
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

	// mock 模式：检查是否显式 mock（callback_url == "mock" 或 body 有 is_mock）
	isMock, _ := item.Body["callback_url"].(string)
	isMockFlag, _ := item.Body["is_mock"].(bool)
	if isMock == "mock" || isMockFlag {
		// mock 模式也记录一个 session，保证前端「会话记录」「会话管理」可见。
		_ = q.store.UpsertSession(sessionID, linkID)
		slog.Info("mock", "task", taskID, "path", item.Path, "prompt_rendered_bytes", len(rendered))
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":      string(store.StatusCompleted),
			"session_id":  sessionID,
			"sdk_output":  `{"note": "mock mode - dsh not invoked"}`,
			"sdk_trace":   "[]",
			"finished_at": store.TimeNowFmt(),
			"finished_ts": store.TimeNowMs(),
		})
		return
	}

	// 启动 dsh
	if err := q.ensureDSH(); err != nil {
		q.failTask(taskID, "启动 dsh 运行时失败: "+err.Error())
		return
	}

	// resume 或新建
	if resumeID != "" {
		if err := q.ds.ResumeSession(resumeID); err != nil {
			slog.Warn("resume failed, starting fresh", "session", resumeID, "err", err)
			sessionID = "s-" + taskID
			_ = q.store.UpsertSession(sessionID, linkID)
			_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"session_id": sessionID})
		}
	}

	// 切换 session 的工作目录到当前任务的 workdir（失败则终止任务，避免在兜底目录下误执行）
	if err := q.ds.SetWorkingDirectory(sessionID, workDir); err != nil {
		q.failTask(taskID, "设置工作目录失败: "+err.Error())
		return
	}

	var allEvents []harness.SessionEvent

	result, err := q.ds.Run(sessionID, rendered, func(n harness.Notification) {
		PublishStream(sessionID, n)

		// 从通知中提取 session event
		if n.Method == "session.event" {
			if ev, ok := n.Payload["event"].(map[string]interface{}); ok {
				allEvents = append(allEvents, harness.SessionEvent{
					Type: getStr(ev, "type"),
					Data: getMap(ev, "data"), // 内层 data，不含外层外壳
					Time: getInt64(ev, "time"),
				})
			}
		}
	})
	if err != nil {
		trace, _ := json.Marshal(allEvents)
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":        string(store.StatusFailed),
			"session_id":    sessionID,
			"sdk_output":    "",
			"sdk_trace":     string(trace),
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
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"session_id":  sessionID,
		"sdk_output":  result.FinalResponse,
		"sdk_trace":   string(trace),
		"status":      string(store.StatusCompleted),
		"finished_at": store.TimeNowFmt(),
		"finished_ts": store.TimeNowMs(),
	})

	PublishStream(sessionID, harness.Notification{Method: "end"})
	slog.Info("task completed", "task", taskID, "link", linkID)
}

func (q *Queue) ensureDSH() error {
	if q.ds != nil {
		return nil
	}
	ds := harness.NewClient(q.cfg.LLM)
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
	return q.ds, nil
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
		// fallback
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

// ── SSE 事件总线 ──

type sseSub struct {
	ch chan harness.Notification
}

var (
	sseMu   sync.Mutex
	sseSubs = make(map[string][]*sseSub)
)

// SubscribeStream 订阅 session 的流式事件
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

// PublishStream 向 session 的所有订阅者推送事件
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

// GetActiveSessions 返回所有有订阅者的 session ID
func GetActiveSessions() []string {
	sseMu.Lock()
	defer sseMu.Unlock()
	out := make([]string, 0, len(sseSubs))
	for k := range sseSubs {
		out = append(out, k)
	}
	return out
}

// GetRunningCount 返回队列中 running 的数量
func (q *Queue) GetRunningCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.running
}

// GetPendingCount 返回队列中等待的数量
func (q *Queue) GetPendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
