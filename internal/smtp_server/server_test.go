package smtp_server

import (
	"bytes"
	"fmt"
	"testing"

	"mail_go/internal/db"
	"mail_go/internal/storage"
	"mail_go/internal/store"

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
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}); err != nil {
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
