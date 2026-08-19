package handlers

// P1 #2 回归测试：OAuth2 state 必须随机、回调必须校验。
// 旧实现 state 为硬编码常量且回调完全不校验（登录 CSRF / 授权码注入）。

import (
	"encoding/json"
	"html/template"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/mailutil"

	"github.com/gin-gonic/gin"
)

// testTemplateFuncs 提供模板解析所需的自定义函数（与 web 包的
// templateFuncs 等价，但 handlers 包无法反向依赖 web 包）。
func testTemplateFuncs() template.FuncMap {
	return template.FuncMap{
		"add":        func(a, b int) int { return a + b },
		"sub":        func(a, b int) int { return a - b },
		"mul":        func(a, b int) int { return a * b },
		"div":        func(a, b int) int { return a / b },
		"mod":        func(a, b int) int { return a % b },
		"ceilDiv":    func(a, b int) int { return int(math.Ceil(float64(a) / float64(b))) },
		"seq":        func(n int) []int { r := make([]int, n); for i := range r { r[i] = i + 1 }; return r },
		"domainName": func(domainID uint, domains []interface{}) string { return "Domain #1" },
		"jsonify": func(v interface{}) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"formatBytes": func(b int64) string {
			return "1 KB"
		},
		"decodeHeader": mailutil.DecodeRFC2047,
		"mailName":     func(s string) string { return s },
		"mailEmail":    func(s string) string { return s },
		"initial":      func(s string) string { return "?" },
		"truncate":     func(s string, n int) string { return s },
		"shortDate":    func(t time.Time) string { return t.Format("2006-01-02") },
		"localTime":    func(t time.Time) time.Time { return t },
		"time12":       func(t time.Time) string { return t.Format("2006-01-02 15:04:05") },
		"time12m":      func(t time.Time) string { return t.Format("2006-01-02 15:04") },
		"avatarStyle":  func(s string) string { return "background:#eee;color:#333" },
		"urlPath":      func(s string) string { return url.PathEscape(s) },
		"folderLabel":  func(s string) string { return s },
	}
}

func newOAuth2TestContext(t *testing.T) (*gin.Context, *AuthHandler, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, engine := gin.CreateTestContext(w)
	// 回调的错误分支渲染 login 模板，需要加载模板及自定义函数
	tmpl := template.Must(template.New("").Funcs(testTemplateFuncs()).ParseGlob(filepath.Join("..", "templates", "*.html")))
	engine.SetHTMLTemplate(tmpl)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth/oauth2", nil)

	authCfg := config.AuthConfig{
		OAuth2Enabled:      true,
		// 使用本地拒绝连接的地址作为 provider，token 交换快速失败，
		// 测试不依赖外部网络。
		OAuth2Provider:     "127.0.0.1:1",
		OAuth2ClientID:     "test-client-id",
		OAuth2ClientSecret: "test-client-secret",
		OAuth2RedirectURL:  "https://mail.example.com/auth/oauth2/callback",
	}
	h := NewAuthHandler(nil, authCfg, config.BanConfig{MaxFailAttempts: 100})
	return c, h, w
}

func TestRandomOAuth2State(t *testing.T) {
	s1, err := randomOAuth2State()
	if err != nil {
		t.Fatalf("randomOAuth2State() error: %v", err)
	}
	if len(s1) != oauth2StateRandLen*2 {
		t.Fatalf("state length = %d, want %d (hex)", len(s1), oauth2StateRandLen*2)
	}
	s2, _ := randomOAuth2State()
	if s1 == s2 {
		t.Fatal("state must be unique per request")
	}
	if s1 == "mailgo_oauth2_state" {
		t.Fatal("state must not be the old hardcoded constant")
	}
}

func TestOAuth2StartSetsRandomStateCookie(t *testing.T) {
	c, h, w := newOAuth2TestContext(t)
	h.OAuth2Start(c)

	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "state=") {
		t.Fatalf("redirect URL should carry state: %s", loc)
	}

	// state cookie 必须存在且与 URL 中的一致
	cookies := w.Result().Cookies()
	var stateVal string
	found := false
	for _, ck := range cookies {
		if ck.Name == oauth2StateCookie {
			found = true
			stateVal = ck.Value
			if !ck.HttpOnly {
				t.Error("state cookie must be HttpOnly")
			}
			if !ck.Secure {
				t.Error("state cookie must be Secure")
			}
			if ck.MaxAge <= 0 || ck.MaxAge > oauth2StateMaxAge {
				t.Errorf("state cookie MaxAge = %d, want in (0, %d]", ck.MaxAge, oauth2StateMaxAge)
			}
		}
	}
	if !found {
		t.Fatal("OAuth2Start should set state cookie")
	}

	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if u.Query().Get("state") != stateVal {
		t.Fatalf("cookie state %q != URL state %q", stateVal, u.Query().Get("state"))
	}

	// 两次发起的 state 不同
	c2, h2, w2 := newOAuth2TestContext(t)
	h2.OAuth2Start(c2)
	u2, _ := url.Parse(w2.Header().Get("Location"))
	if u2.Query().Get("state") == stateVal {
		t.Fatal("state must differ between sessions")
	}
}

func TestOAuth2CallbackRejectsMissingOrMismatchedState(t *testing.T) {
	cases := []struct {
		name        string
		cookieState string
		queryState  string
	}{
		{"no cookie", "", "abc"},
		{"no query state", "abc", ""},
		{"mismatch", "abc", "xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, h, w := newOAuth2TestContext(t)
			q := url.Values{}
			q.Set("code", "test-code")
			if tc.queryState != "" {
				q.Set("state", tc.queryState)
			}
			c.Request = httptest.NewRequest(http.MethodGet, "/auth/oauth2/callback?"+q.Encode(), nil)
			if tc.cookieState != "" {
				c.Request.AddCookie(&http.Cookie{Name: oauth2StateCookie, Value: tc.cookieState})
			}
			h.OAuth2Callback(c)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
		})
	}
}

func TestOAuth2CallbackAcceptsValidState(t *testing.T) {
	// 模拟完整流程：Start 下发 state -> Callback 带回同一 state。
	// state 校验通过后应进入后续流程（本测试无真实 IdP，
	// code 交换会失败并渲染登录错误页，但这证明 state 关卡已通过）。
	c, h, w := newOAuth2TestContext(t)
	h.OAuth2Start(c)
	var stateVal string
	for _, ck := range w.Result().Cookies() {
		if ck.Name == oauth2StateCookie {
			stateVal = ck.Value
		}
	}

	w2 := httptest.NewRecorder()
	c2, engine2 := gin.CreateTestContext(w2)
	tmpl2 := template.Must(template.New("").Funcs(testTemplateFuncs()).ParseGlob(filepath.Join("..", "templates", "*.html")))
	engine2.SetHTMLTemplate(tmpl2)
	q := url.Values{}
	q.Set("code", "test-code")
	q.Set("state", stateVal)
	c2.Request = httptest.NewRequest(http.MethodGet, "/auth/oauth2/callback?"+q.Encode(), nil)
	c2.Request.AddCookie(&http.Cookie{Name: oauth2StateCookie, Value: stateVal})

	h.OAuth2Callback(c2)

	// state 校验失败返回 403；此处应为非 403（进入 token 交换失败分支）
	if w2.Code == http.StatusForbidden {
		t.Fatalf("valid state was rejected")
	}
	if !strings.Contains(w2.Body.String(), "OAuth2") {
		body := w2.Body.String()
		if len(body) > 200 {
			body = body[:200]
		}
		t.Fatalf("expected OAuth2 error page after state check passed, body: %s", body)
	}
}
