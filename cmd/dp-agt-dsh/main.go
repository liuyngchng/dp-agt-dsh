package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"dp-agt-dsh/internal/config"
	"dp-agt-dsh/internal/engine"
	"dp-agt-dsh/internal/gateway"
	"dp-agt-dsh/internal/store"
)

func main() {
	// slog 默认使用 UTC 时间；改为本地时区
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				t := a.Value.Time()
				return slog.String(slog.TimeKey, t.Local().Format(time.RFC3339))
			}
			return a
		},
	})))

	slog.Info("==== AI 数字员工 (dsh) starting ====")

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}
	slog.Info("config loaded", "port", cfg.Server.Port, "model", cfg.LLM.Model, "provider", cfg.LLM.Provider)

	// 目录
	os.MkdirAll(cfg.Sys.DataDir, 0755)
	os.MkdirAll(cfg.Sys.DataDir+"/tasks", 0755)
	os.MkdirAll(cfg.Sys.DataDir+"/logs", 0755)

	// 存储
	s, err := store.Open(cfg.Sys.DataDir)
	if err != nil {
		slog.Error("open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	// 加密
	if err := store.InitCrypto(cfg.Sys.DataDir); err != nil {
		slog.Error("init crypto", "err", err)
		os.Exit(1)
	}

	// 迁移：将旧版明文 api_auth_token 哈希化（一次性）
	migrateAPITokenToHash(s)

	// 迁移：为历史遗留的无 session 任务补齐 session（一次性，幂等）
	if n := s.BackfillTaskSessions(); n > 0 {
		slog.Info("backfilled sessions", "linkids", n)
	}

	// 提示词
	engine.InitPrompts(cfg.Sys.DataDir)

	// 同步 interfaces
	promptFiles, _ := engine.ScanPromptsDir(cfg.Sys.DataDir)
	if len(promptFiles) > 0 {
		for _, pf := range promptFiles {
			_ = s.UpsertInterface(pf.Path+"-id", pf.Name, pf.Path, "")
		}
		slog.Info("synced interfaces from prompts/", "count", len(promptFiles))
	}

	// 任务队列
	q := engine.NewQueue(cfg, s)
	slog.Info("task queue ready")

	// 认证
	auth := gateway.NewAuth(cfg, s)

	// 路由
	mux := http.NewServeMux()
	ah := gateway.NewAdminHandler(cfg, s, q)
	ph := gateway.NewProcessHandler(cfg, s, q)
	th := gateway.NewTasksHandler(cfg, s, q)
	authH := gateway.NewAuthHandler(cfg, s, auth)

	// ── 公开路由 ──
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","timestamp":"` + store.TimeNowFmt() + `"}`))
	})
	mux.HandleFunc("/api/info", ah.HandleInfo)

	// 静态文件
	fs := http.FileServer(http.Dir("web"))
	mux.Handle("/static/", fs)
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/login.html")
	})

	// 认证 API（兼容 dp-agt 前端的 /api/login、/api/logout、/api/inner/* 协议）
	mux.HandleFunc("/api/auth/login", authH.HandleLogin)
	mux.HandleFunc("/api/auth/logout", authH.HandleLogout)
	mux.HandleFunc("/api/login", authH.HandleLogin)
	mux.HandleFunc("/api/logout", authH.HandleLogout)

	// 内部认证 API（需登录，用 Cookie/Bearer 校验）
	innerMux := http.NewServeMux()
	innerMux.HandleFunc("/api/inner/whoami", authH.HandleCurrentUser)
	innerMux.HandleFunc("/api/inner/user/password", authH.HandleChangePassword)
	mux.Handle("/api/inner/", auth.WebAPIMiddleware(innerMux))

	// ── API Token 认证 ──
	mux.Handle("/api/v1/", auth.APITokenMiddleware(http.HandlerFunc(ph.Handle)))
	mux.Handle("/api/tasks", auth.APITokenMiddleware(th))
	mux.Handle("/api/task/", auth.APITokenMiddleware(th))

	// ── 管理后台（Web Cookie 认证） ──
	adminMux := http.NewServeMux()

	// 管理后台页面（登录后跳转 /admin，需返回 admin.html）
	adminMux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, proxy-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		http.ServeFile(w, r, "web/admin.html")
	})

	// 导航 / 服务信息
	adminMux.HandleFunc("/admin/nav", ah.HandleNav)
	adminMux.HandleFunc("/admin/server-info", ah.HandleServerInfo)

	// 接口管理
	adminMux.HandleFunc("/admin/interfaces", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleListInterfaces(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// Mock 数据
	adminMux.HandleFunc("/admin/mock", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleMock(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 模拟
	adminMux.HandleFunc("/admin/simulate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ah.HandleSimulate(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 任务监控
	adminMux.HandleFunc("/admin/tasks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleAdminTasks(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/tasks/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleAdminTaskDetail(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 会话流与记录（见下方 /admin/sessions/ 统一处理）
	// 兼容不带子路径的 GET /admin/sessions
	adminMux.HandleFunc("/admin/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleSessions(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 新建聊天：不绑定公文，发送首条消息并流式返回
	adminMux.HandleFunc("/admin/chat/new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ah.HandleChatNew(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 审计
	adminMux.HandleFunc("/admin/audit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleAudit(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/audit/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/admin/audit/link/") && strings.HasSuffix(r.URL.Path, "/timeline") {
			if r.Method == "GET" {
				ah.HandleLinkTimeline(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if r.Method == "GET" {
			ah.HandleAuditDetail(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 系统设置
	adminMux.HandleFunc("/admin/sys-config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			ah.HandleSysConfig(w, r)
		case "PUT":
			ah.HandleSysConfigSave(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	// 用户管理
	adminMux.HandleFunc("/admin/users", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			ah.HandleUsers(w, r)
		case "POST":
			ah.HandleUserCreate(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/users/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			ah.HandleUserDelete(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// Prompt Config
	adminMux.HandleFunc("/admin/prompt-config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandlePromptConfig(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/prompt-config/template", ah.HandlePromptConfigTemplate)
	adminMux.HandleFunc("/admin/prompt-config/upload-csv", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ah.HandlePromptConfigUpload(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// Prompt Tar
	adminMux.HandleFunc("/admin/prompt-tar/download", ah.HandlePromptTarDownload)
	adminMux.HandleFunc("/admin/prompt-tar/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ah.HandlePromptTarUpload(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/prompt-preview", ah.HandlePromptPreview)

	// 记忆管理
	adminMux.HandleFunc("/admin/memory", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			ah.HandleMemoryList(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/memory/upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			ah.HandleMemoryUpload(w, r)
		} else {
			http.NotFound(w, r)
		}
	})
	adminMux.HandleFunc("/admin/memory/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/content") {
			if r.Method == "GET" {
				ah.HandleMemoryContent(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if r.Method == "DELETE" {
			ah.HandleMemoryDelete(w, r)
		} else {
			http.NotFound(w, r)
		}
	})

	// 会话记录（一次性拉取历史 + 实时流 + 续聊 + 删除）
	adminMux.HandleFunc("/admin/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/stream") {
			ah.HandleSessionStream(w, r)
			return
		}
		if strings.HasSuffix(path, "/chat") {
			if r.Method == "POST" {
				ah.HandleSessionChat(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		if strings.HasSuffix(path, "/messages") {
			if r.Method == "GET" {
				ah.HandleSessionMessages(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		switch r.Method {
		case "GET":
			ah.HandleSessions(w, r)
		case "DELETE":
			ah.HandleSessionDelete(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	withAuth := auth.WebMiddleware(adminMux)
	mux.Handle("/admin", withAuth)
	mux.Handle("/admin/", withAuth)

	// 根路径
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})

	// ── 定期清理 ──
	go func() {
		time.Sleep(10 * time.Second)
		cleanupExpired(s, cfg)
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			cleanupExpired(s, cfg)
		}
	}()

	// ── 启动 ──
	addr := fmt.Sprintf(":%d", cfg.Server.Port)
	slog.Info("listening", "addr", addr)

	// 始终 HTTPS（与 dp-agt 一致）。证书由 build/build.sh 构建时生成到 deploy/certs/；
	// 本地开发未生成证书时自动生成自签名证书，保证服务可启动。
	certFile, keyFile := resolveTLSCerts(cfg)
	if certFile == "" || keyFile == "" {
		slog.Error("HTTPS 证书缺失，无法启动。请运行 build/build.sh 构建镜像，或手动生成 deploy/certs/cert.pem、key.pem")
		os.Exit(1)
	}

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		slog.Error("加载 HTTPS 证书失败", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  60 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}

	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		slog.Info("shutting down...")
		srv.Close()
	}()

	if err := srv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
		slog.Error("listen", "err", err)
		os.Exit(1)
	}
	slog.Info("server stopped")
}

// resolveTLSCerts 返回 HTTPS 证书与私钥路径。
// 优先使用 deploy/certs/（与 dp-agt 一致），其次 data/certs/，最后允许通过
// TLS_CERT_FILE / TLS_KEY_FILE 环境变量指定。若都不存在则尝试自动生成自签名证书。
func resolveTLSCerts(cfg *config.Config) (string, string) {
	candidates := [][2]string{
		{"deploy/certs/cert.pem", "deploy/certs/key.pem"},
		{filepath.Join(cfg.Sys.DataDir, "certs", "cert.pem"), filepath.Join(cfg.Sys.DataDir, "certs", "key.pem")},
	}
	if v := os.Getenv("TLS_CERT_FILE"); v != "" {
		if k := os.Getenv("TLS_KEY_FILE"); k != "" {
			candidates = append([][2]string{{v, k}}, candidates...)
		}
	}
	for _, c := range candidates {
		if fileExists(c[0]) && fileExists(c[1]) {
			return c[0], c[1]
		}
	}

	// 自动生成自签名证书（本地开发兜底），参数与 dp-agt build.sh 保持一致
	certFile := "deploy/certs/cert.pem"
	keyFile := "deploy/certs/key.pem"
	if err := os.MkdirAll(filepath.Dir(certFile), 0755); err != nil {
		return "", ""
	}
	cmd := exec.Command("openssl", "req", "-x509", "-newkey", "rsa:2048", "-sha256",
		"-days", "3650", "-nodes",
		"-keyout", keyFile, "-out", certFile,
		"-subj", "/C=CN/O=AI数字员工/CN=localhost",
		"-addext", "subjectAltName=DNS:localhost,IP:127.0.0.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("自动生成自签名证书失败", "err", err, "openssl_output", strings.TrimSpace(string(out)))
		return "", ""
	}
	if fileExists(certFile) && fileExists(keyFile) {
		slog.Info("已自动生成自签名证书", "cert_file", certFile)
		return certFile, keyFile
	}
	return "", ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func cleanupExpired(s *store.Store, cfg *config.Config) {
	retentionDays := cfg.Sys.RetentionDays
	deletedTasks, expiredSessions := s.CleanupExpired(retentionDays)

	if len(expiredSessions) > 0 {
		slog.Info("cleanup", "expired_sessions", len(expiredSessions))
		activeLinks := s.ActiveSessionLinkIDs()
		tasksDir := cfg.Sys.DataDir + "/tasks"
		if entries, err := os.ReadDir(tasksDir); err == nil {
			for _, e := range entries {
				if e.IsDir() && !activeLinks[e.Name()] {
					_ = os.RemoveAll(tasksDir + "/" + e.Name())
				}
			}
		}
	}
	slog.Info("cleanup done", "tasks", deletedTasks, "sessions", len(expiredSessions))
}

// migrateAPITokenToHash 将旧版明文 api_auth_token 哈希化（一次性）。
// 已经是十六进制 64 位（SHA-256 哈希）则跳过。
func migrateAPITokenToHash(s *store.Store) {
	raw := s.GetConfig("api_auth_token")
	if raw == "" {
		return
	}
	if regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(raw) {
		return
	}
	sum := sha256.Sum256([]byte(raw))
	hash := hex.EncodeToString(sum[:])
	_ = s.SetConfig("api_auth_token", hash, "对外 API 认证 Token（SHA-256 哈希）")
	slog.Info("api_token_migrated_to_hash")
}
