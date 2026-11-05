package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/store"
)

type AuthHandler struct {
	cfg   *config.Config
	store *store.Store
	auth  *Auth
}

func NewAuthHandler(cfg *config.Config, s *store.Store, a *Auth) *AuthHandler {
	return &AuthHandler{cfg: cfg, store: s, auth: a}
}

// HandleLogin 登录。兼容两种请求字段：
//   - dp-agt 前端：{"user_name","user_pwd"} → 返回 {"status":"ok","token","must_change_pwd","user"}
//   - 旧 dsh 前端：{"username","password"} → 返回 {"code":0,"data":{...}}
func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		UserName string `json:"user_name"`
		UserPwd  string `json:"user_pwd"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "请求体无效"})
		return
	}

	username := body.Username
	password := body.Password
	if username == "" {
		username = body.UserName
	}
	if password == "" {
		password = body.UserPwd
	}

	if username == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "用户名和密码不能为空"})
		return
	}

	token, ok := h.auth.Login(username, password)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "用户名或密码错误"})
		return
	}

	u, _ := h.store.GetUser(username)
	mustChange := u != nil && u.MustChangePwd

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Now().Add(24 * time.Hour),
		SameSite: http.SameSiteLaxMode,
	})

	// dp-agt 前端协议（status: ok）
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":          "ok",
		"token":           token,
		"must_change_pwd": mustChange,
		"user":            map[string]interface{}{"id": u.ID, "username": u.Username, "role": u.Role},
	})
}

// HandleLogout 登出：递增 user_version 使旧 token 失效，清除 cookie
func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err == nil && cookie.Value != "" {
		if sess, ok := h.auth.tokens[cookie.Value]; ok {
			h.store.IncrementUserVersion(sess.Username)
		}
		h.auth.Logout(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   CookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

// HandleCurrentUser 前端探活
func (h *AuthHandler) HandleCurrentUser(w http.ResponseWriter, r *http.Request) {
	sess := h.auth.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "请先登录"})
		return
	}
	u, _ := h.store.GetUser(sess.Username)
	mustChange := false
	if u != nil {
		mustChange = u.MustChangePwd
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{
			"id":              sess.Username,
			"username":        sess.Username,
			"role":            sess.Role,
			"must_change_pwd": mustChange,
		},
	})
}

// HandleChangePassword 修改密码（含首次强制改密）
func (h *AuthHandler) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess := h.auth.sessionFromRequest(r)
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "请先登录"})
		return
	}

	var body struct {
		OldPwd string `json:"old_pwd"`
		NewPwd string `json:"new_pwd"`
	}
	if err := readJSON(r, &body); err != nil || body.NewPwd == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "参数错误"})
		return
	}

	if err := validatePasswordStrength(body.NewPwd); err != "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "新密码: " + err})
		return
	}

	u, _ := h.store.GetUser(sess.Username)
	if u == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": "用户不存在"})
		return
	}

	// 首次强制改密（must_change_pwd）时跳过旧密码校验；否则必须校验
	if !u.MustChangePwd && body.OldPwd != "" {
		if !store.CheckPassword(u.PasswordHash, body.OldPwd) {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "旧密码错误"})
			return
		}
	}

	hash, err := store.HashPassword(body.NewPwd)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "密码处理失败"})
		return
	}
	h.store.UpdatePassword(sess.Username, hash)
	h.store.ClearMustChangePwd(sess.Username)

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func validatePasswordStrength(password string) string {
	if len(password) < 8 {
		return "密码长度至少 8 位"
	}
	hasLetter := false
	hasDigit := false
	for _, c := range password {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			hasLetter = true
		}
		if c >= '0' && c <= '9' {
			hasDigit = true
		}
	}
	if !hasLetter {
		return "密码必须包含字母"
	}
	if !hasDigit {
		return "密码必须包含数字"
	}
	return ""
}

// sessionFromRequest 从 Cookie 或 Bearer 提取登录态
func (a *Auth) sessionFromRequest(r *http.Request) *LoginSession {
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		if sess, ok := a.tokens[c.Value]; ok && time.Now().Before(sess.ExpireAt) {
			return sess
		}
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimPrefix(h, "Bearer ")
		if sess, ok := a.tokens[token]; ok && time.Now().Before(sess.ExpireAt) {
			return sess
		}
	}
	return nil
}

// ── 模拟操作用的 mockWriter ──

type mockWriter struct {
	status int
	header http.Header
	body   map[string]interface{}
}

func (w *mockWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *mockWriter) WriteHeader(code int) { w.status = code }

func (w *mockWriter) Write(data []byte) (int, error) {
	_ = json.Unmarshal(data, &w.body)
	return len(data), nil
}

func withBody(r *http.Request, body map[string]interface{}) *http.Request {
	data, _ := json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	return r
}

var _ = io.NopCloser
