package web

import (
	"fmt"
	"html/template"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"

	"mail_go/config"
	"mail_go/internal/storage"
	"mail_go/internal/store"
	"mail_go/internal/web/handlers"
	"mail_go/internal/web/middleware"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// formatBytes converts a file size in bytes to a human-readable string.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// WebServer wraps the Gin engine and its dependencies.
type WebServer struct {
	engine     *gin.Engine
	stores     *store.Stores
	storage    *storage.AttachmentStorage
	cfg        config.WebConfig
	storageCfg config.StorageConfig
	authCfg    config.AuthConfig
	banCfg     config.BanConfig
}

// templateFuncs returns custom template functions for rendering.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"add":     func(a, b int) int { return a + b },
		"sub":     func(a, b int) int { return a - b },
		"mul":     func(a, b int) int { return a * b },
		"div":     func(a, b int) int { return a / b },
		"mod":     func(a, b int) int { return a % b },
		"ceilDiv": func(a, b int) int { return int(math.Ceil(float64(a) / float64(b))) },
		"seq": func(n int) []int {
			result := make([]int, n)
			for i := 0; i < n; i++ {
				result[i] = i + 1
			}
			return result
		},
		"domainName": func(domainID uint, domains []interface{}) string {
			return fmt.Sprintf("Domain #%d", domainID)
		},
		"safeHTML": func(s string) template.HTML {
			return template.HTML(s)
		},
		"safeJS": func(s string) template.JS {
			return template.JS(s)
		},
		"formatBytes": func(b int64) string {
			return formatBytes(b)
		},
	}
}

// NewWebServer creates a new WebServer, initializes the Gin engine,
// configures sessions, middleware, and registers all routes.
func NewWebServer(cfg config.WebConfig, stores *store.Stores, attStorage *storage.AttachmentStorage, storageCfg config.StorageConfig, authCfg config.AuthConfig, banCfg config.BanConfig) *WebServer {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Logger())
	engine.Use(gin.Recovery())

	// Session store (cookie-based)
	cookieStore := cookie.NewStore([]byte("mail-go-secret-key-change-in-production"))
	cookieStore.Options(sessions.Options{
		HttpOnly: true,
		SameSite: 3, // SameSiteLaxMode
		MaxAge:   86400,
		Path:     "/",
	})
	engine.Use(sessions.Sessions("mail_go_session", cookieStore))

	// Load HTML templates with custom functions
	// Note: Go's filepath.Glob doesn't support **, so we load in two passes
	tmpl := template.Must(template.New("").Funcs(templateFuncs()).ParseGlob("internal/web/templates/*.html"))
	template.Must(tmpl.ParseGlob("internal/web/templates/admin/*.html"))
	engine.SetHTMLTemplate(tmpl)

	ws := &WebServer{
		engine:     engine,
		stores:     stores,
		storage:    attStorage,
		cfg:        cfg,
		storageCfg: storageCfg,
		authCfg:    authCfg,
		banCfg:     banCfg,
	}

	ws.registerRoutes()
	return ws
}

// registerRoutes sets up all HTTP routes with their handlers and middleware.
func (ws *WebServer) registerRoutes() {
	authHandler := handlers.NewAuthHandler(ws.stores, ws.authCfg, ws.banCfg)
	mailHandler := handlers.NewMailHandler(ws.stores, ws.storage)
	adminHandler := handlers.NewAdminHandler(ws.stores, ws.storage, filepath.Join(ws.storageCfg.BaseDir, "tls", "domains"))

	// Apply BanMiddleware globally before public routes
	ws.engine.Use(middleware.BanMiddleware(ws.stores))

	// Public routes (no auth required)
	ws.engine.GET("/login", authHandler.ShowLogin)
	ws.engine.POST("/login", authHandler.DoLogin)
	ws.engine.POST("/login/ldap", authHandler.LDAPLogin)
	ws.engine.GET("/auth/oauth2", authHandler.OAuth2Start)
	ws.engine.GET("/auth/oauth2/callback", authHandler.OAuth2Callback)

	// Auth-protected routes
	auth := ws.engine.Group("")
	auth.Use(middleware.AuthMiddleware(ws.stores))
	{
		auth.POST("/logout", authHandler.DoLogout)
		auth.GET("/", func(c *gin.Context) {
			c.Redirect(302, "/inbox")
		})

		// Mail routes
		auth.GET("/inbox", mailHandler.Inbox)
		auth.GET("/inbox/:id", mailHandler.View)
		auth.GET("/compose", mailHandler.Compose)
		auth.POST("/compose", mailHandler.DoSend)
		auth.GET("/drafts", mailHandler.Drafts)
		auth.GET("/drafts/:id", mailHandler.View)
		auth.GET("/sent", mailHandler.Sent)
		auth.GET("/sent/:id", mailHandler.View)
		auth.GET("/settings", mailHandler.Settings)
		auth.POST("/settings", mailHandler.UpdateSettings)
		auth.POST("/mail/delete/:id", mailHandler.Delete)
		auth.POST("/mail/read/:id", mailHandler.MarkRead)
		auth.GET("/attachment/:id", mailHandler.DownloadAttachment)
	}

	// Admin routes (auth + admin required)
	admin := ws.engine.Group("/admin")
	admin.Use(middleware.AuthMiddleware(ws.stores))
	admin.Use(middleware.AdminMiddleware())
	{
		admin.GET("", adminHandler.Dashboard)
		admin.GET("/", adminHandler.Dashboard)
		admin.GET("/domains", adminHandler.ListDomains)
		admin.GET("/domains/new", adminHandler.NewDomain)
		admin.POST("/domains", adminHandler.CreateDomain)
		admin.GET("/domains/:id/edit", adminHandler.EditDomain)
		admin.POST("/domains/:id", adminHandler.UpdateDomain)
		admin.POST("/domains/:id/delete", adminHandler.DeleteDomain)
		admin.GET("/domains/:id/dns", adminHandler.DNSHint)
		admin.GET("/users", adminHandler.ListUsers)
		admin.GET("/users/new", adminHandler.NewUser)
		admin.POST("/users", adminHandler.CreateUser)
		admin.POST("/users/:id/delete", adminHandler.DeleteUser)
		admin.GET("/users/:id/edit", adminHandler.EditUser)
		admin.POST("/users/:id", adminHandler.UpdateUser)
		admin.GET("/mails", adminHandler.ListMails)
		admin.GET("/mails/:id", adminHandler.AdminViewMail)
		admin.GET("/attachment/:id", adminHandler.AdminDownloadAttachment)
		admin.GET("/bans", adminHandler.ListBans)
		admin.POST("/bans/:id/unban", adminHandler.UnbanIP)
		admin.POST("/bans/cleanup", adminHandler.CleanupBans)
	}
}

// Start launches the HTTP server on the configured address.
// Supports both TCP (e.g. ":8080") and Unix socket (e.g. "/run/mail_go/web.sock").
func (ws *WebServer) Start() error {
	addr := ws.cfg.Addr

	// Unix socket: 地址以 / 开头
	if strings.HasPrefix(addr, "/") {
		// 清理旧的 socket 文件
		os.Remove(addr)

		listener, err := net.Listen("unix", addr)
		if err != nil {
			return fmt.Errorf("监听 Unix socket 失败 %s: %w", addr, err)
		}
		// 允许 nginx 等外部进程连接
		os.Chmod(addr, 0666)

		return ws.engine.RunListener(listener)
	}

	// TCP 端口
	return ws.engine.Run(addr)
}
