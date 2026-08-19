package web

// P1 #3 回归测试：客户端 IP 不可通过 X-Forwarded-For 伪造。
// 外部直连时伪造头必须被忽略（防绕过登录封禁/恶意封禁他人），
// 本机回环（反向代理）转发时必须取 X-Forwarded-For 中的真实客户端 IP。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doLoginFailure 触发一次登录失败（使 BanStore 按 ClientIP 记录失败计数），
// 返回使用的请求。
func doLoginFailure(t *testing.T, ws *WebServer, remoteAddr, xff string) {
	t.Helper()
	form := strings.NewReader("email=nobody@example.com&password=wrong")
	req := httptest.NewRequest(http.MethodPost, "/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	ws.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK { // 登录失败重渲染登录页
		t.Fatalf("login failure status = %d, want 200", w.Code)
	}
}

func TestExternalClientIPCannotBeSpoofed(t *testing.T) {
	ws, stores := newTestWebServer(t, "0123456789abcdef0123456789abcdef")

	// 模拟外部攻击者直连 8080 端口，伪造 X-Forwarded-For
	doLoginFailure(t, ws, "203.0.113.99:5555", "1.2.3.4")

	// 失败计数必须记在真实来源 IP 上
	if _, err := stores.Bans.GetByIP("1.2.3.4"); err == nil {
		t.Fatal("spoofed X-Forwarded-For IP must not be recorded")
	}
	entry, err := stores.Bans.GetByIP("203.0.113.99")
	if err != nil {
		t.Fatalf("real client IP should be recorded: %v", err)
	}
	if entry.FailCount != 1 {
		t.Fatalf("fail count = %d, want 1", entry.FailCount)
	}
}

func TestLoopbackProxyXFFIsHonored(t *testing.T) {
	ws, stores := newTestWebServer(t, "0123456789abcdef0123456789abcdef")

	// 模拟本机 Caddy/Nginx 转发：RemoteAddr 是回环，XFF 是真实客户端
	doLoginFailure(t, ws, "127.0.0.1:5555", "198.51.100.7")

	entry, err := stores.Bans.GetByIP("198.51.100.7")
	if err != nil {
		t.Fatalf("proxied client IP should be recorded: %v", err)
	}
	if entry.FailCount != 1 {
		t.Fatalf("fail count = %d, want 1", entry.FailCount)
	}
	if _, err := stores.Bans.GetByIP("127.0.0.1"); err == nil {
		t.Fatal("proxy's own IP should not be recorded")
	}
}
