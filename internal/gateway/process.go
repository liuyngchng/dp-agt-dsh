package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/engine"
	"dp-agt-dsh/internal/store"
)

type ProcessHandler struct {
	cfg   *config.Config
	store *store.Store
	queue *engine.Queue
}

func NewProcessHandler(cfg *config.Config, s *store.Store, q *engine.Queue) *ProcessHandler {
	return &ProcessHandler{cfg: cfg, store: s, queue: q}
}

func (h *ProcessHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Handle(w, r)
}

// Handle 处理 POST /api/v1/:path（通配路由）
func (h *ProcessHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// 从 URL 去掉 /api/v1/ 前缀
	interfacePath := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	interfacePath = strings.Trim(interfacePath, "/")

	if interfacePath == "" {
		writeJSON(w, http.StatusNotFound, resp(1001, "缺少访问路径", "", "", nil))
		return
	}

	// 解析 body
	var body map[string]interface{}
	if err := readJSON(r, &body); err != nil || body == nil {
		writeJSON(w, http.StatusBadRequest, resp(1001, "请求体不能为空", "", "", nil))
		return
	}

	// 校验接口
	iface, err := h.store.GetInterfaceByPath(interfacePath)
	if err != nil || iface == nil {
		writeJSON(w, http.StatusNotFound, resp(1001, "接口不存在: "+interfacePath, "", "", nil))
		return
	}

	// 校验必填字段
	linkID, _ := body["linkid"].(string)
	if linkID == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 linkid", "", "", nil))
		return
	}

	// 同一 linkid 有未完成任务时拒绝
	if h.store.HasActiveByLinkid(linkID) {
		writeJSON(w, http.StatusConflict, resp(2003, "该公文尚有任务正在处理中，请等待完成后再次提交", "", linkID, nil))
		return
	}

	// 队列上限
	maxPending := h.cfg.Sys.MaxPendingTasks
	activeCount := h.queue.GetRunningCount() + h.queue.GetPendingCount()
	if activeCount >= maxPending {
		msg := "任务队列已满，当前处理中任务数 " + itoa(activeCount) + "/" + itoa(maxPending) + "，请稍后重试"
		writeJSON(w, http.StatusServiceUnavailable, resp(2002, msg, "", linkID, nil))
		return
	}

	// 入队
	item := h.queue.Enqueue(iface, body)
	log.Printf("[process] enqueued: task=%s link=%s path=%s", item.TaskID, linkID, interfacePath)

	writeJSON(w, http.StatusOK, resp(0, "success", item.TaskID, linkID, nil))
}

// ── 任务查询路由 ──

type TasksHandler struct {
	cfg   *config.Config
	store *store.Store
	queue *engine.Queue
}

func NewTasksHandler(cfg *config.Config, s *store.Store, q *engine.Queue) *TasksHandler {
	return &TasksHandler{cfg: cfg, store: s, queue: q}
}

func (h *TasksHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	method := r.Method

	switch {
	case method == "GET" && path == "/api/tasks":
		h.HandleList(w, r)
	case method == "DELETE" && strings.HasPrefix(path, "/api/tasks/"):
		h.HandleCancel(w, r)
	case method == "GET" && strings.HasPrefix(path, "/api/task/trace"):
		h.HandleTrace(w, r)
	case method == "GET" && strings.HasPrefix(path, "/api/task/stream"):
		h.HandleStream(w, r)
	case method == "GET" && strings.HasPrefix(path, "/api/task/log/"):
		h.HandleLog(w, r)
	case method == "GET" && strings.HasPrefix(path, "/api/task/link/log/"):
		h.HandleLinkLog(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *TasksHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.store.ListActiveTasks()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, resp(5000, err.Error(), "", "", nil))
		return
	}

	type taskView struct {
		TaskID    string `json:"task_id"`
		LinkID    string `json:"linkid"`
		Path      string `json:"interface_path"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	var views []taskView
	for _, t := range tasks {
		views = append(views, taskView{
			TaskID:    t.TaskID,
			LinkID:    t.LinkID,
			Path:      t.Path,
			Status:    string(t.Status),
			CreatedAt: t.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, resp(0, "success", "", "", map[string]interface{}{
		"total": len(views),
		"tasks": views,
	}))
}

func (h *TasksHandler) HandleCancel(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	if taskID == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 task_id", "", "", nil))
		return
	}
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, http.StatusNotFound, resp(1001, "任务不存在", "", "", nil))
		return
	}
	if task.Status != store.StatusPending {
		writeJSON(w, http.StatusBadRequest, resp(1001, "只有 pending 状态的任务可以取消", "", task.LinkID, nil))
		return
	}
	_ = h.store.UpdateTaskResult(taskID, map[string]interface{}{"status": string(store.StatusCancelled)})
	writeJSON(w, http.StatusOK, resp(0, "任务已取消", taskID, task.LinkID, nil))
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

// HandleTrace 任务追踪
func (h *TasksHandler) HandleTrace(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("taskid")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, http.StatusNotFound, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, http.StatusOK, resp(0, "success", taskID, task.LinkID, map[string]interface{}{
		"task_id":       task.TaskID,
		"linkid":        task.LinkID,
		"path":          task.Path,
		"status":        task.Status,
		"request_body":  mustParseJSON(task.RequestBody),
		"prompt":        task.Prompt,
		"sdk_output":    task.SdkOutput,
		"sdk_trace":     task.SdkTrace,
		"error_message": task.ErrorMessage,
		"created_at":    task.CreatedAt,
		"finished_at":   task.FinishedAt,
	}))
}

// HandleStream SSE 流
func (h *TasksHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	// 委托给 AdminHandler 的 stream
	ah := NewAdminHandler(h.cfg, h.store, h.queue)
	ah.HandleSessionStream(w, r)
}

// HandleLog 任务审计日志（支持 format=html 返回完整 HTML 页面）
func (h *TasksHandler) HandleLog(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/api/task/log/")
	format := r.URL.Query().Get("format")

	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		if format == "html" {
			writeHTML404(w, "任务不存在")
			return
		}
		writeJSON(w, http.StatusNotFound, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}

	if format == "html" {
		renderTaskLogHTML(w, task)
		return
	}

	writeJSON(w, http.StatusOK, resp(0, "success", taskID, task.LinkID, map[string]interface{}{
		"task_id":       task.TaskID,
		"linkid":        task.LinkID,
		"path":          task.Path,
		"status":        task.Status,
		"request_body":  mustParseJSON(task.RequestBody),
		"prompt":        task.Prompt,
		"sdk_output":    task.SdkOutput,
		"sdk_trace":     task.SdkTrace,
		"error_message": task.ErrorMessage,
		"created_at":    task.CreatedAt,
		"finished_at":   task.FinishedAt,
	}))
}

// HandleLinkLog 按 linkid 查审计日志
func (h *TasksHandler) HandleLinkLog(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/api/task/link/log/")
	format := r.URL.Query().Get("format")

	task, _ := h.store.GetLatestTaskByLinkid(linkID)
	if task == nil {
		if format == "html" {
			writeHTML404(w, "无该 linkid 的任务记录")
			return
		}
		writeJSON(w, http.StatusNotFound, resp(1001, "无该 linkid 的任务记录", "", linkID, nil))
		return
	}

	if format == "html" {
		renderTaskLogHTML(w, task)
		return
	}

	writeJSON(w, http.StatusOK, resp(0, "success", task.TaskID, linkID, map[string]interface{}{
		"task_id":       task.TaskID,
		"linkid":        task.LinkID,
		"path":          task.Path,
		"status":        task.Status,
		"request_body":  mustParseJSON(task.RequestBody),
		"prompt":        task.Prompt,
		"sdk_output":    task.SdkOutput,
		"sdk_trace":     task.SdkTrace,
		"error_message": task.ErrorMessage,
		"created_at":    task.CreatedAt,
		"finished_at":   task.FinishedAt,
	}))
}

// ── HTML 时间线渲染 ──

const timelineCSS = `*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#f5f6fa;color:#333;padding:40px 20px}
.wrap{max-width:960px;margin:0 auto;background:#fff;border-radius:12px;box-shadow:0 2px 16px rgba(0,0,0,.08);overflow:hidden}
.page-head{padding:28px 36px;background:linear-gradient(to right,#4b6cb7,#182848);color:#fff}
.page-head h1{font-size:1.4rem;display:flex;align-items:center;gap:10px}
.page-head .meta{margin-top:10px;font-size:.85rem;opacity:.85;display:flex;flex-direction:column;gap:6px}
.meta-line{display:flex;flex-wrap:wrap;align-items:center;gap:6px}
.meta-label{font-weight:600;opacity:.9}
.tl{padding:28px 36px}
.item{display:flex;gap:14px;padding:14px 0;border-left:2px solid #e0e0e0;margin-left:8px;position:relative}
.item:last-child{border-left-color:transparent}
.dot{width:16px;height:16px;border-radius:50%;background:#4b6cb7;flex-shrink:0;margin-left:-9px;margin-top:2px;position:relative;z-index:1}
.item.error .dot{background:#e53e3e}
.item.done .dot{background:#2e7d32}
.body{flex:1;min-width:0}
.ihead{display:flex;flex-wrap:wrap;align-items:center;gap:10px;font-size:.92rem}
.time{font-family:monospace;font-size:.78rem;color:#888;white-space:nowrap}
.detail{background:#f7f8fa;border-radius:6px;padding:12px 16px;margin-top:8px;font-size:.82rem;line-height:1.6;white-space:pre-wrap;word-break:break-word;max-height:400px;overflow:auto;border:1px solid #edf2f7}
.foot{padding:16px 36px;border-top:1px solid #edf2f7;font-size:.78rem;color:#999;text-align:center}`

func renderTaskLogHTML(w http.ResponseWriter, task *store.TaskLog) {
	events := buildTimelineEvents(task)
	linkid := task.LinkID

	title := "-"
	if task.RequestBody != "" {
		var obj map[string]interface{}
		if json.Unmarshal([]byte(task.RequestBody), &obj) == nil {
			if fd, ok := obj["form_data"].(map[string]interface{}); ok {
				if t, _ := fd["title"].(string); t != "" {
					title = t
				}
			}
			if title == "-" {
				if t, _ := obj["title"].(string); t != "" {
					title = t
				}
			}
		}
	}
	startStr := task.CreatedAt
	endStr := task.FinishedAt
	if endStr == "" {
		endStr = "处理中"
	}

	var itemsHTML string
	for i, ev := range events {
		timeStr := ev.Time
		detailBlock := ""
		if ev.Detail != "" {
			detailBlock = fmt.Sprintf(`<pre class="detail">%s</pre>`, escHTML(ev.Detail))
		}
		itemsHTML += fmt.Sprintf(`<div class="item %s">
		<div class="dot"></div>
		<div class="body">
			<div class="ihead"><span class="time">%s</span><strong>%d. %s</strong></div>
			%s
		</div>
	</div>`, ev.Type, escHTML(timeStr), i+1, escHTML(ev.Title), detailBlock)
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>任务执行时间线 — %s</title>
<style>%s</style>
</head>
<body>
<div class="wrap">
  <div class="page-head">
    <h1>&#129302; 任务执行时间线</h1>
    <div class="meta">
      <div class="meta-line"><span class="meta-label">标题:</span> %s</div>
      <div class="meta-line"><span class="meta-label">执行周期:</span> %s ~ %s</div>
      <div class="meta-line"><span class="meta-label">Task ID:</span> %s <span class="meta-label">Link ID:</span> %s</div>
    </div>
  </div>
  <div class="tl">%s</div>
  <div class="foot">Powered by AI 数字员工</div>
</div>
</body>
</html>`, escHTML(linkid), timelineCSS, escHTML(title), escHTML(startStr), escHTML(endStr),
		escHTML(task.TaskID), escHTML(linkid), itemsHTML)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(html))
}

type timelineEvent struct {
	Time   string
	Type   string
	Title  string
	Detail string
}

func buildTimelineEvents(task *store.TaskLog) []timelineEvent {
	var events []timelineEvent

	// 1. 任务创建
	events = append(events, timelineEvent{
		Time: task.CreatedAt, Type: "create", Title: "任务创建",
		Detail: "访问路径: " + task.Path,
	})

	// 2. 收到请求数据
	if task.RequestBody != "" {
		events = append(events, timelineEvent{
			Time: task.CreatedAt, Type: "input", Title: "收到请求数据",
			Detail: jsonFmt(task.RequestBody),
		})
	}

	// 3. 返回同步响应
	if task.RequestResponse != "" {
		events = append(events, timelineEvent{
			Time: task.CreatedAt, Type: "output", Title: "返回同步响应",
			Detail: jsonFmt(task.RequestResponse),
		})
	}

	// 4. 渲染提示词
	if task.Prompt != "" {
		events = append(events, timelineEvent{
			Time: task.CreatedAt, Type: "prompt", Title: "渲染提示词",
			Detail: task.Prompt,
		})
	}

	// 5. Agent 最终输出
	if task.SdkOutput != "" {
		events = append(events, timelineEvent{
			Time: task.FinishedAt, Type: "output", Title: "Agent 输出结果",
			Detail: task.SdkOutput,
		})
	}

	// 6. 完成 / 失败
	finalTitle := statusLabel(string(task.Status))
	finalType := "info"
	if task.Status == store.StatusCompleted {
		finalType = "done"
	} else if task.Status == store.StatusFailed {
		finalType = "error"
	}
	events = append(events, timelineEvent{
		Time: task.FinishedAt, Type: finalType, Title: finalTitle,
		Detail: task.ErrorMessage,
	})

	return events
}

func statusLabel(s string) string {
	switch s {
	case "pending":
		return "排队中"
	case "processing":
		return "处理中"
	case "failed":
		return "失败"
	case "completed":
		return "已完成"
	case "cancelled":
		return "已取消"
	case "callback_fail":
		return "回调失败"
	default:
		return s
	}
}

func jsonFmt(v string) string {
	if v == "" {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(v), &parsed); err != nil {
		return v
	}
	b, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return v
	}
	return string(b)
}

func escHTML(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;")
	return replacer.Replace(s)
}

func writeHTML404(w http.ResponseWriter, message string) {
	html := fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>%s</title>
<style>%s</style></head>
<body><div class="wrap"><div class="page-head"><h1>%s</h1></div></div></body></html>`,
		message, timelineCSS, message)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(html))
}

var _ = json.Marshal
