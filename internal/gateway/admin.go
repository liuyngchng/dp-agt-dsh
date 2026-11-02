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
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/engine"
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
			tab{"simulate", "fa-flask", "操作模拟"},
			tab{"tasks", "fa-list-alt", "任务监控"},
		)
	}
	tabs = append(tabs, tab{"audit", "fa-search", "任务审查"})
	if isAdmin {
		tabs = append(tabs,
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
	protocol := "http"
	if h.cfg.Server.CertFile != "" && h.cfg.Server.KeyFile != "" {
		protocol = "https"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"port":     h.cfg.Server.Port,
		"protocol": protocol,
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
			"path": pf.Path,
			"name": pf.Name,
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

	path, _ := body["path"].(string)
	payload, _ := body["payload"].(map[string]interface{})
	if path == "" {
		writeJSON(w, http.StatusBadRequest, resp(1001, "缺少 path", "", "", nil))
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
	taskID := r.URL.Query().Get("taskid")
	linkID := r.URL.Query().Get("linkid")
	offset := 0
	limit := 50

	tasks, total, err := h.store.ListTasksPaged(taskID, linkID, offset, limit)
	if err != nil {
		writeJSON(w, 500, resp(5000, err.Error(), "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"items": tasks, "total": total,
	}))
}

func (h *AdminHandler) HandleAdminTaskDetail(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/admin/tasks/")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", taskID, task.LinkID, task))
}

// ── SSE 流 ──

func (h *AdminHandler) HandleSessionStream(w http.ResponseWriter, r *http.Request) {
	sessionID := r.URL.Query().Get("session_id")
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
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			if msg.Method == "end" {
				return
			}
		}
	}
}

// ── 审计 (audit) ──

func (h *AdminHandler) HandleAudit(w http.ResponseWriter, r *http.Request) {
	tasks, err := h.store.ListActiveTasks()
	if err != nil {
		tasks = nil
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"tasks": tasks,
	}))
}

func (h *AdminHandler) HandleAuditDetail(w http.ResponseWriter, r *http.Request) {
	taskID := strings.TrimPrefix(r.URL.Path, "/admin/audit/")
	task, _ := h.store.GetTaskLog(taskID)
	if task == nil {
		writeJSON(w, 404, resp(1001, "任务不存在: "+taskID, "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", taskID, task.LinkID, task))
}

// ── 系统设置 ──

func (h *AdminHandler) HandleSysConfig(w http.ResponseWriter, r *http.Request) {
	name := h.store.GetConfig("sys.name")
	if name == "" {
		name = "AI 数字员工"
	}
	token := h.store.GetConfig("api_auth_token")
	authEnabled := token != ""
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"sys_name": name,
		"api_auth_enabled": authEnabled,
		"api_auth_token": maskIfPresent(token),
	}})
}

func (h *AdminHandler) HandleSysConfigSave(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	readJSON(r, &body)
	if name, ok := body["sys_name"].(string); ok {
		_ = h.store.SetConfig("sys.name", name, "系统名称")
	}
	if token, ok := body["api_auth_token"].(string); ok && token != "" && !strings.HasPrefix(token, "**") {
		_ = h.store.SetConfig("api_auth_token", token, "API认证Token")
	}
	writeJSON(w, 200, resp(0, "保存成功", "", "", nil))
}

// ── 用户管理 ──

func (h *AdminHandler) HandleUsers(w http.ResponseWriter, r *http.Request) {
	users, _ := h.store.ListUsers()
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"users": users}))
}

// ── 配置 CSV ──

func (h *AdminHandler) HandlePromptConfig(w http.ResponseWriter, r *http.Request) {
	rows := h.store.ListConfigRows(false)
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": rows})
}

func (h *AdminHandler) HandlePromptConfigTemplate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Write([]byte("key,value,说明\n"))
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
	for i, rec := range records {
		if i == 0 {
			continue // 跳过 header
		}
		if len(rec) < 2 {
			continue
		}
		key := strings.TrimSpace(rec[0])
		val := strings.TrimSpace(rec[1])
		isSecret := strings.HasSuffix(key, "_token") || strings.HasSuffix(key, "_secret") ||
			strings.HasSuffix(key, "_key") || strings.HasSuffix(key, "_password")
		note := ""
		if len(rec) >= 3 {
			note = strings.TrimSpace(rec[2])
		}
		rows = append(rows, store.ConfigRow{Key: key, Value: val, IsSecret: isSecret, Note: note})
	}

	_ = h.store.ClearConfigRows()
	if err := h.store.ImportConfigRows(rows); err != nil {
		writeJSON(w, 500, resp(5000, "保存失败: "+err.Error(), "", "", nil))
		return
	}
	log.Printf("[admin] imported %d config rows", len(rows))
	writeJSON(w, 200, resp(0, "导入成功", "", "", nil))
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
	_, files := engine.ScanPromptsDir(h.cfg.Sys.DataDir)
	type fv struct {
		Filename string `json:"filename"`
		Path     string `json:"path"`
		Content  string `json:"content"`
	}
	var out []fv
	for _, pf := range files {
		out = append(out, fv{Filename: pf.Filename, Path: pf.Path, Content: pf.Content})
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"files": out}))
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
		"task_id":          task.TaskID,
		"linkid":           task.LinkID,
		"path":             task.Path,
		"status":           task.Status,
		"request_body":     mustParseJSON(task.RequestBody),
		"prompt":           task.Prompt,
		"sdk_output":       task.SdkOutput,
		"callback_body":    mustParseJSON(task.CallbackBody),
		"callback_response": mustParseJSON(task.CallbackResponse),
		"error_message":    task.ErrorMessage,
		"created_at":       task.CreatedAt,
		"finished_at":      task.FinishedAt,
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
		"task_id":          task.TaskID,
		"linkid":           task.LinkID,
		"path":             task.Path,
		"status":           task.Status,
		"request_body":     mustParseJSON(task.RequestBody),
		"prompt":           task.Prompt,
		"sdk_output":       task.SdkOutput,
		"callback_body":    mustParseJSON(task.CallbackBody),
		"error_message":    task.ErrorMessage,
		"created_at":       task.CreatedAt,
		"finished_at":      task.FinishedAt,
	}))
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

func maskIfPresent(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
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

var _ = bytes.NewReader
var _ = zip.Reader{}
var _ = io.EOF
var _ = log.Println
var _ = fmt.Sprintf