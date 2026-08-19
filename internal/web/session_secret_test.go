package web

// P0 回归测试：验证会话 cookie 由配置中的 secret_key 签名，
// 且旧版硬编码密钥（源码公开，视为已泄露）无法再伪造有效会话。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/gorilla/securecookie"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func chdirRepoRoot(t *testing.T) {
	t.Helper()
	// NewWebServer 以相对路径加载 internal/web/templates/，
	// 测试进程的 CWD 是 internal/web，需要切到仓库根目录。
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		t.Fatalf("chdir to repo root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(filepath.Join("internal", "web")) })
}

func newTestStores(t *testing.T) *store.Stores {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.NewStores(gdb)
}

func newTestWebServer(t *testing.T, secretKey string) (*WebServer, *store.Stores) {
	t.Helper()
	chdirRepoRoot(t)

	stores := newTestStores(t)

	domain := &db.Domain{Name: "example.com", SmtpPort: 25, ImapPort: 143, Pop3Port: 110}
	if err := stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("test-password-123"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := stores.Users.Create(&db.User{
		Username:     "alice",
		PasswordHash: string(hash),
		DomainID:     domain.ID,
		IsActive:     true,
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	baseDir := t.TempDir()
	attStorage := storage.NewAttachmentStorage(filepath.Join(baseDir, "attachments"))
	cfg := config.WebConfig{Addr: "127.0.0.1:0", SecretKey: secretKey, CookieSecure: true}

	ws, err := NewWebServer(cfg, stores, attStorage, config.StorageConfig{BaseDir: baseDir},
		config.AuthConfig{}, config.BanConfig{MaxFailAttempts: 100}, config.CaddyConfig{}, nil, connhub.New(), nil)
	if err != nil {
		t.Fatalf("NewWebServer: %v", err)
	}
	return ws, stores
}

func TestSessionSignedWithConfiguredSecretKey(t *testing.T) {
	ws, _ := newTestWebServer(t, "0123456789abcdef0123456789abcdef")
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	// 登录成功 -> 返回会话 cookie（禁用自动重定向以获取原始 302 响应）
	form := url.Values{"email": {"alice@example.com"}, "password": {"test-password-123"}}
	loginReq, _ := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(loginReq)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d, want 302", resp.StatusCode)
	}
	var sessionCookie string
	for _, c := range resp.Cookies() {
		if c.Name == "mail_go_session" {
			sessionCookie = c.Value
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if !c.Secure {
				t.Error("session cookie must be Secure")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Errorf("session cookie SameSite = %v, want Strict", c.SameSite)
			}
		}
	}
	if sessionCookie == "" {
		t.Fatal("login should set mail_go_session cookie")
	}

	// 合法会话可以访问收件箱
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/inbox", nil)
	req.AddCookie(&http.Cookie{Name: "mail_go_session", Value: sessionCookie})
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatalf("inbox request: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("inbox with valid session: status = %d, want 200", resp2.StatusCode)
	}
}

func TestLegacyHardcodedKeyCannotForgeSession(t *testing.T) {
	// 服务端使用随机生成的新密钥
	ws, _ := newTestWebServer(t, "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c5b4a39281706f5e4d3c2b1a0")
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	// 攻击者用旧硬编码密钥（源码中公开）伪造管理员会话
	forger := securecookie.New([]byte(config.InsecureLegacySecretKey), nil)
	forged, err := forger.Encode("mail_go_session", map[interface{}]interface{}{
		"userID":    uint(1),
		"userEmail": "admin@example.com",
		"isAdmin":   true,
	})
	if err != nil {
		t.Fatalf("forge cookie: %v", err)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/inbox", nil)
	req.AddCookie(&http.Cookie{Name: "mail_go_session", Value: forged})
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request with forged cookie: %v", err)
	}
	defer resp.Body.Close()

	// 签名校验失败 -> 未认证，必须被重定向到登录页
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("forged legacy-key session must be rejected: status = %d, want 302 redirect to /login", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("forged session should redirect to /login, got Location: %q", loc)
	}
}

func TestNewWebServerRejectsBadSecretKeys(t *testing.T) {
	chdirRepoRoot(t)
	stores := newTestStores(t)
	baseDir := t.TempDir()
	attStorage := storage.NewAttachmentStorage(filepath.Join(baseDir, "attachments"))

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"legacy default", config.InsecureLegacySecretKey},
		{"too short", "short-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewWebServer(config.WebConfig{Addr: "127.0.0.1:0", SecretKey: tc.key},
				stores, attStorage, config.StorageConfig{BaseDir: baseDir},
				config.AuthConfig{}, config.BanConfig{}, config.CaddyConfig{}, nil, connhub.New(), nil)
			if err == nil {
				t.Fatalf("NewWebServer should reject secret key %q", tc.key)
			}
		})
	}
}

// encodeSessionCookie 用配置密钥伪造一个签名合法的会话 cookie。
// 仅用于测试会话治理逻辑（生产密钥不会泄露）。
func encodeSessionCookie(t *testing.T, secretKey string, values map[interface{}]interface{}) string {
	t.Helper()
	sc := securecookie.New([]byte(secretKey), nil)
	enc, err := sc.Encode("mail_go_session", values)
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	return enc
}

// authCookieValues 构造 AuthMiddleware 可识别的最小会话内容。
func authCookieValues(userID uint, loginAt int64) map[interface{}]interface{} {
	return map[interface{}]interface{}{
		"userID":    userID,
		"userEmail": "alice@example.com",
		"isAdmin":   false,
		"loginAt":   loginAt,
	}
}

// P3 #16：会话绝对过期（7 天）后强制重新登录。
func TestSessionAbsoluteExpiryForcesRelogin(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	ws, _ := newTestWebServer(t, key)
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	expired := time.Now().Add(-8 * 24 * time.Hour).Unix()
	cookie := encodeSessionCookie(t, key, authCookieValues(1, expired))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/inbox", nil)
	req.AddCookie(&http.Cookie{Name: "mail_go_session", Value: cookie})
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("expired session should redirect to /login, got %d Location=%q",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

// P3 #16：未过期会话（含滑动续期窗口内）正常访问。
func TestSessionWithinExpiryWorks(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	ws, _ := newTestWebServer(t, key)
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	cookie := encodeSessionCookie(t, key, authCookieValues(1, time.Now().Add(-time.Hour).Unix()))

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/inbox", nil)
	req.AddCookie(&http.Cookie{Name: "mail_go_session", Value: cookie})
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("fresh session should access inbox, got %d", resp.StatusCode)
	}
}
