package web

// 回归测试：写邮件页的 Quill 编辑器必须由本服务同源提供静态资源
// （CSP script-src/style-src 'self' 会拦截外部 CDN），否则正文无法输入。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuillAssetsServedLocally(t *testing.T) {
	ws, _ := newTestWebServer(t, "0123456789abcdef0123456789abcdef")
	srv := httptest.NewServer(ws.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/static/vendor/quill/quill.min.js",
		"/static/vendor/quill/quill.snow.css",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		if len(body) < 1000 {
			t.Fatalf("GET %s body too small (%d bytes), vendor file missing?", path, len(body))
		}
		if strings.HasPrefix(path, "/static/vendor/quill/quill.min.js") &&
			!strings.Contains(string(body), "Quill") {
			t.Fatalf("%s 不是 Quill 脚本", path)
		}
	}
}
