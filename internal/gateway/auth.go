package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/store"
)

const CookieName = "dp_agt_session"

type Auth struct {
	cfg    *config.Config
	store  *store.Store
	tokens map[string]*LoginSession
}

type LoginSession struct {
	Username string
	Role     int
	ExpireAt time.Time
}

func NewAuth(cfg *config.Config, s *store.Store) *Auth {
	return &Auth{cfg: cfg, store: s, tokens: make(map[string]*LoginSession)}
}

// hashApiToken 对外 API 认证 token 的 SHA-256 十六进制哈希（DB 只存哈希，不存明文）
func hashApiToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// APITokenMiddleware 外部系统调用认证
// 仅当 config 表 api_auth_enabled == "1" 且配置了 api_auth_token 时启用校验；
// 否则放行（与 dp-agt 的 fail-open 语义一致，未配置认证时不阻塞业务）。
// token 在 DB 中仅存 SHA-256 哈希，比较使用 subtle.ConstantTimeCompare 防时序侧信道。
func (a *Auth) APITokenMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dbEnabled := a.store.GetConfig("api_auth_enabled") == "1"
		dbTokenHash := a.store.GetConfig("api_auth_token")

		respondUnauthorized := func(reason string) {
			resp := resp(1003, "Unauthorized: 无效或缺失的 X-Api-Key / Authorization Bearer Token", "", "", nil)
			// 记录校验失败日志：含来源 IP、路径、原因，但不记录 token 明文/哈希
			slog.Warn("api_auth_failed", "path", r.URL.Path, "ip", r.RemoteAddr, "reason", reason, "has_token", extractAPIToken(r) != "")
			writeJSON(w, http.StatusUnauthorized, resp)
		}

		if dbEnabled && dbTokenHash != "" {
			token := extractAPIToken(r)
			if token == "" {
				respondUnauthorized("missing_token")
				return
			}
			providedHash := hashApiToken(token)
			// 哈希长度固定为 64 字符，比较安全
			ok := false
			if len(providedHash) == len(dbTokenHash) {
				ok = subtle.ConstantTimeCompare([]byte(providedHash), []byte(dbTokenHash)) == 1
			}
			if !ok {
				respondUnauthorized("token_mismatch")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// extractAPIToken 从 X-Api-Key 或 Authorization: Bearer 中提取 token
func extractAPIToken(r *http.Request) string {
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return h
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// WebMiddleware 管理后台登录态校验
func (a *Auth) WebMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		sess := a.sessionFromRequest(r)
		if sess == nil {
			// API 请求返回 JSON 401，页面请求重定向到 /login
			if strings.HasPrefix(r.URL.Path, "/admin/") {
				writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "请先登录"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WebAPIMiddleware 内部 API 登录态校验：未登录返回 JSON 401，不跳转页面
func (a *Auth) WebAPIMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := a.sessionFromRequest(r)
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "请先登录"})
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isPublicPath(p string) bool {
	return p == "/login" || p == "/api/auth/login" || p == "/api/auth/logout" ||
		p == "/api/info" || strings.HasPrefix(p, "/static/") ||
		p == "/health" || strings.HasPrefix(p, "/api/v1/") ||
		strings.HasPrefix(p, "/api/tasks") || strings.HasPrefix(p, "/api/task/")
}

func (a *Auth) Login(username, password string) (string, bool) {
	u, err := a.store.GetUser(username)
	if err != nil || u == nil {
		return "", false
	}
	if !store.CheckPassword(u.PasswordHash, password) {
		return "", false
	}
	token := newToken()
	a.tokens[token] = &LoginSession{
		Username: u.Username,
		Role:     u.Role,
		ExpireAt: time.Now().Add(24 * time.Hour),
	}
	return token, true
}

func (a *Auth) Logout(token string) {
	delete(a.tokens, token)
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── 用户上下文 ──

type userKey struct{}

func currentUser(r *http.Request) (string, int) {
	if sess, ok := r.Context().Value(userKey{}).(*LoginSession); ok {
		return sess.Username, sess.Role
	}
	return "", 0
}

// ── 响应辅助 ──

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeText(w http.ResponseWriter, status int, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func readJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func resp(code int, message, taskID, linkID string, data interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"code":      code,
		"timestamp": store.TimeNowFmt(),
		"message":   message,
	}
	if taskID != "" {
		out["task_id"] = taskID
	}
	if linkID != "" {
		out["linkid"] = linkID
	}
	if data != nil {
		out["data"] = data
	}
	return out
}
