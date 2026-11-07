package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	log.Println("==== AI 数字员工 (dsh) starting ====")

	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	cfg, err := config.Load(dataDir)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("config loaded: port=%d model=%s provider=%s", cfg.Server.Port, cfg.LLM.Model, cfg.LLM.Provider)

	// 目录
	os.MkdirAll(cfg.Sys.DataDir, 0755)
	os.MkdirAll(cfg.Sys.DataDir+"/tasks", 0755)
	os.MkdirAll(cfg.Sys.DataDir+"/logs", 0755)

	// 存储
	s, err := store.Open(cfg.Sys.DataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// 加密
	if err := store.InitCrypto(cfg.Sys.DataDir); err != nil {
		log.Fatalf("init crypto: %v", err)
	}

	// 迁移：将旧版明文 api_auth_token 哈希化（一次性）
	migrateAPITokenToHash(s)

	// 提示词
	engine.InitPrompts(cfg.Sys.DataDir)

	// 同步 interfaces
	promptFiles, _ := engine.ScanPromptsDir(cfg.Sys.DataDir)
	if len(promptFiles) > 0 {
		for _, pf := range promptFiles {
			_ = s.UpsertInterface(pf.Path+"-id", pf.Name, pf.Path, "")
		}
		log.Printf("synced %d interfaces from prompts/", len(promptFiles))
	}

	// 任务队列
	q := engine.NewQueue(cfg, s)
	log.Println("task queue ready")

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
	log.Printf("listening on %s", addr)

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		quit := make(chan os.Signal, 1)
		signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
		<-quit
		log.Println("shutting down...")
		srv.Close()
	}()

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
	log.Println("server stopped")
}

func cleanupExpired(s *store.Store, cfg *config.Config) {
	retentionDays := cfg.Sys.RetentionDays
	deletedTasks, expiredSessions := s.CleanupExpired(retentionDays)

	if len(expiredSessions) > 0 {
		log.Printf("cleanup: %d expired sessions", len(expiredSessions))
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
	log.Printf("cleanup done: %d tasks, %d sessions", deletedTasks, len(expiredSessions))
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
	log.Println("api_token_migrated_to_hash")
}
