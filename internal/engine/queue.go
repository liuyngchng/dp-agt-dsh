package engine

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
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

	linkMu    sync.Mutex
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
		log.Printf("[queue] recovered: %d resumed, %d killed", resumed, killed)
		if resumed > 0 {
			go q.pump()
		}
	}
}

func (q *Queue) Enqueue(iface *store.AgentInterface, body map[string]interface{}) *QueueItem {
	taskID := newUUID()
	linkID, _ := body["linkid"].(string)
	callbackURL, _ := body["callback_url"].(string)
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
		CallbackURL:     callbackURL,
		CreatedAt:       store.TimeNowFmt(),
		CreatedTs:       store.TimeNowMs(),
	}
	_ = q.store.CreateTaskLog(t)

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
	callbackURL, _ := item.Body["callback_url"].(string)

	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"status": string(store.StatusProcessing)})
	log.Printf("[queue] processing: task=%s link=%s path=%s", taskID, linkID, item.Path)

	// 读提示词模板
	template := LoadPromptFromDisk(item.Path)
	if template == "" {
		q.failTask(taskID, "提示词文件不存在: "+item.Path, callbackURL, linkID)
		return
	}

	// 渲染
	rendered := RenderPrompt(template, item.Body)
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{"prompt": rendered})

	// 查 session
	existingSession, _ := q.store.GetSessionByLinkid(linkID)
	var resumeID string
	if existingSession != nil {
		resumeID = existingSession.SessionID
	}

	// 创建/复用 workdir
	workDir := filepath.Join(q.cfg.Sys.DataDir, "tasks", sanitizeDirName(linkID))
	_, err := BuildTaskDir(workDir, q.store, q.cfg.RawConfig())
	if err != nil {
		q.failTask(taskID, "创建工作目录失败: "+err.Error(), callbackURL, linkID)
		return
	}

	// mock 模式：不启动 dsh，直接返回一个占位结果
	if callbackURL == "mock" {
		log.Printf("[queue] mock: task=%s path=%s prompt_rendered=%d bytes", taskID, item.Path, len(rendered))
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status":      string(store.StatusCompleted),
			"sdk_output":  `{"note": "mock mode - dsh not invoked"}`,
			"sdk_trace":   "[]",
			"finished_at": store.TimeNowFmt(),
			"finished_ts": store.TimeNowMs(),
		})
		return
	}

	// 启动 dsh
	if err := q.ensureDSH(); err != nil {
		q.failTask(taskID, "启动 dsh 运行时失败: "+err.Error(), callbackURL, linkID)
		return
	}

	sessionID := resumeID
	if sessionID == "" {
		sessionID = "s-" + taskID
	}

	// resume 或新建
	if resumeID != "" {
		if err := q.ds.ResumeSession(resumeID); err != nil {
			log.Printf("[queue] resume failed for %s, starting fresh: %v", resumeID, err)
			sessionID = "s-" + taskID
		}
	}

	var allEvents []harness.SessionEvent

	result, err := q.ds.Run(sessionID, rendered, func(n harness.Notification) {
		PublishStream(sessionID, n)

		// 从通知中提取 session event
		if n.Method == "session.event" {
			if ev, ok := n.Payload["event"].(map[string]interface{}); ok {
				allEvents = append(allEvents, harness.SessionEvent{
					Type: getStr(ev, "type"),
					Data: ev,
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
		q.callback(callbackURL, linkID, taskID, 5000, fmt.Sprintf("dsh 处理失败: %v", err), nil)
		return
	}

	// 成功
	_ = q.store.UpsertSession(sessionID, linkID)
	trace, _ := json.Marshal(result.Events)
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"session_id": sessionID,
		"sdk_output": result.FinalResponse,
		"sdk_trace":  string(trace),
	})

	parsed := extractJSON(result.FinalResponse)
	PublishStream(sessionID, harness.Notification{Method: "end"})
	q.callback(callbackURL, linkID, taskID, 0, "success", parsed)
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

func (q *Queue) failTask(taskID, errMsg, callbackURL, linkID string) {
	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"status":        string(store.StatusFailed),
		"error_message": errMsg,
		"finished_at":   store.TimeNowFmt(),
		"finished_ts":   store.TimeNowMs(),
	})
	q.callback(callbackURL, linkID, taskID, 5000, errMsg, nil)
}

func (q *Queue) callback(url, linkID, taskID string, code int, message string, data interface{}) {
	if url == "" || url == "mock" {
		// mock 模式：直接标记完成
		status := store.StatusCompleted
		if code != 0 {
			status = store.StatusFailed
		}
		_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
			"status": string(status),
		})
		log.Printf("[queue] mock complete: task=%s status=%s code=%d", taskID, status, code)
		return
	}

	payload := map[string]interface{}{
		"code":      code,
		"timestamp": store.TimeNowFmt(),
		"message":   message,
		"task_id":   taskID,
		"linkid":    linkID,
		"data":      data,
	}
	if data == nil {
		payload["data"] = nil
	}

	cbPayload, _ := json.Marshal(payload)

	// 回调 + 重试
	maxRetries := q.cfg.Sys.CallbackRetries
	baseInterval := time.Duration(q.cfg.Sys.CallbackRetryIntervalSec) * time.Second

	success := false
	var lastResp string
	var lastErr string

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := baseInterval * time.Duration(1<<(attempt-1))
			time.Sleep(delay)
		}

		resp, err := postOnce(url, string(cbPayload), taskID, q.store)
		if err == nil {
			success = true
			lastResp = resp
			break
		}
		lastErr = err.Error()
		lastResp = resp
	}

	finalStatus := store.StatusCompleted
	if !success {
		finalStatus = store.StatusCallbackFail
	}

	_ = q.store.UpdateTaskResult(taskID, map[string]interface{}{
		"status":            string(finalStatus),
		"callback_body":     string(cbPayload),
		"callback_response": lastResp,
		"error_message":     lastErr,
		"finished_at":       store.TimeNowFmt(),
		"finished_ts":       store.TimeNowMs(),
	})
}

// postOnce 发送单次 HTTP POST 回调
func postOnce(url, payload, taskID string, s *store.Store) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	token, _ := s.GetConfigPlain("doc.file_url_token")
	if token == "" {
		token, _ = s.GetConfigPlain("sys.file_url_token")
	}

	req, err := http.NewRequest("POST", url, bytes.NewBufferString(payload))
	if err != nil {
		return "", fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	bodyStr := string(body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return bodyStr, fmt.Errorf("http %d: %s", resp.StatusCode, bodyStr)
	}
	return bodyStr, nil
}

// DeliverCallback 公开的回调投递接口（给 tasks 路由用）
func DeliverCallback(url string, payload map[string]interface{}, cfg *config.Config, taskID string) {
	payloadJSON, _ := json.Marshal(payload)
	maxRetries := cfg.Sys.CallbackRetries
	baseInterval := time.Duration(cfg.Sys.CallbackRetryIntervalSec) * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(baseInterval * time.Duration(1<<(attempt-1)))
		}
		_, err := postOnce(url, string(payloadJSON), taskID, nil)
		if err == nil {
			return
		}
	}
}

// ── helpers ──

func sanitizeDirName(s string) string {
	r := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	return r.Replace(s)
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

func extractJSON(text string) interface{} {
	re := regexp.MustCompile("(?s)```json\\s*([\\s\\S]*?)```")
	if m := re.FindStringSubmatch(text); m != nil {
		var result interface{}
		if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &result); err == nil {
			return result
		}
	}
	var result interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &result); err == nil {
		return result
	}
	return map[string]string{"raw_output": text}
}

func getStr(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// ── SSE 事件总线 ──

type sseSub struct {
	ch chan harness.Notification
}

var (
	sseMu  sync.Mutex
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