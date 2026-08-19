package pop3_server

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestServer(t *testing.T) *POP3Server {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}, &db.ProtocolLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)
	return &POP3Server{
		stores: stores,
		cfg:    config.POP3Config{},
		banCfg: config.BanConfig{MaxFailAttempts: 5, BanDurationMin: 30},
	}
}

// TestHandleConnLogsAuthFailure 验证认证失败的 POP3 会话写入协议日志。
func TestHandleConnLogsAuthFailure(t *testing.T) {
	s := newTestServer(t)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(server, 110)
	}()

	br := bufio.NewReader(client)
	// 等待 greeting
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	client.Write([]byte("USER no-such-user\r\n"))
	br.ReadString('\n')
	client.Write([]byte("PASS wrong-pass\r\n"))
	br.ReadString('\n')
	client.Write([]byte("QUIT\r\n"))
	br.ReadString('\n')
	client.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn did not return")
	}

	logs, total, err := s.stores.ProtocolLogs.List(1, 10, store.ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 log, got %d", total)
	}
	log := logs[0]
	if log.Protocol != db.ProtocolPOP3 || log.Port != 110 {
		t.Fatalf("unexpected log: %+v", log)
	}
	if log.Success {
		t.Fatalf("expected failure, got %+v", log)
	}
	if log.Username != "no-such-user" {
		t.Fatalf("username = %q, want no-such-user", log.Username)
	}
	if !strings.Contains(log.Detail, "USER") || !strings.Contains(log.Detail, "PASS") {
		t.Fatalf("detail missing commands: %q", log.Detail)
	}
}

// TestHandleConnLogsSuccess 验证认证成功的 POP3 会话写入成功日志。
func TestHandleConnLogsSuccess(t *testing.T) {
	s := newTestServer(t)

	domain := &db.Domain{Name: "example.com"}
	if err := s.stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	user := &db.User{Username: "alice", DomainID: domain.ID, IsActive: true}
	hashed, err := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	user.PasswordHash = string(hashed)
	if err := s.stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(server, 110)
	}()

	br := bufio.NewReader(client)
	br.ReadString('\n')
	client.Write([]byte("USER alice@example.com\r\n"))
	br.ReadString('\n')
	client.Write([]byte("PASS secret123\r\n"))
	br.ReadString('\n')
	client.Write([]byte("STAT\r\n"))
	br.ReadString('\n')
	client.Write([]byte("QUIT\r\n"))
	br.ReadString('\n')
	client.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn did not return")
	}

	logs, total, err := s.stores.ProtocolLogs.List(1, 10, store.ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("expected 1 log, got %d", total)
	}
	if !logs[0].Success {
		t.Fatalf("expected success, got %+v", logs[0])
	}
	if logs[0].Username != "alice@example.com" {
		t.Fatalf("username = %q", logs[0].Username)
	}
}

// TestPop3CommandDetail 验证命令摘要生成。
func TestPop3CommandDetail(t *testing.T) {
	counts := map[string]int{"USER": 1, "PASS": 1, "RETR": 3, "DELE": 1, "NOOP": 2}
	got := pop3CommandDetail(counts, 2)
	if got != "USER PASS RETR×3 DELE 删除2" {
		t.Fatalf("detail = %q", got)
	}
	if empty := pop3CommandDetail(nil, 0); empty != "连接建立，无命令" {
		t.Fatalf("empty detail = %q", empty)
	}
}

// mockPusher 记录推送调用的测试桩。
type mockPusher struct {
	expunged []struct {
		Email   string
		Mailbox string
		Seqs    []uint32
	}
}

func (m *mockPusher) PushNewMessage(string, *db.Message)           {}
func (m *mockPusher) PushFlagsChanged(string, string, *db.Message) {}
func (m *mockPusher) PushExpunged(email, mailbox string, seqs []uint32) {
	m.expunged = append(m.expunged, struct {
		Email   string
		Mailbox string
		Seqs    []uint32
	}{email, mailbox, seqs})
}

// TestExpungePushesIMAPUpdate 验证 POP3 删除邮件后向 IMAP 推送 Expunge。
func TestExpungePushesIMAPUpdate(t *testing.T) {
	s := newTestServer(t)
	pusher := &mockPusher{}
	s.pusher = pusher

	domain := &db.Domain{Name: "example.com"}
	if err := s.stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	user := &db.User{Username: "alice", DomainID: domain.ID, IsActive: true}
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	user.PasswordHash = string(hashed)
	if err := s.stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	for i := 0; i < 3; i++ {
		msg := &db.Message{UserID: user.ID, Folder: "INBOX", FromAddr: "x@y", Subject: "m", Date: time.Now()}
		if err := s.stores.Mails.Create(msg); err != nil {
			t.Fatalf("create message %d: %v", i, err)
		}
	}

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleConn(server, 110)
	}()

	br := bufio.NewReader(client)
	br.ReadString('\n')
	client.Write([]byte("USER alice@example.com\r\n"))
	br.ReadString('\n')
	client.Write([]byte("PASS secret123\r\n"))
	br.ReadString('\n')
	// 删除第 1 封后退出
	client.Write([]byte("DELE 1\r\n"))
	br.ReadString('\n')
	client.Write([]byte("QUIT\r\n"))
	br.ReadString('\n')
	client.Close()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleConn did not return")
	}

	if len(pusher.expunged) != 1 {
		t.Fatalf("expunge pushes = %d, want 1", len(pusher.expunged))
	}
	p := pusher.expunged[0]
	if p.Email != "alice@example.com" || p.Mailbox != "INBOX" {
		t.Fatalf("push target = %s/%s", p.Email, p.Mailbox)
	}
	if len(p.Seqs) != 1 || p.Seqs[0] != 1 {
		t.Fatalf("seqs = %v, want [1]", p.Seqs)
	}
	// 邮件确实已删除
	if n, _ := s.stores.Mails.CountByUserAndFolder(user.ID, "INBOX"); n != 2 {
		t.Fatalf("inbox count = %d, want 2", n)
	}
}
