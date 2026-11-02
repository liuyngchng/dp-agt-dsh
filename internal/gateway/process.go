package gateway

import (
	"encoding/json"
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
	callbackURL, _ := body["callback_url"].(string)
	if linkID == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 linkid", "", "", nil))
		return
	}
	if callbackURL == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 callback_url", "", "", nil))
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
		CallbackURL string `json:"callback_url"`
		CreatedAt string `json:"created_at"`
	}
	var views []taskView
	for _, t := range tasks {
		views = append(views, taskView{
			TaskID:      t.TaskID,
			LinkID:      t.LinkID,
			Path:        t.Path,
			Status:      string(t.Status),
			CallbackURL: t.CallbackURL,
			CreatedAt:   t.CreatedAt,
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
		"task_id":           task.TaskID,
		"linkid":            task.LinkID,
		"path":              task.Path,
		"status":            task.Status,
		"request_body":      mustParseJSON(task.RequestBody),
		"prompt":            task.Prompt,
		"sdk_output":        task.SdkOutput,
		"callback_body":     mustParseJSON(task.CallbackBody),
		"callback_response": mustParseJSON(task.CallbackResponse),
		"error_message":     task.ErrorMessage,
		"created_at":        task.CreatedAt,
		"finished_at":       task.FinishedAt,
	}))
}

// HandleStream SSE 流
func (h *TasksHandler) HandleStream(w http.ResponseWriter, r *http.Request) {
	// 委托给 AdminHandler 的 stream
	ah := NewAdminHandler(h.cfg, h.store, h.queue)
	ah.HandleSessionStream(w, r)
}

// HandleLog 任务审计日志
func (h *TasksHandler) HandleLog(w http.ResponseWriter, r *http.Request) {
	h.HandleTrace(w, r)
}

// HandleLinkLog 按 linkid 查审计日志
func (h *TasksHandler) HandleLinkLog(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/api/task/link/log/")
	task, _ := h.store.GetLatestTaskByLinkid(linkID)
	if task == nil {
		writeJSON(w, http.StatusNotFound, resp(1001, "无该 linkid 的任务记录", "", linkID, nil))
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
		"callback_body": mustParseJSON(task.CallbackBody),
		"error_message": task.ErrorMessage,
		"created_at":    task.CreatedAt,
		"finished_at":   task.FinishedAt,
	}))
}

var _ = json.Marshal
