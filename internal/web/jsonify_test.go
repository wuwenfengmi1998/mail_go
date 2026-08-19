package web

// P3 #14：jsonify 模板函数在 <script> 上下文中必须能阻止
// </script> 逃逸（encoding/json 默认转义 < > &）。

import (
	"html/template"
	"strings"
	"testing"
)

func TestJsonifyEscapesScriptBreakout(t *testing.T) {
	jsonify, ok := templateFuncs()["jsonify"].(func(interface{}) template.JS)
	if !ok {
		t.Fatal("template funcs must include jsonify")
	}

	payloads := []string{
		`x</script><script>alert(1)</script>`,
		`"><img src=x onerror=alert(1)>`,
		"line1\nline2\ttab",
		"中文内容",
		`quill "quotes" 'single'`,
	}
	for _, p := range payloads {
		out := jsonify(p)
		if !strings.HasPrefix(string(out), `"`) || !strings.HasSuffix(string(out), `"`) {
			t.Errorf("jsonify(%q) = %s, want a quoted JS string literal", p, out)
		}
		if strings.Contains(string(out), "</script>") || strings.Contains(string(out), "</SCRIPT>") {
			t.Errorf("jsonify(%q) must not emit raw </script>: %s", p, out)
		}
		if strings.ContainsAny(string(out), "\r\n") {
			t.Errorf("jsonify(%q) must escape control chars: %q", p, out)
		}
	}

	// 特殊值：nil -> null
	out := jsonify(nil)
	if string(out) != "null" {
		t.Errorf("jsonify(nil) = %s, want null", out)
	}
}
