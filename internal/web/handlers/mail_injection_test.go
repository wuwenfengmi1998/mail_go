package handlers

// P1 #4 回归测试：Web 写信的邮件头不可被 CRLF 注入。
// 旧实现把 to/cc/subject/附件名原样拼进 MIME 头，攻击者可通过
// subject 注入 Reply-To/Bcc 等任意头用于钓鱼。

import (
	"strings"
	"testing"
)

func TestSanitizeHeaderField(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"normal value", "normal value"},
		{"with\r\ninjected: header", "withinjected: header"},
		{"lf\nonly", "lfonly"},
		{"cr\ronly", "cronly"},
		{"nul\x00byte", "nulbyte"},
		{"mixed\r\n\x00all", "mixedall"},
		{"中文主题", "中文主题"},
	}
	for _, tc := range cases {
		if got := sanitizeHeaderField(tc.in); got != tc.want {
			t.Errorf("sanitizeHeaderField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildOutgoingMessageBlocksHeaderInjection(t *testing.T) {
	_, raw := buildOutgoingMessage(
		"alice@example.com",
		"bob@example.com\r\nBcc: victim@evil.com",
		"carol@example.com\r\nReply-To: attacker@evil.com",
		"Hi\r\nBcc: victim@evil.com\r\nReply-To: attacker@evil.com",
		"body",
		"",
		nil,
	)

	// 注入的头不允许以独立头形式出现
	for _, injected := range []string{
		"Bcc:", "Reply-To:",
	} {
		if strings.Contains(raw, "\r\n"+injected) || strings.HasPrefix(raw, injected) {
			t.Fatalf("injected header %q found in message:\n%s", injected, raw)
		}
	}

	// 注入的邮箱地址本身允许以折叠形式残留在原头值中，
	// 但绝不能成为独立的一行头。
	lines := strings.Split(raw, "\r\n")
	for _, line := range lines[1:] { // 跳过 From
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Bcc:") || strings.HasPrefix(trimmed, "Reply-To:") {
			t.Fatalf("injected header line %q found in message:\n%s", line, raw)
		}
	}
}

func TestBuildOutgoingMessageAttFilenameInjection(t *testing.T) {
	atts := []pendingAttachment{
		{filename: "evil.png\r\nBcc: victim@evil.com", contentType: "image/png", data: []byte("x")},
		{filename: `quote".png`, contentType: "image/png", data: []byte("x")},
	}
	_, raw := buildOutgoingMessage("a@b.com", "c@d.com", "", "t", "body", "", atts)

	lines := strings.Split(raw, "\r\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Bcc:") {
			t.Fatalf("filename header injection found: %q\nmessage:\n%s", line, raw)
		}
	}

	// 含引号/换行的文件名必须被正确编码，不能破坏头结构
	if !strings.Contains(raw, "Content-Disposition: attachment;") {
		t.Fatalf("Content-Disposition missing in message:\n%s", raw)
	}
}

func TestBuildOutgoingMessageEncodesNonASCIISubject(t *testing.T) {
	_, raw := buildOutgoingMessage("a@b.com", "c@d.com", "", "中文主题测试", "body", "", nil)
	// 非 ASCII 主题应按 RFC 2047 编码为 =?utf-8?...?= 形式
	if !strings.Contains(raw, "Subject: =?utf-8?") && !strings.Contains(raw, "Subject: =?UTF-8?") {
		t.Fatalf("non-ASCII subject should be RFC 2047 encoded, got:\n%s", raw)
	}
	// 头部不应再包含裸中文（应被编码）
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "Subject:") && strings.ContainsAny(line, "中文测试") {
			t.Fatalf("raw non-ASCII in Subject header: %q", line)
		}
	}
}

func TestFormatContentDisposition(t *testing.T) {
	if got := formatContentDisposition("report.pdf"); got != "attachment; filename=report.pdf" {
		t.Fatalf("simple filename: got %q", got)
	}
	// 特殊字符需要安全编码而不是原样嵌入
	got := formatContentDisposition("a\"b\\c\r\nd.png")
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("CRLF leaked into Content-Disposition: %q", got)
	}
}
