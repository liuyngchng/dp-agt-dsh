package gateway

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/engine"
	"dp-agt-dsh/internal/harness"
	"dp-agt-dsh/internal/store"
)

type AdminHandler struct {
	cfg   *config.Config
	store *store.Store
	queue *engine.Queue
}

func NewAdminHandler(cfg *config.Config, s *store.Store, q *engine.Queue) *AdminHandler {
	return &AdminHandler{cfg: cfg, store: s, queue: q}
}

// ── Mock 数据 ──

func (h *AdminHandler) HandleMock(w http.ResponseWriter, r *http.Request) {
	mockFile := filepath.Join(h.cfg.Sys.DataDir, "mock.json")
	data, err := os.ReadFile(mockFile)
	if err != nil {
		writeJSON(w, http.StatusNotFound, resp(1001, "mock.json 不存在", "", "", nil))
		return
	}
	var parsed interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		writeJSON(w, http.StatusBadRequest, resp(1001, "mock.json 解析失败: "+err.Error(), "", "", nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": parsed})
}

// ── 导航 ──

func (h *AdminHandler) HandleNav(w http.ResponseWriter, r *http.Request) {
	_, role := currentUser(r)
	isAdmin := role == 1
	type tab struct {
		ID    string `json:"id"`
		Icon  string `json:"icon"`
		Label string `json:"label"`
	}
	tabs := []tab{}
	if isAdmin {
		tabs = append(tabs,
			tab{"interfaces", "fa-brain", "模板管理"},
			tab{"memory", "fa-database", "记忆管理"},
			tab{"simulate", "fa-flask", "操作模拟"},
			tab{"tasks", "fa-list-alt", "任务监控"},
		)
	}
	tabs = append(tabs, tab{"audit", "fa-search", "任务审查"})
	if isAdmin {
		tabs = append(tabs,
			tab{"sessions", "fa-comments", "会话管理"},
			tab{"sys", "fa-sliders-h", "系统设置"},
			tab{"users", "fa-users", "用户管理"},
		)
	}
	defaultTab := "audit"
	if isAdmin {
		defaultTab = "interfaces"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"role": role, "default_tab": defaultTab, "tabs": tabs,
	}})
}

// ── 服务器信息 ──

func (h *AdminHandler) HandleServerInfo(w http.ResponseWriter, r *http.Request) {
	host := h.cfg.Server.SelfIP
	if host == "" {
		host = "127.0.0.1"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"host": host,
		"port": h.cfg.Server.SelfPort,
	}})
}

// ── 接口管理 ──

func (h *AdminHandler) HandleListInterfaces(w http.ResponseWriter, r *http.Request) {
	promptFiles, _ := engine.ScanPromptsDir(h.cfg.Sys.DataDir)
	// 同步到 DB
	dbIfaces, _ := h.store.ListInterfaces()
	_ = dbIfaces

	var out []map[string]interface{}
	for _, pf := range promptFiles {
		out = append(out, map[string]interface{}{
			"path":     pf.Path,
			"name":     pf.Name,
			"filename": pf.Filename,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": out})
}

// ── 模拟操作 ──

func (h *AdminHandler) HandleSimulate(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, resp(1001, "请求体不能为空", "", "", nil))
		return
	}

	path, _ := body["interface_path"].(string)
	if path == "" {
		path, _ = body["path"].(string) // 兼容旧字段
	}
	payload, _ := body["request_body"].(map[string]interface{})
	if payload == nil {
		payload, _ = body["payload"].(map[string]interface{}) // 兼容旧字段
	}
	if path == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 interface_path", "", "", nil))
		return
	}
	if payload == nil {
		payload = make(map[string]interface{})
	}
	if _, ok := payload["linkid"]; !ok {
		payload["linkid"] = "mock-" + newToken()[:8]
	}
	if _, ok := payload["callback_url"]; !ok {
		payload["callback_url"] = "mock"
	}

	// 走与 /api/v1/:path 相同的提交逻辑
	done := make(chan map[string]interface{}, 1)
	go func() {
		w2 := &mockWriter{status: 200}
		req, _ := http.NewRequest("POST", "/api/v1/"+path, nil)
		ph := NewProcessHandler(h.cfg, h.store, h.queue)
		ph.Handle(w2, withBody(req, payload))
		done <- w2.body
	}()

	result := <-done
	var statusCode int
	// 根据 code 决定 HTTP 状态
	if code, ok := result["code"].(float64); ok && code == 0 {
		statusCode = 200
	} else if code, ok := result["code"].(float64); ok && code == 1001 {
		statusCode = 404
	} else {
		statusCode = 400
	}
	writeJSON(w, statusCode, result)
}

// ── 任务监控 ──

func (h *AdminHandler) HandleAdminTasks(w http.ResponseWriter, r *http.Request) {
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	pageSize := atoiDefault(r.URL.Query().Get("page_size"), 10)
	if pageSize > 100 {
		pageSize = 100
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	taskID := r.URL.Query().Get("taskid")
	linkID := r.URL.Query().Get("linkid")
	tasks, total, err := h.store.ListTasksPaged(taskID, linkID, offset, pageSize)
	if err != nil {
		writeJSON(w, 500, resp(5000, err.Error(), "", "", nil))
		return
	}

	type taskView struct {
		TaskID        string `json:"task_id"`
		LinkID        string `json:"linkid"`
		SessionID     string `json:"session_id"`
		SessionStatus string `json:"session_status"`
		Title         string `json:"title"`
		Path          string `json:"path"`
		Status        string `json:"status"`
		ErrorMsg      string `json:"error_message"`
		CreatedAt     string `json:"created_at"`
		FinishedAt    string `json:"finished_at"`
	}
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		sessionStatus := ""
		if t.SessionID != "" {
			if sess, _ := h.store.GetSession(t.SessionID); sess != nil {
				sessionStatus = sess.Status
			} else {
				sessionStatus = "closed"
			}
		}
		views = append(views, taskView{
			TaskID:        t.TaskID,
			LinkID:        t.LinkID,
			SessionID:     t.SessionID,
			SessionStatus: sessionStatus,
			Title:         extractTitle(t.RequestBody),
			Path:          t.Path,
			Status:        string(t.Status),
			ErrorMsg:      t.ErrorMessage,
			CreatedAt:     t.CreatedAt,
			FinishedAt:    t.FinishedAt,
		})
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"items": views, "total": total, "page": page, "page_size": pageSize,
	}))
}

func (h *AdminHandler) HandleAdminTaskDetail(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/admin/tasks/")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", taskID, task.LinkID, map[string]interface{}{
		"task_id":          task.TaskID,
		"linkid":           task.LinkID,
		"session_id":       task.SessionID,
		"title":            extractTitle(task.RequestBody),
		"path":             task.Path,
		"status":           string(task.Status),
		"request_body":     mustParseJSON(task.RequestBody),
		"request_response": mustParseJSON(task.RequestResponse),
		"prompt":           task.Prompt,
		"sdk_output":       task.SdkOutput,
		"sdk_trace":        task.SdkTrace,
		"error_message":    task.ErrorMessage,
		"created_at":       task.CreatedAt,
		"finished_at":      task.FinishedAt,
	}))
}

// ── SSE 流 ──

func (h *AdminHandler) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		// 也支持 /admin/sessions/:session_id/stream 形式
		path := strings.TrimPrefix(r.URL.Path, "/admin/sessions/")
		path = strings.TrimSuffix(path, "/stream")
		path = strings.TrimSuffix(path, "/")
		if path != "" && path != r.URL.Path {
			sessionID = path
		}
	}
	if sessionID == "" {
		writeJSON(w, 400, resp(1001, "缺少 session_id", "", "", nil))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, resp(5000, "SSE not supported", "", "", nil))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// ── 1. 连接时先发送 snapshot：从 DB 拉取已有历史消息 ──
	sendSSEEvent(w, flusher, h.getSessionSnapshot(sessionID))

	// ── 2. 然后订阅实时事件，滚动推送 ──
	ch, cancel := engine.SubscribeStream(sessionID)
	defer cancel()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// 将原始通知转换为前端兼容格式
			for _, m := range notificationToFrontend(msg) {
				sendSSEEvent(w, flusher, m)
			}
			if msg.Method == "end" {
				return
			}
		}
	}
}

// getSessionSnapshot 从 sdk_trace 读取已有消息，组装 snapshot
func (h *AdminHandler) getSessionSnapshot(sessionID string) map[string]interface{} {
	rawEvents := h.loadSessionEvents(sessionID)
	messages := convertEventsToFrontend(rawEvents)
	return map[string]interface{}{
		"type":     "snapshot",
		"messages": messages,
	}
}

// loadSessionEvents 从 DB 反序列化 session 对应的全部 sdk_trace。
//
// DSH SDK 没有类似 Claude Agent SDK 的 getSessionMessages() 可以从磁盘会话文件
// 直接读全量历史；每条消息只能从 task_logs.sdk_trace 拿到，且 sdk_trace 只在任务
// 结束时才落库。同一 session 被 resume 复用后，历史被拆到多条 task 的 sdk_trace 里，
// 单取最新一条会导致「重新打开只看到实时推送、看不到历史」。因此这里合并该 session
// 名下所有 task 的 sdk_trace，按 task 创建时间升序拼接，并按消息 id 去重（resume
// 重放可能带来重复的 user/assistant 消息）。
func (h *AdminHandler) loadSessionEvents(sessionID string) []map[string]interface{} {
	tasks, err := h.store.ListTasksBySessionID(sessionID)
	if err != nil || len(tasks) == 0 {
		// 兜底：老数据可能 session_id 未回填，退回按 task_id / 最新 task 查
		taskID := strings.TrimPrefix(sessionID, "s-")
		task, _ := h.store.GetTaskLog(taskID)
		if task == nil {
			task, _ = h.store.GetTaskBySessionID(sessionID)
		}
		if task == nil {
			return nil
		}
		tasks = []store.TaskLog{*task}
	}

	var events []map[string]interface{}
	seen := make(map[string]bool)
	for _, t := range tasks {
		if t.SdkTrace == "" {
			continue
		}
		var batch []map[string]interface{}
		if err := json.Unmarshal([]byte(t.SdkTrace), &batch); err != nil {
			continue
		}
		for _, ev := range batch {
			// 去重：优先用消息 id（data.message.id 或 data.id），否则用 type+time 兜底。
			key := eventDedupKey(ev)
			if key != "" {
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			events = append(events, ev)
		}
	}
	return events
}

// eventDedupKey 生成会话事件的去重键；非消息事件返回空（不参与去重，保留原始顺序）。
func eventDedupKey(ev map[string]interface{}) string {
	evType := getStr(ev, "type")
	if evType != "user/message" && evType != "assistant/message" {
		return ""
	}
	data, _ := ev["data"].(map[string]interface{})
	if data == nil {
		return evType + "|" + getStr(ev, "time")
	}
	// assistant/message 内容在 data.message.id；user/message 在 data.id
	id := ""
	if msg, ok := data["message"].(map[string]interface{}); ok {
		id = getStr(msg, "id")
	}
	if id == "" {
		id = getStr(data, "id")
	}
	if id != "" {
		return evType + "|" + id
	}
	return evType + "|" + getStr(ev, "time")
}

// notificationToFrontend 把原始 SDK 通知转换为前端 viewSession 兼容的消息列表
func notificationToFrontend(n harness.Notification) []map[string]interface{} {
	if n.Method == "end" {
		return []map[string]interface{}{{"type": "end"}}
	}
	if n.Method != "session.event" {
		return nil
	}
	ev, _ := n.Payload["event"].(map[string]interface{})
	if ev == nil {
		return nil
	}
	evType := getStr(ev, "type")

	var role string
	switch evType {
	case "user/message":
		role = "user"
	case "assistant/message":
		role = "assistant"
	default:
		return nil // 非对话事件不推给前端
	}

	// 内层 data 包含真正的消息内容
	payload, _ := ev["data"].(map[string]interface{})
	if payload == nil {
		return nil
	}

	var message map[string]interface{}
	if role == "assistant" {
		if m, ok := payload["message"].(map[string]interface{}); ok {
			message = m
		} else {
			message = payload
		}
	} else {
		message = payload
	}
	message = convertMessageToFrontend(message)

	msg := map[string]interface{}{"type": role, "message": message}
	if ts := ev["time"]; ts != nil {
		msg["timestamp"] = formatDshTime(ts)
	}
	return []map[string]interface{}{msg}
}

// formatDshTime 把 dsh 的毫秒时间戳转为 "YYYY-MM-DD HH:MM:SS.SSS" 格式
func formatDshTime(v interface{}) string {
	var ms int64
	switch n := v.(type) {
	case float64:
		ms = int64(n)
	case int64:
		ms = n
	case json.Number:
		ms, _ = n.Int64()
	default:
		return ""
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05.000")
}

// convertMessageToFrontend 把 dsh 的 message 内容块转换为前端 formatSessionMessages 期望的格式。
// dsh 使用 reasoning / tool-call 类型，前端期望 thinking / tool_use。
func convertMessageToFrontend(message map[string]interface{}) map[string]interface{} {
	if message == nil {
		return message
	}
	content, ok := message["content"].([]interface{})
	if !ok {
		return message
	}
	converted := make([]interface{}, 0, len(content))
	for _, raw := range content {
		block, ok := raw.(map[string]interface{})
		if !ok {
			converted = append(converted, raw)
			continue
		}
		bt := getStr(block, "type")
		switch bt {
		case "reasoning":
			// dsh: {type:"reasoning", text:"..."} → 前端: {type:"thinking", thinking:"..."}
			nb := map[string]interface{}{"type": "thinking"}
			if v, ok := block["text"].(string); ok {
				nb["thinking"] = v
			}
			converted = append(converted, nb)
		case "tool-call":
			// dsh: {type:"tool-call", name, arguments(string), id} → 前端: {type:"tool_use", name, input(obj), id}
			nb := map[string]interface{}{"type": "tool_use"}
			if v, ok := block["name"].(string); ok {
				nb["name"] = v
			}
			if v, ok := block["id"].(string); ok {
				nb["id"] = v
			}
			if args, ok := block["arguments"].(string); ok && args != "" {
				var input interface{}
				if err := json.Unmarshal([]byte(args), &input); err == nil {
					nb["input"] = input
				} else {
					nb["input"] = args
				}
			}
			converted = append(converted, nb)
		default:
			converted = append(converted, block)
		}
	}
	out := make(map[string]interface{}, len(message))
	for k, v := range message {
		out[k] = v
	}
	out["content"] = converted
	return out
}

// convertEventsToFrontend 把 sdk_trace 中的事件转为前端格式
func convertEventsToFrontend(rawEvents []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, ev := range rawEvents {
		evType := getStr(ev, "type")
		var role string
		switch evType {
		case "user/message":
			role = "user"
		case "assistant/message":
			role = "assistant"
		default:
			continue
		}
		payload, _ := ev["data"].(map[string]interface{})
		if payload == nil {
			continue
		}
		var message map[string]interface{}
		if role == "assistant" {
			if m, ok := payload["message"].(map[string]interface{}); ok {
				message = m
			} else {
				message = payload
			}
		} else {
			message = payload
		}
		message = convertMessageToFrontend(message)
		msg := map[string]interface{}{"type": role, "message": message}
		if ts := ev["time"]; ts != nil {
			msg["timestamp"] = formatDshTime(ts)
		}
		out = append(out, msg)
	}
	return out
}

// ── 审计 (audit) ──

func (h *AdminHandler) HandleAudit(w http.ResponseWriter, r *http.Request) {
	page := atoiDefault(r.URL.Query().Get("page"), 1)
	pageSize := atoiDefault(r.URL.Query().Get("page_size"), 10)
	if pageSize > 100 {
		pageSize = 100
	}
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * pageSize

	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	linkID := strings.TrimSpace(r.URL.Query().Get("linkid"))

	tasks, total, err := h.store.ListTasksPaged(taskID, linkID, offset, pageSize)
	if err != nil {
		writeJSON(w, 500, resp(5000, err.Error(), "", "", nil))
		return
	}

	type auditView struct {
		TaskID     string `json:"task_id"`
		LinkID     string `json:"linkid"`
		SessionID  string `json:"session_id"`
		Title      string `json:"title"`
		Path       string `json:"path"`
		Status     string `json:"status"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	}
	views := make([]auditView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, auditView{
			TaskID:     t.TaskID,
			LinkID:     t.LinkID,
			SessionID:  t.SessionID,
			Title:      extractTitle(t.RequestBody),
			Path:       t.Path,
			Status:     string(t.Status),
			CreatedAt:  t.CreatedAt,
			FinishedAt: t.FinishedAt,
		})
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"items": views, "total": total, "page": page, "page_size": pageSize,
	}))
}

func (h *AdminHandler) HandleAuditDetail(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/admin/audit/")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", taskID, task.LinkID, map[string]interface{}{
		"task_id":          task.TaskID,
		"linkid":           task.LinkID,
		"session_id":       task.SessionID,
		"title":            extractTitle(task.RequestBody),
		"path":             task.Path,
		"status":           string(task.Status),
		"error_message":    task.ErrorMessage,
		"created_at":       task.CreatedAt,
		"finished_at":      task.FinishedAt,
		"request_body":     mustParseJSON(task.RequestBody),
		"request_response": mustParseJSON(task.RequestResponse),
		"prompt":           task.Prompt,
		"sdk_output":       task.SdkOutput,
		"sdk_trace":        task.SdkTrace,
	}))
}

// HandleLinkTimeline 会话处理时间线（按 linkid 聚合所有 task），管理后台内部接口
func (h *AdminHandler) HandleLinkTimeline(w http.ResponseWriter, r *http.Request) {
	// path = /admin/audit/link/:linkid/timeline
	linkID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/audit/link/"), "/timeline")
	linkID = strings.Trim(linkID, "/")
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "json"
	}

	tasks, err := h.store.ListTasksByLinkid(linkID, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, resp(5000, err.Error(), "", linkID, nil))
		return
	}
	if len(tasks) == 0 {
		if format == "html" {
			writeHTML404(w, "该 linkid 下无任务记录")
			return
		}
		if format == "md" {
			writeText(w, http.StatusNotFound, "text/markdown; charset=utf-8", "# 该 linkid 下无任务记录\n")
			return
		}
		writeJSON(w, http.StatusNotFound, resp(1001, "该 linkid 下无任务记录", "", linkID, nil))
		return
	}

	views := make([]historyTaskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, historyTaskView{
			TaskID:       t.TaskID,
			SessionID:    t.SessionID,
			Title:        extractTitle(t.RequestBody),
			Path:         t.Path,
			Status:       string(t.Status),
			ErrorMessage: t.ErrorMessage,
			CreatedAt:    t.CreatedAt,
			FinishedAt:   t.FinishedAt,
			Events:       buildOutputOnlyEvents(&t),
		})
	}

	if format == "html" {
		writeText(w, http.StatusOK, "text/html; charset=utf-8", renderHistoryHTML(linkID, views))
		return
	}
	if format == "md" {
		writeText(w, http.StatusOK, "text/markdown; charset=utf-8", renderHistoryMd(linkID, views))
		return
	}

	writeJSON(w, http.StatusOK, resp(0, "success", "", linkID, map[string]interface{}{
		"linkid": linkID,
		"total":  len(views),
		"tasks":  views,
	}))
}

// ── 系统设置 ──

func (h *AdminHandler) HandleSysConfig(w http.ResponseWriter, r *http.Request) {
	name := h.store.GetConfig("sys.name")
	if name == "" {
		name = "AI 数字员工"
	}
	// token 已哈希存储，不再回传哈希值，仅告知是否已配置
	hasToken := h.store.GetConfig("api_auth_token") != ""
	authEnabled := h.store.GetConfig("api_auth_enabled") == "1"
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"sys": map[string]interface{}{"name": name},
		"api": map[string]interface{}{
			"auth_enabled": authEnabled,
			"auth_token":   "", // 不回传 token/哈希
			"has_token":    hasToken,
		},
	}})
}

func (h *AdminHandler) HandleSysConfigSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sys struct {
			Name string `json:"name"`
		} `json:"sys"`
		API struct {
			AuthEnabled bool   `json:"auth_enabled"`
			AuthToken   string `json:"auth_token"`
		} `json:"api"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, 400, resp(1001, "无效的请求体", "", "", nil))
		return
	}
	if body.Sys.Name != "" {
		_ = h.store.SetConfig("sys.name", strings.TrimSpace(body.Sys.Name), "系统名称")
	}
	if strings.TrimSpace(body.API.AuthToken) != "" {
		// 仅存 SHA-256 哈希，不落明文
		hash := hashApiToken(strings.TrimSpace(body.API.AuthToken))
		_ = h.store.SetConfig("api_auth_token", hash, "对外 API 认证 Token（SHA-256 哈希）")
	}
	// 保存 api_auth_enabled 开关状态
	_ = h.store.SetConfig("api_auth_enabled", boolToStr(body.API.AuthEnabled), "对外 API 认证开关")
	writeJSON(w, 200, resp(0, "保存成功", "", "", nil))
}

// ── 用户管理 ──

func (h *AdminHandler) HandleUsers(w http.ResponseWriter, r *http.Request) {
	users, _ := h.store.ListUsers()
	type userView struct {
		ID        int    `json:"id"`
		UserName  string `json:"user_name"`
		Role      int    `json:"role"`
		Note      string `json:"note"`
		CreatedAt string `json:"created_at"`
	}
	views := make([]userView, 0, len(users))
	for _, u := range users {
		views = append(views, userView{ID: u.ID, UserName: u.Username, Role: u.Role, Note: u.Note, CreatedAt: u.CreatedAt})
	}
	writeJSON(w, 200, resp(0, "success", "", "", views))
}

func (h *AdminHandler) HandleUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UserName string `json:"user_name"`
		UserPwd  string `json:"user_pwd"`
		Role     int    `json:"role"`
		Note     string `json:"note"`
	}
	if err := readJSON(r, &body); err != nil || body.UserName == "" || body.UserPwd == "" {
		writeJSON(w, 400, resp(1001, "用户名和密码不能为空", "", "", nil))
		return
	}
	if err := validatePasswordStrength(body.UserPwd); err != "" {
		writeJSON(w, 400, resp(1001, err, "", "", nil))
		return
	}
	if existing, _ := h.store.GetUser(body.UserName); existing != nil {
		writeJSON(w, 409, resp(1001, "用户名已存在", "", "", nil))
		return
	}
	hash, err := store.HashPassword(body.UserPwd)
	if err != nil {
		writeJSON(w, 500, resp(1001, "创建失败", "", "", nil))
		return
	}
	role := 0
	if body.Role == 1 {
		role = 1
	}
	if _, err := h.store.CreateUser(body.UserName, hash, role, body.Note); err != nil {
		writeJSON(w, 500, resp(1001, "创建失败", "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "添加成功", "", "", nil))
}

func (h *AdminHandler) HandleUserDelete(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	if username == "" {
		writeJSON(w, 400, resp(1001, "缺少用户名", "", "", nil))
		return
	}
	if username == "admin" {
		writeJSON(w, 400, resp(1001, "不能删除 admin 用户", "", "", nil))
		return
	}
	if !h.store.DeleteUser(username) {
		writeJSON(w, 404, resp(1001, "用户不存在", "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "已删除", "", "", nil))
}

// ── 配置 CSV ──

func (h *AdminHandler) HandlePromptConfig(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1"
	rows := h.store.ListConfigRows(reveal)
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": map[string]interface{}{"rows": rows, "count": len(rows)}})
}

func (h *AdminHandler) HandlePromptConfigTemplate(w http.ResponseWriter, r *http.Request) {
	csv := ""
	templatePaths := []string{
		filepath.Join(h.cfg.Sys.DataDir, "cfg.csv.template"),
		"cfg.csv.template",
	}
	for _, p := range templatePaths {
		if data, err := os.ReadFile(p); err == nil {
			csv = string(data)
			break
		}
	}
	if csv == "" {
		csv = "key,value,说明\n"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="prompt-config-template.csv"`)
	// BOM for Excel UTF-8 compatibility
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	_, _ = w.Write([]byte(csv))
}

func (h *AdminHandler) HandlePromptConfigUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, 400, resp(1001, "解析上传失败", "", "", nil))
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, resp(1001, "缺少上传文件", "", "", nil))
		return
	}
	defer f.Close()

	reader := csv.NewReader(f)
	records, err := reader.ReadAll()
	if err != nil {
		writeJSON(w, 400, resp(1001, "CSV解析失败: "+err.Error(), "", "", nil))
		return
	}

	var rows []store.ConfigRow
	var keys []string
	lineNo := 1 // csv.NewReader 已跳过了第一行（header），但我们循环访问
	for _, rec := range records {
		lineNo++
		// 跳过空行和注释行
		if len(rec) == 0 {
			continue
		}
		// 此行不是标准格式，可能只有一列
		if len(rec) < 2 {
			continue
		}
		key := strings.TrimSpace(rec[0])
		val := strings.TrimSpace(rec[1])
		// 跳过 CSV 表头重复行
		if key == "key" && val == "value" {
			continue
		}
		if key == "" || val == "" {
			continue
		}
		// 敏感检测：key 以 .token/.key/.secret/.password 或 _token/_key/_secret/_password 结尾
		isSecret := false
		for _, suffix := range []string{"token", "secret", "password", "key"} {
			if strings.HasSuffix(key, "_"+suffix) || strings.HasSuffix(key, "."+suffix) {
				isSecret = true
				break
			}
		}
		note := ""
		if len(rec) >= 3 {
			note = strings.TrimSpace(rec[2])
		}
		rows = append(rows, store.ConfigRow{Key: key, Value: val, IsSecret: isSecret, Note: note})
		keys = append(keys, key)
	}

	if len(rows) == 0 {
		writeJSON(w, 400, resp(1001, "CSV 文件中没有有效数据行", "", "", nil))
		return
	}

	_ = h.store.ClearConfigRows()
	if err := h.store.ImportConfigRows(rows); err != nil {
		writeJSON(w, 500, resp(5000, "保存失败: "+err.Error(), "", "", nil))
		return
	}
	slog.Info("imported config rows", "count", len(rows))
	writeJSON(w, 200, resp(0, "导入成功", "", "", map[string]interface{}{"imported": len(rows), "keys": keys}))
}

// ── Prompt Tar ──

func (h *AdminHandler) HandlePromptTarDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=prompts.tar.gz")
	gw := gzip.NewWriter(w)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	// 遍历 prompts/ 目录
	_ = filepath.Walk("prompts", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		tw.WriteHeader(&tar.Header{
			Name: strings.TrimPrefix(path, "prompts/"),
			Size: int64(len(data)),
			Mode: 0644,
		})
		tw.Write(data)
		return nil
	})
}

func (h *AdminHandler) HandlePromptPreview(w http.ResponseWriter, r *http.Request) {
	type fv struct {
		File string `json:"file"`
		Raw  string `json:"raw"`
	}
	var out []fv

	// 直接遍历 prompts/ 目录，返回所有 .md 文件（包括 claude-md.md、sections/*.md）
	// 与 ScanPromptsDir 不同：预览要展示所有模板文件，而非仅暴露为访问路径的文件。
	candidates := []string{"prompts", filepath.Join(h.cfg.Sys.DataDir, "..", "prompts")}
	for _, dir := range candidates {
		abs, _ := filepath.Abs(dir)
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			continue
		}
		_ = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(info.Name(), ".md") {
				return nil
			}
			rel, _ := filepath.Rel(abs, path)
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			out = append(out, fv{File: rel, Raw: string(data)})
			return nil
		})
		break
	}

	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"previews": out}))
}

// HandlePromptTarUpload 上传 tar/zip 包并替换 prompts/ 目录
func (h *AdminHandler) HandlePromptTarUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, 400, resp(1001, "请求体太大", "", "", nil))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, resp(1001, "缺少上传文件", "", "", nil))
		return
	}
	defer file.Close()

	// 检查 CSV 已配置
	if len(h.store.ListConfigRows(false)) == 0 {
		writeJSON(w, 400, resp(1001, "请先上传 CSV 配置文件（步骤 1），再进行步骤 2", "", "", nil))
		return
	}
	validKeys := h.store.AllConfigKeysSet()
	builtinKeys := []string{"request.body", "request.linkid", "request.files",
		"request.form_data", "request.approval_history"}
	for _, k := range builtinKeys {
		validKeys[k] = true
	}

	buf, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, 400, resp(1001, "读取文件失败", "", "", nil))
		return
	}

	files, extractErr := extractArchive(buf, hdr.Filename)
	if extractErr != "" {
		writeJSON(w, 400, resp(1001, extractErr, "", "", nil))
		return
	}
	if files == nil {
		writeJSON(w, 400, resp(1001, "无法解压文件。支持的格式：.tar / .tar.gz / .tgz / .zip", "", "", nil))
		return
	}
	if len(files) == 0 {
		writeJSON(w, 400, resp(1001, "包内没有 .md 文件", "", "", nil))
		return
	}

	// 校验占位符（仅提示，不阻断上传）
	var warnings []string
	rePH := regexp.MustCompile(`\{\{([^}]+)\}\}`)
	for name, content := range files {
		for _, m := range rePH.FindAllStringSubmatch(content, -1) {
			inner := strings.TrimSpace(m[1])
			if strings.HasPrefix(inner, "section:") || strings.HasPrefix(inner, "request.") {
				continue
			}
			if !validKeys[inner] {
				warnings = append(warnings, name+": 占位符 {{"+inner+"}} 未在 CSV 中配置")
			}
		}
	}

	// 替换 prompts/ 目录
	promptsDir := "prompts"
	if _, err := os.Stat(promptsDir); err == nil {
		os.RemoveAll(promptsDir)
	}
	os.MkdirAll(promptsDir, 0755)
	for name, content := range files {
		dest := filepath.Join(promptsDir, name)
		os.MkdirAll(filepath.Dir(dest), 0755)
		os.WriteFile(dest, []byte(content), 0644)
	}

	// 重新扫描并同步 interfaces
	engine.InitPrompts(h.cfg.Sys.DataDir)
	promptFiles, _ := engine.ScanPromptsDir(h.cfg.Sys.DataDir)
	for _, pf := range promptFiles {
		_ = h.store.UpsertInterface(pf.Path+"-id", pf.Name, pf.Path, "")
	}

	fileList := make([]string, 0, len(files))
	for k := range files {
		fileList = append(fileList, k)
	}
	slog.Info("prompt archive uploaded", "files", len(fileList), "warnings", len(warnings))
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"files": fileList, "total": len(fileList), "warnings": warnings}))
}

// ── 记忆管理 ──

func (h *AdminHandler) HandleMemoryList(w http.ResponseWriter, r *http.Request) {
	items := engine.ListMemories(h.cfg.Sys.DataDir)
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"items": items, "count": len(items)}))
}

func (h *AdminHandler) HandleMemoryContent(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/memory/"), "/content")
	content, ok := engine.GetMemory(h.cfg.Sys.DataDir, filename)
	if !ok {
		writeJSON(w, 404, resp(1001, "记忆文件不存在", "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"filename": filename, "content": content}))
}

func (h *AdminHandler) HandleMemoryUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		writeJSON(w, 400, resp(1001, "请求体太大", "", "", nil))
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, 400, resp(1001, "缺少上传文件", "", "", nil))
		return
	}
	defer file.Close()

	// 记忆库规模限制：单文件 ≤ 100KB，文件数 ≤ 20，总量 ≤ 2MB。
	// 索引表会注入每个任务的提示词，文件过多会拖长上下文；单文件过大读取时会吃掉大量 token。
	const (
		maxSingleBytes = 100 * 1024
		maxFiles       = 20
		maxTotalBytes  = 2 * 1024 * 1024
	)

	engine.EnsureMemoryDir(h.cfg.Sys.DataDir)
	existing := engine.ListMemories(h.cfg.Sys.DataDir)
	existingTotal := int64(0)
	for _, m := range existing {
		existingTotal += m.Size
	}

	// 还原中文文件名（multipart 按 latin1 解码）
	filename := decodeUploadFilename(hdr.Filename)

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, 400, resp(1001, "读取文件失败", "", "", nil))
		return
	}

	// 收集待写入的 .md 文件内容
	type pendingFile struct {
		name    string
		content string
	}
	var pending []pendingFile

	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".md"):
		pending = append(pending, pendingFile{name: filename, content: string(data)})
	case strings.HasSuffix(lower, ".xmind"):
		// 将 .xmind 脑图转为 Markdown，存为同名 .md
		md, err := engine.XmindToMarkdown(data)
		if err != nil {
			writeJSON(w, 400, resp(1001, err.Error(), "", "", nil))
			return
		}
		mdName := filename[:len(filename)-len(".xmind")] + ".md"
		pending = append(pending, pendingFile{name: mdName, content: md})
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz") ||
		strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".zip"):
		// 复用 extractArchive 解压，传 nil 不剥离顶级目录
		entries, errMsg := extractArchive(data, filename)
		if errMsg != "" {
			writeJSON(w, 400, resp(1001, errMsg, "", "", nil))
			return
		}
		for name, content := range entries {
			if !strings.HasSuffix(strings.ToLower(name), ".md") {
				continue
			}
			pending = append(pending, pendingFile{name: name, content: content})
		}
	default:
		writeJSON(w, 400, resp(1001, "仅支持 .md / .xmind / .tar.gz / .tgz / .zip 文件", "", "", nil))
		return
	}

	// 校验：单文件大小
	for _, p := range pending {
		bytes := int64(len(p.content))
		if bytes > maxSingleBytes {
			writeJSON(w, 400, resp(1001, fmt.Sprintf("文件「%s」超过单文件上限 100KB（当前 %.1fKB），请精简内容后重试", p.name, float64(bytes)/1024), "", "", nil))
			return
		}
	}

	// 校验：文件数量（按文件名去重后的增量）
	existingNames := make(map[string]bool)
	for _, m := range existing {
		existingNames[m.Filename] = true
	}
	newCount := 0
	for _, p := range pending {
		if !existingNames[p.name] {
			newCount++
		}
	}
	if len(existing)+newCount > maxFiles {
		writeJSON(w, 400, resp(1001, fmt.Sprintf("记忆文件数将超过上限 %d 个（当前 %d 个，本次新增 %d 个），请合并或删除部分文件", maxFiles, len(existing), newCount), "", "", nil))
		return
	}

	// 校验：总字节数
	incomingTotal := int64(0)
	for _, p := range pending {
		incomingTotal += int64(len(p.content))
	}
	existingKeptTotal := int64(0)
	for _, m := range existing {
		overwritten := false
		for _, p := range pending {
			if p.name == m.Filename {
				overwritten = true
				break
			}
		}
		if !overwritten {
			existingKeptTotal += m.Size
		}
	}
	if existingKeptTotal+incomingTotal > maxTotalBytes {
		writeJSON(w, 400, resp(1001, fmt.Sprintf("记忆库总量将超过上限 2MB（当前 %.1fKB，本次新增 %.1fKB），请精简内容", float64(existingKeptTotal)/1024, float64(incomingTotal)/1024), "", "", nil))
		return
	}

	var uploaded []string
	for _, p := range pending {
		safe, err := engine.SaveMemory(h.cfg.Sys.DataDir, p.name, p.content)
		if err != nil {
			continue
		}
		uploaded = append(uploaded, safe)
	}

	if len(uploaded) == 0 {
		writeJSON(w, 400, resp(1001, "没有有效的文件", "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"files": uploaded, "total": len(uploaded)}))
}

func (h *AdminHandler) HandleMemoryDelete(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/admin/memory/")
	if ok := engine.DeleteMemory(h.cfg.Sys.DataDir, filename); !ok {
		writeJSON(w, 404, resp(1001, "记忆文件不存在", "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "已删除", "", "", nil))
}

// decodeUploadFilename 还原 multer/busboy 对中文文件名的 latin1→UTF-8 解码问题
func decodeUploadFilename(name string) string {
	if name == "" {
		return name
	}
	// Go 的 multipart 已经按 UTF-8 解码，通常无需处理；这里兜底转一次（对纯 ASCII 无副作用）
	return name
}

// HandleSessionMessages 一次性拉取会话历史
func (h *AdminHandler) HandleSessionMessages(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/sessions/")
	path = strings.TrimSuffix(path, "/messages")
	path = strings.TrimSuffix(path, "/")
	sessionID := path
	if sessionID == "" {
		writeJSON(w, 400, resp(1001, "缺少 session_id", "", "", nil))
		return
	}

	rawEvents := h.loadSessionEvents(sessionID)
	messages := convertEventsToFrontend(rawEvents)

	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"session_id": sessionID,
		"messages":   messages,
	}))
}

// ── 任务审计 / trace / link log ──

func (h *AdminHandler) HandleTaskTrace(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("taskid")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", taskID, task.LinkID, map[string]interface{}{
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

func (h *AdminHandler) HandleTaskLog(w http.ResponseWriter, r *http.Request) {
	h.HandleTaskTrace(w, r)
}

func (h *AdminHandler) HandleLinkLog(w http.ResponseWriter, r *http.Request) {
	linkID := strings.TrimPrefix(r.URL.Path, "/api/task/link/log/")
	task, _ := h.store.GetLatestTaskByLinkid(linkID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "无该 linkid 的任务记录", "", linkID, nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", task.TaskID, linkID, map[string]interface{}{
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

// ── 会话管理 ──

func (h *AdminHandler) HandleSessions(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.ListAllSessions()
	if err != nil {
		writeJSON(w, 500, resp(5000, err.Error(), "", "", nil))
		return
	}
	if list == nil {
		list = []store.Session{}
	}
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": list})
}

func (h *AdminHandler) HandleSessionDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/admin/sessions/")
	session, _ := h.store.GetSession(sessionID)
	if session == nil {
		writeJSON(w, 404, resp(1001, "会话不存在", "", "", nil))
		return
	}
	// 逻辑删除：该 linkid 下未完成任务标记为 cancelled，session 标记为 closed，
	cancelled := h.store.CancelByLinkid(session.LinkID)
	h.store.CloseSession(sessionID)
	// 删除工作目录（按 sessionID 命名，位于 runtime/tasks）
	workDir := engine.SessionWorkDir(h.cfg.RuntimeTasksDir(), sessionID)
	if _, err := os.Stat(workDir); err == nil {
		_ = os.RemoveAll(workDir)
	}
	slog.Info("session_deleted", "session", sessionID, "linkid", session.LinkID, "cancelled_tasks", cancelled)
	writeJSON(w, 200, resp(0, "已删除", "", "", nil))
}

// HandleSessionChat 会话续聊（debug 用）：复用原 session 上下文发送消息 SSE 流
func (h *AdminHandler) HandleSessionChat(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// path = /admin/sessions/:session_id/chat
	parts := strings.Split(strings.TrimPrefix(path, "/admin/sessions/"), "/")
	if len(parts) < 2 || parts[1] != "chat" {
		writeJSON(w, 400, resp(1001, "缺少 session_id", "", "", nil))
		return
	}
	sessionID := parts[0]

	var body struct {
		Message string `json:"message"`
	}
	if err := readJSON(r, &body); err != nil || body.Message == "" {
		writeJSON(w, 400, resp(1001, "缺少 message", "", "", nil))
		return
	}

	session, _ := h.store.GetSession(sessionID)
	if session == nil {
		writeJSON(w, 404, resp(1001, "会话不存在", "", "", nil))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, resp(5000, "SSE not supported", "", "", nil))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 复用任务队列共享的 dsh 客户端，避免新起 harness.Client 去 resume 已被持有的 session
	ds, err := h.queue.DSH()
	if err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "启动运行时失败: " + err.Error()})
		return
	}

	// resume session（同一客户端内，session 若已打开会报 already owned，忽略即可继续对话）
	if err := ds.ResumeSession(sessionID); err != nil {
		// 已打开则说明上下文已就绪，直接继续
		if !strings.Contains(err.Error(), "already owned") {
			sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "恢复会话失败: " + err.Error()})
			return
		}
	}

	// 切到该 session 对应的工作目录（与任务处理时一致）
	workDir := engine.SessionWorkDir(h.cfg.RuntimeTasksDir(), sessionID)
	if err := ds.SetWorkingDirectory(sessionID, workDir); err != nil {
		slog.Warn("chat set working directory failed", "session", sessionID, "workdir", workDir, "err", err)
	}

	// run query with stream
	_, err = ds.Run(sessionID, body.Message, func(n harness.Notification) {
		data, _ := json.Marshal(n)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()
	})
	if err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	sendSSEEvent(w, flusher, map[string]interface{}{"type": "end"})
}

// HandleChatNew 新建聊天：不绑定公文，发送首条消息并流式返回，session 由 dsh 真实创建
func (h *AdminHandler) HandleChatNew(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string `json:"message"`
	}
	if err := readJSON(r, &body); err != nil || body.Message == "" {
		writeJSON(w, 400, resp(1001, "缺少 message", "", "", nil))
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, resp(5000, "SSE not supported", "", "", nil))
		return
	}

	linkid := "chat-" + newToken()[:8]
	sessionID := "s-" + linkid

	// 会话工作目录按 sessionID 命名，位于 runtime/tasks（不落在 data/ 挂载卷内）
	runtimeTasksDir := h.cfg.RuntimeTasksDir()
	if err := os.MkdirAll(runtimeTasksDir, 0755); err != nil {
		writeJSON(w, 500, resp(5000, "创建运行时目录失败: "+err.Error(), "", "", nil))
		return
	}
	workDir := engine.SessionWorkDir(runtimeTasksDir, sessionID)

	// 创建目录 + 写入 CLAUDE.md（仅在目录中无 CLAUDE.md 时写入）
	if _, err := engine.BuildTaskDir(workDir, h.store, h.cfg.RawConfig(), h.cfg.Sys.DataDir); err != nil {
		writeJSON(w, 500, resp(5000, "创建工作目录失败: "+err.Error(), "", "", nil))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ds := harness.NewClient(h.cfg.LLM)
	ds.SetExtraEnv(engine.BuildBusinessEnv(h.store.GetAllConfigPlain()))
	if err := ds.Start(); err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "启动运行时失败: " + err.Error()})
		return
	}
	defer ds.Close()

	// 会话首次使用时，先切到其专属工作目录（SDK 会在该会话首次 prompt 时创建 session）
	if err := ds.SetWorkingDirectory(sessionID, workDir); err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "设置工作目录失败: " + err.Error()})
		return
	}

	capturedSessionId := ""
	result, err := ds.Run(sessionID, body.Message, func(n harness.Notification) {
		if n.Method == "session.event" {
			if sid, _ := n.Payload["sessionId"].(string); sid != "" && capturedSessionId == "" {
				capturedSessionId = sid
				// 同时写入 sessions 表，前端据此发起后续续聊
				_ = h.store.UpsertSession(sid, linkid)
			}
		}
		data, _ := json.Marshal(n)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()
	})
	if err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": err.Error()})
		return
	}
	_ = result
	sendSSEEvent(w, flusher, map[string]interface{}{"type": "end", "session_id": capturedSessionId, "linkid": linkid})
}

// ── 服务器信息（公开） ──

func (h *AdminHandler) HandleInfo(w http.ResponseWriter, r *http.Request) {
	name := h.store.GetConfig("sys.name")
	if name == "" {
		name = "AI 数字员工"
	}
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"sys_name": name,
	}})
}

// ── helpers ──

func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, v map[string]interface{}) {
	data, _ := json.Marshal(v)
	fmt.Fprintf(w, "data: %s\n\n", string(data))
	flusher.Flush()
}

func maskIfPresent(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// extractTitle 从 request_body JSON 中提取标题
func extractTitle(requestBody string) string {
	if requestBody == "" {
		return "-"
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(requestBody), &obj); err != nil {
		return "-"
	}
	if fd, ok := obj["form_data"].(map[string]interface{}); ok {
		if t, _ := fd["title"].(string); t != "" {
			return t
		}
		if t, _ := fd["标题"].(string); t != "" {
			return t
		}
	}
	if t, _ := obj["title"].(string); t != "" {
		return t
	}
	if t, _ := obj["标题"].(string); t != "" {
		return t
	}
	return "-"
}

func mustParseJSON(s string) interface{} {
	if s == "" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	return v
}

// extractArchive 解压 .tar / .tar.gz / .tgz / .zip 包，返回 {filename: content}。
// 顶层固定前缀 "prompts/" 会被剥离。失败时返回错误信息（files 为 nil 表示格式错误，为空表示可解压但无 .md）。
func extractArchive(buf []byte, origName string) (map[string]string, string) {
	lower := strings.ToLower(origName)

	// ── Zip ──
	if strings.HasSuffix(lower, ".zip") || (len(buf) >= 2 && buf[0] == 0x50 && buf[1] == 0x4b) {
		zr, err := zip.NewReader(bytes.NewReader(buf), int64(len(buf)))
		if err != nil {
			return nil, "无法解压 zip 文件"
		}
		files := make(map[string]string)
		for _, entry := range zr.File {
			if entry.FileInfo().IsDir() || !strings.HasSuffix(entry.Name, ".md") {
				continue
			}
			rc, err := entry.Open()
			if err != nil {
				continue
			}
			data, _ := io.ReadAll(rc)
			rc.Close()
			files[strings.TrimPrefix(entry.Name, "prompts/")] = string(data)
		}
		return files, ""
	}

	// ── Tar / Tar.gz ──
	var raw io.Reader = bytes.NewReader(buf)
	if strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".tgz") ||
		(len(buf) >= 2 && buf[0] == 0x1f && buf[1] == 0x8b) {
		gzr, err := gzip.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, "无法解压 gzip 文件"
		}
		defer gzr.Close()
		raw = gzr
	}

	tr := tar.NewReader(raw)
	files := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "无法解压 tar 文件"
		}
		if hdr.Typeflag == tar.TypeDir || !strings.HasSuffix(hdr.Name, ".md") {
			continue
		}
		data, _ := io.ReadAll(tr)
		files[strings.TrimPrefix(hdr.Name, "prompts/")] = string(data)
	}
	return files, ""
}

var _ = bytes.NewReader
var _ = zip.Reader{}
var _ = io.EOF
var _ = fmt.Sprintf
