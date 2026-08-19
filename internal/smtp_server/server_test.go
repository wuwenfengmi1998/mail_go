package smtp_server

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/emersion/go-sasl"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// testMultipartMessage builds an RFC 5322 message with one text part and one
// base64 attachment.
func testMultipartMessage() []byte {
	const boundary = "X"
	return []byte(fmt.Sprintf(
		"From: sender@example.com\r\n"+
			"To: rcpt@lmve.net\r\n"+
			"Subject: with attachment\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: multipart/mixed; boundary=\"%s\"\r\n"+
			"\r\n"+
			"--%s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"hello body\r\n"+
			"--%s\r\n"+
			"Content-Type: text/plain; name=\"test.txt\"\r\n"+
			"Content-Transfer-Encoding: base64\r\n"+
			"Content-Disposition: attachment; filename=\"test.txt\"\r\n"+
			"\r\n"+
			"aGVsbG8gd29ybGQ=\r\n"+
			"--%s--\r\n",
		boundary, boundary, boundary, boundary))
}

func TestParseSMTPMessageExtractsAttachmentData(t *testing.T) {
	parsed, err := parseSMTPMessage(testMultipartMessage())
	if err != nil {
		t.Fatalf("parseSMTPMessage: %v", err)
	}
	if parsed.textBody != "hello body" {
		t.Fatalf("unexpected text body: %q", parsed.textBody)
	}
	if len(parsed.attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(parsed.attachments))
	}
	att := parsed.attachments[0]
	if att.fileName != "test.txt" {
		t.Fatalf("unexpected filename: %q", att.fileName)
	}
	if string(att.data) != "hello world" {
		t.Fatalf("unexpected attachment data: %q", att.data)
	}
}

func TestSaveMessagePersistsAttachments(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}, &db.ProtocolLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	attStorage := storage.NewAttachmentStorage(t.TempDir())
	srv := &SMTPServer{stores: stores, storage: attStorage}
	sess := &smtpSession{backend: &smtpBackend{server: srv}}

	data := testMultipartMessage()
	parsed, err := parseSMTPMessage(data)
	if err != nil {
		t.Fatalf("parseSMTPMessage: %v", err)
	}

	user := &db.User{Username: "rcpt", PasswordHash: "x", DomainID: 0, IsActive: true}
	if err := stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := sess.saveMessage(user.ID, "INBOX", parsed, data, false); err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	msgs, err := stores.Mails.ListAllByUserAndFolder(user.ID, "INBOX")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("expected 1 inbox message, got %d (err=%v)", len(msgs), err)
	}

	atts, err := stores.Attachments.ListByMessage(msgs[0].ID)
	if err != nil {
		t.Fatalf("ListByMessage: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment record, got %d", len(atts))
	}
	att := atts[0]
	if att.FileName != "test.txt" || att.FileSize != int64(len("hello world")) {
		t.Fatalf("unexpected attachment record: %+v", att)
	}

	// The file must exist on disk with the original content.
	content, err := attStorage.Read(att.FilePath)
	if err != nil {
		t.Fatalf("read attachment from disk: %v", err)
	}
	if !bytes.Equal(content, []byte("hello world")) {
		t.Fatalf("attachment content mismatch: %q", content)
	}
}

// TestSessionLoggingRecordsAuthFailure 验证认证失败的会话在 Logout 时写入协议日志。
func TestSessionLoggingRecordsAuthFailure(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}, &db.ProtocolLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	srv := &SMTPServer{stores: stores, banCfg: config.BanConfig{MaxFailAttempts: 5, BanDurationMin: 30}}
	sess := &smtpSession{
		backend:   &smtpBackend{server: srv, mode: smtpModeSubmission},
		clientIP:  "203.0.113.7",
		startedAt: time.Now(),
		port:      587,
	}

	// 触发一次认证（用户名不存在 → 失败）
	mech, err := sess.Auth(sasl.Plain)
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	// SASL PLAIN 凭据格式: authzid\0authcid\0passwd
	if _, _, err := mech.Next([]byte("\x00no-such-user\x00wrong-pass")); err == nil {
		t.Fatal("expected auth failure for unknown user")
	}

	// 直接调用 Logout 模拟连接结束
	if err := sess.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	logs, total, err := stores.ProtocolLogs.List(1, 10, store.ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 log, got %d", total)
	}
	log := logs[0]
	if log.Protocol != db.ProtocolSMTP || log.Port != 587 || log.ClientIP != "203.0.113.7" {
		t.Fatalf("unexpected log: %+v", log)
	}
	if log.Success {
		t.Fatalf("expected failure, got %+v", log)
	}
	if log.FailReason == "" {
		t.Fatal("expected fail reason")
	}
	if log.Username != "no-such-user" {
		t.Fatalf("username = %q", log.Username)
	}
}

// TestSessionLoggingRecordsDelivery 验证投递成功的会话写入成功日志。
func TestSessionLoggingRecordsDelivery(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}, &db.ProtocolLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	attStorage := storage.NewAttachmentStorage(t.TempDir())
	srv := &SMTPServer{stores: stores, storage: attStorage}
	sess := &smtpSession{
		backend:   &smtpBackend{server: srv, mode: smtpModeInbound},
		clientIP:  "203.0.113.8",
		startedAt: time.Now(),
		port:      25,
		rcpts:     make([]string, 0),
	}

	if err := sess.Mail("sender@example.com", nil); err != nil {
		t.Fatalf("Mail: %v", err)
	}
	if err := sess.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	logs, total, err := stores.ProtocolLogs.List(1, 10, store.ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 log, got %d", total)
	}
	if !logs[0].Success {
		t.Fatalf("expected success, got %+v", logs[0])
	}
}
