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
