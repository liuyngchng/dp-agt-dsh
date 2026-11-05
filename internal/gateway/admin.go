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
	"regexp"
	"strings"

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
	protocol := "http"
	if h.cfg.Server.CertFile != "" && h.cfg.Server.KeyFile != "" {
		protocol = "https"
	}
	host := h.cfg.Server.SelfIP
	if host == "" {
		host = "127.0.0.1"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"protocol": protocol,
		"host":     host,
		"port":     h.cfg.Server.Port,
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
		TaskID     string `json:"task_id"`
		LinkID     string `json:"linkid"`
		SessionID  string `json:"session_id"`
		Title      string `json:"title"`
		Path       string `json:"path"`
		Status     string `json:"status"`
		ErrorMsg   string `json:"error_message"`
		CreatedAt  string `json:"created_at"`
		FinishedAt string `json:"finished_at"`
	}
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, taskView{
			TaskID:     t.TaskID,
			LinkID:     t.LinkID,
			SessionID:  t.SessionID,
			Title:      extractTitle(t.RequestBody),
			Path:       t.Path,
			Status:     string(t.Status),
			ErrorMsg:   t.ErrorMessage,
			CreatedAt:  t.CreatedAt,
			FinishedAt: t.FinishedAt,
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

// ── 系统设置 ──

func (h *AdminHandler) HandleSysConfig(w http.ResponseWriter, r *http.Request) {
	name := h.store.GetConfig("sys.name")
	if name == "" {
		name = "AI 数字员工"
	}
	token := h.store.GetConfig("api_auth_token")
	authEnabled := h.store.GetConfig("api_auth_enabled") == "1"
	writeJSON(w, 200, map[string]interface{}{"code": 0, "data": map[string]interface{}{
		"sys": map[string]interface{}{"name": name},
		"api": map[string]interface{}{
			"auth_enabled": authEnabled,
			"auth_token":   token,
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
	if body.API.AuthToken != "" {
		// Don't save the masked placeholder from the frontend
		if !strings.HasPrefix(body.API.AuthToken, "****") && !strings.HasPrefix(body.API.AuthToken, "**") {
			_ = h.store.SetConfig("api_auth_token", body.API.AuthToken, "对外 API 认证 Token")
		}
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
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"users": views}))
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
		File string `json:"file"`
		Raw  string `json:"raw"`
	}
	var out []fv
	for _, pf := range files {
		out = append(out, fv{File: pf.Filename, Raw: pf.Content})
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

	// 校验占位符
	var errors []string
	rePH := regexp.MustCompile(`\{\{([^}]+)\}\}`)
	for name, content := range files {
		for _, m := range rePH.FindAllStringSubmatch(content, -1) {
			inner := strings.TrimSpace(m[1])
			if strings.HasPrefix(inner, "section:") || strings.HasPrefix(inner, "request.") {
				continue
			}
			if !validKeys[inner] {
				errors = append(errors, name+": 占位符 {{"+inner+"}} 未在 CSV 中配置")
			}
		}
	}
	if len(errors) > 0 {
		writeJSON(w, 400, map[string]interface{}{"code": 1001, "message": "占位符校验失败", "data": map[string]interface{}{"errors": errors}})
		return
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
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"files": fileList, "total": len(fileList)}))
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

	// 还原中文文件名（multipart 按 latin1 解码）
	filename := decodeUploadFilename(hdr.Filename)
	if !strings.HasSuffix(strings.ToLower(filename), ".md") {
		writeJSON(w, 400, resp(1001, "仅支持 .md 文件", "", "", nil))
		return
	}
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, 400, resp(1001, "读取文件失败", "", "", nil))
		return
	}
	safe, err := engine.SaveMemory(h.cfg.Sys.DataDir, filename, string(data))
	if err != nil {
		writeJSON(w, 400, resp(1001, "保存失败: "+err.Error(), "", "", nil))
		return
	}
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{"files": []string{safe}, "total": 1}))
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

// HandleSessionMessages 一次性拉取会话历史（dsh 协议暂未实现 session/load，返回空列表占位）
func (h *AdminHandler) HandleSessionMessages(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, resp(0, "success", "", "", map[string]interface{}{
		"session_id": "",
		"messages":   []interface{}{},
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
	// 删除工作目录
	workDir := filepath.Join(h.cfg.Sys.DataDir, "tasks", sanitizeDirName(session.LinkID))
	if _, err := os.Stat(workDir); err == nil {
		_ = os.RemoveAll(workDir)
	}
	h.store.RemoveSession(sessionID)
	log.Printf("[admin] session_deleted: %s linkid=%s workDir=%s", sessionID, session.LinkID, workDir)
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

	// 确保 dsh 已运行
	if h.queue != nil {
		// 使用 queue 现有的 ensureDSH 能力
	}
	// 直接创建 harness client 做 query
	ds := harness.NewClient(h.cfg.LLM)
	if err := ds.Start(); err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "启动运行时失败: " + err.Error()})
		return
	}
	defer ds.Close()

	// resume session
	if err := ds.ResumeSession(sessionID); err != nil {
		sendSSEEvent(w, flusher, map[string]interface{}{"type": "error", "message": "恢复会话失败: " + err.Error()})
		return
	}

	// run query with stream
	_, err := ds.Run(sessionID, body.Message, func(n harness.Notification) {
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

func sanitizeDirName(s string) string {
	replacer := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	)
	return replacer.Replace(s)
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
var _ = log.Println
var _ = fmt.Sprintf
