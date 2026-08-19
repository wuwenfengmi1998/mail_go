package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSecurityHeaders 验证所有基础安全响应头都存在。
func TestSecurityHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(SecurityHeaders())
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.ServeHTTP(w, req)

	for _, h := range []string{
		"Strict-Transport-Security",
		"X-Frame-Options",
		"X-Content-Type-Options",
		"Referrer-Policy",
		"Content-Security-Policy",
	} {
		if v := w.Header().Get(h); v == "" {
			t.Errorf("missing security header %q", h)
		}
	}

	// 关键头内容抽查
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("CSP should include frame-ancestors 'none', got %q", got)
	}
	if got := w.Header().Get("Content-Security-Policy"); !strings.Contains(got, "connect-src 'self'") {
		t.Errorf("CSP should include connect-src 'self', got %q", got)
	}
}
