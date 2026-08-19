package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/imap_server"
	"mail_go/internal/mailutil"
	"mail_go/internal/outbound"
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
	engine       *gin.Engine
	stores       *store.Stores
	storage      *storage.AttachmentStorage
	cfg          config.WebConfig
	storageCfg   config.StorageConfig
	authCfg      config.AuthConfig
	banCfg       config.BanConfig
	caddyDataDir string
	outbound     *outbound.Manager
	hub          *connhub.Hub
	// pusher 邮件状态变化推送（IMAP 客户端实时同步），可空
	pusher imap_server.Pusher
}

// templateFuncs returns custom template functions for rendering.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
		"mul": func(a, b int) int { return a * b },
		"div": func(a, b int64) int64 { return a / b },
		// durationSeconds 将 time.Duration 转为整秒（模板中无法做类型转换）。
		"durationSeconds": func(d time.Duration) int64 { return int64(d / time.Second) },
		"mod":             func(a, b int) int { return a % b },
		"ceilDiv":         func(a, b int) int { return int(math.Ceil(float64(a) / float64(b))) },
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
		// jsonify 把任意值序列化为安全的 JS 字面量（JSON 字符串），
		// 用于在 <script> 上下文中注入数据。encoding/json 默认转义
		// < > &（\u003c 等），无法逃出 </script>，杜绝 script 注入。
		"jsonify": func(v interface{}) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return template.JS("null")
			}
			return template.JS(b)
		},
		"formatBytes": func(b int64) string {
			return formatBytes(b)
		},
		"decodeHeader": func(s string) string {
			return mailutil.DecodeRFC2047(s)
		},
		// mailName 从 "Name <addr>" 中提取显示名；无显示名时退回邮箱地址。
		"mailName": mailName,
		// mailEmail 从 "Name <addr>" 中提取邮箱地址部分。
		"mailEmail": mailEmail,
		// initial 返回字符串的首字符（用于头像占位）。
		"initial": initial,
		// truncate 折叠空白并截断到 n 个字符（用于列表摘要）。
		"truncate": truncate,
		// shortDate 按 QQ 邮箱习惯格式化：今天显示 HH:mm，今年显示 MM-DD，更早显示 YYYY-MM-DD。
		"shortDate": shortDate,
		// localTime 把存储的 UTC 时间转换为 Web 配置时区（默认 Asia/Shanghai）。
		"localTime": localTime,
		// avatarStyle 根据字符串哈希生成头像背景/前景色。
		"avatarStyle": avatarStyle,
	}
}

// mailName extracts the display name from an RFC 5322 address.
func mailName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		name := strings.Trim(strings.TrimSpace(s[:i]), `"' `)
		if name != "" {
			return name
		}
		if j := strings.IndexByte(s, '>'); j > i {
			return s[i+1 : j]
		}
	}
	return s
}

// mailEmail extracts the bare email address from an RFC 5322 address.
func mailEmail(s string) string {
	if i := strings.IndexByte(s, '<'); i >= 0 {
		if j := strings.IndexByte(s, '>'); j > i {
			return s[i+1 : j]
		}
	}
	return strings.TrimSpace(s)
}

// initial returns the first rune of a string, upper-cased.
func initial(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "?"
	}
	r, _ := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r))
}

// truncate collapses whitespace and cuts the string to n runes.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// shortDate formats a time like QQ Mail does: today -> HH:mm,
// this year -> MM-DD, otherwise -> YYYY-MM-DD.
// 时间先按 Web 配置时区转换（库内为 UTC），"今天"判断也使用该时区。
func shortDate(t time.Time) string {
	t = inWebTZ(t)
	now := time.Now().In(t.Location())
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("01-02")
	}
	return t.Format("2006-01-02")
}

// webTZ 是 Web 界面显示时间使用的时区（默认 Asia/Shanghai）。
var webTZ = time.Local

// fixedTimezone 解析 "+08:00"/"UTC+8" 形式的固定偏移时区；解析失败返回 nil。
func fixedTimezone(s string) *time.Location {
	s = strings.TrimSpace(s)
	sign := 1
	rest := s
	if strings.HasPrefix(rest, "+") {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, "-") {
		sign = -1
		rest = rest[1:]
	}
	if strings.HasPrefix(strings.ToUpper(rest), "UTC") {
		rest = strings.TrimSpace(rest[3:])
	}
	parts := strings.SplitN(rest, ":", 2)
	h, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || h < 0 || h > 23 {
		return nil
	}
	m := 0
	if len(parts) == 2 {
		if m, err = strconv.Atoi(strings.TrimSpace(parts[1])); err != nil || m < 0 || m > 59 {
			return nil
		}
	}
	offset := sign * (h*3600 + m*60)
	return time.FixedZone("UTC"+strconv.Itoa(offset/3600), offset)
}

// inWebTZ 把时间转换到 Web 展示时区。
func inWebTZ(t time.Time) time.Time {
	return t.In(webTZ)
}

// localTime 是 localTime 模板函数的实现（转换到 Web 展示时区）。
func localTime(t time.Time) time.Time {
	return t.In(webTZ)
}

// avatarStyle returns inline CSS colors derived from a string hash.
func avatarStyle(s string) string {
	h := 0
	for _, r := range s {
		h = (h*31 + int(r)) % 360
	}
	return fmt.Sprintf("background:hsl(%d,78%%,92%%);color:hsl(%d,72%%,36%%)", h, h)
}

// NewWebServer creates a new WebServer, initializes the Gin engine,
// configures sessions, middleware, and registers all routes.
func NewWebServer(cfg config.WebConfig, stores *store.Stores, attStorage *storage.AttachmentStorage, storageCfg config.StorageConfig, authCfg config.AuthConfig, banCfg config.BanConfig, caddyCfg config.CaddyConfig, ob *outbound.Manager, hub *connhub.Hub, pusher imap_server.Pusher) (*WebServer, error) {
	if err := config.ValidateSecretKey(cfg.SecretKey); err != nil {
		return nil, err
	}

	// Web 展示时区：邮件日期库内统一 UTC 存储，界面按配置时区显示。
	if cfg.Timezone != "" {
		if loc, err := time.LoadLocation(cfg.Timezone); err == nil {
			webTZ = loc
		} else if loc2 := fixedTimezone(cfg.Timezone); loc2 != nil {
			webTZ = loc2
		} else {
			return nil, fmt.Errorf("无效的 Web 时区配置 %q: %v", cfg.Timezone, err)
		}
	}

	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	engine.Use(gin.Logger())
	engine.Use(gin.Recovery())

	// 仅信任本机回环上的反向代理（Caddy/Nginx）。外部直连时
	// X-Forwarded-For 不可信，防止伪造客户端 IP 绕过登录封禁或
	// 恶意封禁他人 IP。gin 对 Unix socket 监听无条件信任转发头，
	// 因此 socket 必须保持仅本机可达。
	if err := engine.SetTrustedProxies([]string{"127.0.0.1", "::1"}); err != nil {
		return nil, fmt.Errorf("设置可信代理失败: %w", err)
	}

	// Session store (cookie-based). The signing key comes from the config
	// file (auto-generated random key) or the MAILGO_SECRET_KEY env var.
	cookieStore := cookie.NewStore([]byte(cfg.SecretKey))
	cookieStore.Options(sessions.Options{
		HttpOnly: true,
		SameSite: 3, // SameSiteStrictMode（比 Lax 更严格）
		Secure:   cfg.CookieSecure,
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
		engine:       engine,
		stores:       stores,
		storage:      attStorage,
		cfg:          cfg,
		storageCfg:   storageCfg,
		authCfg:      authCfg,
		banCfg:       banCfg,
		caddyDataDir: caddyCfg.DataDir,
		outbound:     ob,
		hub:          hub,
		pusher:       pusher,
	}

	ws.registerRoutes()
	return ws, nil
}

// registerRoutes sets up all HTTP routes with their handlers and middleware.
func (ws *WebServer) registerRoutes() {
	authHandler := handlers.NewAuthHandler(ws.stores, ws.authCfg, ws.banCfg)
	mailHandler := handlers.NewMailHandler(ws.stores, ws.storage, ws.outbound, ws.pusher)
	adminHandler := handlers.NewAdminHandler(ws.stores, ws.storage, filepath.Join(ws.storageCfg.BaseDir, "tls", "domains"), ws.caddyDataDir, ws.outbound, ws.cfg.ProtocolLogKeepDays, ws.hub)

	// Apply BanMiddleware globally before public routes
	ws.engine.Use(middleware.BanMiddleware(ws.stores))
	// Security headers on every response
	ws.engine.Use(middleware.SecurityHeaders())

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
		admin.POST("/domains/:id/fetch-caddy-cert", adminHandler.FetchCaddyCert)
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
		admin.GET("/outbound", adminHandler.ListOutbound)
		admin.POST("/outbound/:id/retry", adminHandler.RetryOutbound)
		admin.POST("/outbound/:id/cancel", adminHandler.CancelOutbound)
		admin.GET("/bans", adminHandler.ListBans)
		admin.POST("/bans/:id/unban", adminHandler.UnbanIP)
		admin.GET("/protocol-logs", adminHandler.ListProtocolLogs)
		admin.POST("/protocol-logs/cleanup", adminHandler.CleanupProtocolLogs)
		admin.GET("/connections", adminHandler.ListConnections)
		admin.POST("/connections/:id/disconnect", adminHandler.DisconnectConnection)
	}
}

// Handler returns the underlying Gin engine as an http.Handler, useful for
// integration tests and for embedding behind a reverse proxy.
func (ws *WebServer) Handler() http.Handler {
	return ws.engine
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
