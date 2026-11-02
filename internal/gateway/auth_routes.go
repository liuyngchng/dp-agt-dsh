package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

func (h *AuthHandler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, resp(1001, "请求体无效", "", "", nil))
		return
	}

	token, ok := h.auth.Login(body.Username, body.Password)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, resp(1003, "用户名或密码错误", "", "", nil))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Now().Add(24 * time.Hour),
		SameSite: http.SameSiteLaxMode,
	})

	u, _ := h.store.GetUser(body.Username)
	mustChange := u != nil && u.MustChangePwd

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"code":    0,
		"message": "登录成功",
		"data": map[string]interface{}{
			"username":        body.Username,
			"role":            u.Role,
			"must_change_pwd": mustChange,
		},
	})
}

func (h *AuthHandler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(CookieName)
	if err == nil {
		h.auth.Logout(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   CookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, resp(0, "已登出", "", "", nil))
}

func (h *AuthHandler) HandleCurrentUser(w http.ResponseWriter, r *http.Request) {
	username, role := currentUser(r)
	if username == "" {
		writeJSON(w, http.StatusUnauthorized, resp(1003, "未登录", "", "", nil))
		return
	}
	u, _ := h.store.GetUser(username)
	mustChange := false
	if u != nil {
		mustChange = u.MustChangePwd
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"code": 0,
		"data": map[string]interface{}{
			"username":        username,
			"role":            role,
			"must_change_pwd": mustChange,
		},
	})
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