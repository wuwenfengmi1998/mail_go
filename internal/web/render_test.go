package web

// Temporary render test used to validate the rewritten frontend templates.
// Renders every page template with realistic dummy data and writes the
// output to /tmp/mailgo_preview for visual verification.

import (
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mail_go/internal/db"
)

func TestRenderAllPages(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	tmpl := template.Must(template.New("").Funcs(templateFuncs()).ParseGlob(filepath.Join(wd, "templates", "*.html")))
	template.Must(tmpl.ParseGlob(filepath.Join(wd, "templates", "admin", "*.html")))

	now := time.Now()
	user := &db.User{
		ID:         1,
		Username:   "admin",
		IsAdmin:    true,
		Domain:     db.Domain{Name: "lmve.net"},
		UsedBytes:  5 * 1024 * 1024,
		QuotaBytes: 5 * 1024 * 1024 * 1024,
	}

	messages := []db.Message{
		{ID: 1, Folder: "INBOX", FromAddr: "=?UTF-8?B?5byg5LiJ?= <zhangsan@lmve.net>", ToAddr: "admin@lmve.net", Subject: "邮件系统部署完成通知", TextBody: "您好！您的 MailGo 邮件系统已成功部署，本邮件为测试邮件。", Date: now, IsRead: false},
		{ID: 2, Folder: "INBOX", FromAddr: "alice@example.com", ToAddr: "admin@lmve.net", Subject: "Re: 项目进度同步", TextBody: "好的，我们下周一上午十点开会同步一下进度。", Date: now.Add(-3 * time.Hour), IsRead: true},
		{ID: 3, Folder: "INBOX", FromAddr: "=?UTF-8?B?6ZmI5rKz?= <wangwu@lmve.net>", ToAddr: "admin@lmve.net", Subject: "服务器巡检报告（8 月）", TextBody: "本月巡检完成，磁盘使用率 62%，内存使用正常。", Date: now.Add(-48 * time.Hour), IsRead: false},
		{ID: 4, Folder: "INBOX", FromAddr: "bob@other.com", ToAddr: "admin@lmve.net", Subject: "Newsletter #42", TextBody: "这是本周的资讯摘要，共 5 篇文章。", Date: now.Add(-10 * 24 * time.Hour), IsRead: true},
		{ID: 5, Folder: "INBOX", FromAddr: "=?UTF-8?B?6ZmI5rKz?= <wangwu@lmve.net>", ToAddr: "admin@lmve.net", Subject: "DNS 记录更新", TextBody: "已按文档更新 SPF 与 DKIM 记录，请验证。", Date: now.Add(-100 * 24 * time.Hour), IsRead: true},
	}

	attachments := []db.Attachment{
		{ID: 1, FileName: "部署文档.pdf", FileSize: 1024 * 1024},
		{ID: 2, FileName: "logo.png", FileSize: 128 * 1024},
	}

	cases := []struct {
		name string
		data ginH
	}{
		{"login", ginH{"error": ""}},
		{"banned", ginH{"entry": &db.BanEntry{IPAddress: "1.2.3.4", Reason: "登录失败次数过多", FailCount: 8, ExpiresAt: now.Add(20 * time.Minute)}}},
		{"inbox", ginH{"currentUser": user, "messages": messages, "total": 5, "page": 1, "totalPages": 1, "activeFolder": "inbox", "inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3)}},
		{"drafts", ginH{"currentUser": user, "messages": messages, "total": 1, "page": 1, "totalPages": 1, "activeFolder": "drafts", "inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3)}},
		{"sent", ginH{"currentUser": user, "messages": messages, "total": 3, "page": 1, "totalPages": 1, "activeFolder": "sent", "inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3)}},
		{"view", ginH{
			"currentUser": user, "activeFolder": "inbox",
			"message":     &db.Message{ID: 1, Folder: "INBOX", FromAddr: "=?UTF-8?B?5byg5LiJ?= <zhangsan@lmve.net>", ToAddr: "admin@lmve.net", Subject: "邮件系统部署完成通知", TextBody: "您好！您的 MailGo 邮件系统已成功部署。", HtmlBody: "", Date: now, IsRead: false},
			"attachments": attachments, "inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3),
		}},
		{"compose", ginH{
			"currentUser": user, "activeFolder": "compose", "error": "",
			"to": "zhangsan@lmve.net", "subject": "Re: 邮件系统部署完成通知", "bodyContent": "",
			"usedBytes": int64(5 * 1024 * 1024), "quotaBytes": int64(5 * 1024 * 1024 * 1024),
			"inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3),
		}},
		{"settings", ginH{"currentUser": user, "activeFolder": "settings", "error": "", "success": "", "inboxUnread": int64(2), "draftsTotal": int64(1), "sentTotal": int64(3)}},
		{"admin_dashboard", ginH{"currentUser": user, "activeFolder": "admin", "domainCount": 2, "userCount": 5, "totalMails": 100, "banCount": 1, "inboxCount": 50, "sentCount": 30, "draftsCount": 10, "trashCount": 5, "inboxSize": int64(1024), "sentSize": int64(512), "totalSize": int64(2048), "todayReceived": 3, "todaySent": 2, "weekReceived": 20, "weekSent": 15}},
		{"admin_bans", ginH{
			"currentUser": user, "activeFolder": "bans",
			"rows": []struct {
				db.BanEntry
				Active bool
			}{
				{BanEntry: db.BanEntry{IPAddress: "203.0.113.7", BanCount: 4, FailCount: 5, Reason: "第1次封禁：登录失败次数过多（第4次触发，失败5次）", ExpiresAt: now.Add(20 * time.Minute)}, Active: true},
				{BanEntry: db.BanEntry{IPAddress: "203.0.113.9", BanCount: 5, FailCount: 6, Reason: "第2次封禁：邮件协议认证失败次数过多（第5次触发，失败6次）", ExpiresAt: now.Add(-24 * time.Hour)}, Active: false},
				{BanEntry: db.BanEntry{IPAddress: "10.0.0.2", BanCount: 1, FailCount: 5, Reason: "", ExpiresAt: time.Time{}}, Active: false},
			},
			"total": 3, "page": 1, "pageSize": 20, "totalPages": 1,
		}},
		{"admin_protocol_logs", ginH{
			"currentUser": user, "activeFolder": "protocol-logs",
			"logs": []db.ProtocolLog{
				{ID: 1, Protocol: db.ProtocolSMTP, Port: 25, ClientIP: "203.0.113.7", Username: "", Success: true, FailReason: "", Detail: "MAIL FROM:<spam@evil.example> RCPT×1 本地投递1", MsgCount: 1, DurationMs: 1234, CreatedAt: now},
				{ID: 2, Protocol: db.ProtocolIMAP, Port: 993, ClientIP: "203.0.113.9", Username: "admin", Success: false, FailReason: "用户名或密码错误", Detail: "LOGIN 失败", DurationMs: 88, CreatedAt: now.Add(-time.Minute)},
				{ID: 3, Protocol: db.ProtocolPOP3, Port: 110, ClientIP: "10.0.0.2", Username: "alice", Success: true, FailReason: "", Detail: "USER PASS STAT RETR×3 QUIT", MsgCount: 3, DurationMs: 500, CreatedAt: now.Add(-2 * time.Minute)},
			},
			"total": 3, "page": 1, "pageSize": 50, "totalPages": 1,
			"filter": map[string]string{"protocol": "smtp", "success": "fail", "ip": "203.0.113", "username": "", "from": "2026-08-01", "to": "2026-08-19"},
			"todayStats": map[string]map[string]int{
				db.ProtocolSMTP: {"success": 10, "fail": 2},
				db.ProtocolIMAP: {"success": 5, "fail": 7},
				db.ProtocolPOP3: {"success": 3, "fail": 4},
			},
			"allStats": map[string]map[string]int{
				db.ProtocolSMTP: {"success": 100, "fail": 20},
				db.ProtocolIMAP: {"success": 50, "fail": 70},
				db.ProtocolPOP3: {"success": 30, "fail": 40},
			},
			"keepDays": 30,
		}},
		{"admin_connections", ginH{
			"currentUser": user, "activeFolder": "connections",
			"conns": []struct {
				ID         uint64
				Protocol   string
				IP         string
				Port       int
				User       string
				TLS        bool
				Connected  time.Time
				LastActive time.Time
			}{
				{ID: 1, Protocol: "smtp", IP: "203.0.113.7", Port: 25, TLS: true, Connected: now.Add(-2 * time.Minute), LastActive: now},
				{ID: 2, Protocol: "imap", IP: "203.0.113.9", Port: 993, User: "admin", TLS: true, Connected: now.Add(-30 * time.Minute), LastActive: now.Add(-10 * time.Second)},
				{ID: 3, Protocol: "pop3", IP: "10.0.0.2", Port: 110, User: "alice", TLS: false, Connected: now.Add(-time.Minute), LastActive: now.Add(-30 * time.Second)},
			},
			"total": 3, "smtpCount": 1, "imapCount": 1, "pop3Count": 1, "now": now,
		}},
	}

	outDir := os.Getenv("MAILGO_PREVIEW_DIR")
	if outDir == "" {
		outDir = filepath.Join(os.TempDir(), "mailgo_preview")
	}
	os.MkdirAll(outDir, 0755)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			if err := tmpl.ExecuteTemplate(&buf, tc.name, tc.data); err != nil {
				t.Fatalf("render %s: %v", tc.name, err)
			}
			os.WriteFile(filepath.Join(outDir, tc.name+".html"), []byte(buf.String()), 0644)
			t.Logf("%s -> %d bytes", tc.name, buf.Len())
		})
	}
}

// ginH mimics gin.H so the test does not need the gin dependency surface.
type ginH map[string]interface{}
