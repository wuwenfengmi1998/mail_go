package imap_server

import (
	"path/filepath"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/backend"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestPushNewMessage 验证本地投递成功后推送的 MessageUpdate 内容正确。
func TestPushNewMessage(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	domain := &db.Domain{Name: "example.com"}
	if err := stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	user := &db.User{Username: "alice", DomainID: domain.ID, IsActive: true}
	if err := stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	email := "alice@example.com"

	// 已有一封旧邮件，新邮件应为 INBOX 第 2 封
	old := &db.Message{UserID: user.ID, Folder: "INBOX", FromAddr: "x@y", Subject: "old", Date: time.Now()}
	if err := stores.Mails.Create(old); err != nil {
		t.Fatalf("create old message: %v", err)
	}
	inboxMsg := &db.Message{
		UserID:    user.ID,
		Folder:    "INBOX",
		FromAddr:  "sender@other.com",
		ToAddr:    email,
		Subject:   "新邮件",
		RawData:   "From: sender@other.com\r\nSubject: 新邮件\r\n\r\nhello",
		MessageID: "<new-1@other.com>",
		Date:      time.Now(),
		IsRead:    false,
	}
	if err := stores.Mails.Create(inboxMsg); err != nil {
		t.Fatalf("create message: %v", err)
	}

	hub := connhub.New()
	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, hub)
	// 模拟明文 + TLS 两个监听器（生产环境由 Start/StartTLS 注册）
	srv.newServer("127.0.0.1:143", nil)
	srv.newServer("127.0.0.1:993", nil)
	srv.PushNewMessage(email, inboxMsg)

	// 两个监听器（明文/TLS）各有一个 backend 通道，都应收到同一更新
	srv.beMu.Lock()
	bes := append([]*imapBackend(nil), srv.bes...)
	srv.beMu.Unlock()
	if len(bes) == 0 {
		t.Fatal("no backends registered")
	}

	for i, b := range bes {
		select {
		case upd := <-b.updates:
			mu, ok := upd.(*backend.MessageUpdate)
			if !ok {
				t.Fatalf("backend %d: update type = %T, want *MessageUpdate", i, upd)
			}
			if mu.Username() != email {
				t.Fatalf("backend %d: username = %q, want %q", i, mu.Username(), email)
			}
			if mu.Mailbox() != "INBOX" {
				t.Fatalf("backend %d: mailbox = %q, want INBOX", i, mu.Mailbox())
			}
			if mu.Message.Uid != uint32(inboxMsg.ID) {
				t.Fatalf("backend %d: uid = %d, want %d", i, mu.Message.Uid, inboxMsg.ID)
			}
			if mu.Message.SeqNum != 2 {
				t.Fatalf("backend %d: seq = %d, want 2", i, mu.Message.SeqNum)
			}
			if mu.Message.Envelope == nil || mu.Message.Envelope.Subject != "新邮件" {
				t.Fatalf("backend %d: envelope missing subject", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("backend %d: no update received", i)
		}
	}
}

// TestPushNewMessageChannelFull 验证通道满时推送不阻塞（非阻塞丢弃）。
func TestPushNewMessageChannelFull(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	hub := connhub.New()
	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, hub)
	srv.newServer("127.0.0.1:143", nil)
	srv.newServer("127.0.0.1:993", nil)

	msg := &db.Message{ID: 1, Folder: "INBOX", Date: time.Now()}
	done := make(chan struct{})
	go func() {
		// 灌满所有 backend 通道（容量 256），再调用必须立即返回
		srv.beMu.Lock()
		bes := append([]*imapBackend(nil), srv.bes...)
		srv.beMu.Unlock()
		for _, b := range bes {
			for i := 0; i < cap(b.updates); i++ {
				b.updates <- backend.NewUpdate("a@b", "INBOX")
			}
		}
		srv.PushNewMessage("a@b", msg)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("PushNewMessage blocked on full channel")
	}
}

// TestPushNewMessageNilSafe 验证空参数/空指针安全。
func TestPushNewMessageNilSafe(t *testing.T) {
	var srv *IMAPServer
	srv.PushNewMessage("a@b", &db.Message{ID: 1}) // 不应 panic
	srv = NewIMAPServer(config.IMAPConfig{}, nil, nil, config.BanConfig{}, nil)
	srv.PushNewMessage("", &db.Message{ID: 1}) // 空邮箱
	srv.PushNewMessage("a@b", nil)             // 空消息
}

// TestPushFlagsChanged 验证标志变化（已读/星标）推送内容正确。
func TestPushFlagsChanged(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	domain := &db.Domain{Name: "example.com"}
	if err := stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	user := &db.User{Username: "alice", DomainID: domain.ID, IsActive: true}
	if err := stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	msg := &db.Message{UserID: user.ID, Folder: "INBOX", FromAddr: "x@y", Subject: "s", Date: time.Now()}
	if err := stores.Mails.Create(msg); err != nil {
		t.Fatalf("create message: %v", err)
	}
	msg.IsRead = true
	msg.IsFlagged = true

	hub := connhub.New()
	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, hub)
	srv.newServer("127.0.0.1:143", nil)
	srv.PushFlagsChanged("alice@example.com", "INBOX", msg)

	srv.beMu.Lock()
	b := srv.bes[0]
	srv.beMu.Unlock()

	select {
	case upd := <-b.updates:
		mu, ok := upd.(*backend.MessageUpdate)
		if !ok {
			t.Fatalf("update type = %T, want *MessageUpdate", upd)
		}
		if mu.Username() != "alice@example.com" || mu.Mailbox() != "INBOX" {
			t.Fatalf("update targeting = %s/%s", mu.Username(), mu.Mailbox())
		}
		if mu.Message.Uid != uint32(msg.ID) {
			t.Fatalf("uid = %d, want %d", mu.Message.Uid, msg.ID)
		}
		got := make(map[string]bool)
		for _, f := range mu.Message.Flags {
			got[f] = true
		}
		if !got["\\Seen"] || !got["\\Flagged"] {
			t.Fatalf("flags = %v, want \\Seen and \\Flagged", mu.Message.Flags)
		}
	case <-time.After(time.Second):
		t.Fatal("no flags update received")
	}
}

// TestPushExpunged 验证删除推送：每条序号一个 ExpungeUpdate。
func TestPushExpunged(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	hub := connhub.New()
	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, hub)
	srv.newServer("127.0.0.1:143", nil)
	srv.PushExpunged("alice@example.com", "INBOX", []uint32{2, 5})

	srv.beMu.Lock()
	b := srv.bes[0]
	srv.beMu.Unlock()

	var seqs []uint32
	for i := 0; i < 2; i++ {
		select {
		case upd := <-b.updates:
			eu, ok := upd.(*backend.ExpungeUpdate)
			if !ok {
				t.Fatalf("update type = %T, want *ExpungeUpdate", upd)
			}
			if eu.Username() != "alice@example.com" || eu.Mailbox() != "INBOX" {
				t.Fatalf("update targeting = %s/%s", eu.Username(), eu.Mailbox())
			}
			seqs = append(seqs, eu.SeqNum)
		case <-time.After(time.Second):
			t.Fatal("no expunge update received")
		}
	}
	if seqs[0] != 2 || seqs[1] != 5 {
		t.Fatalf("seqs = %v, want [2 5]", seqs)
	}
}

// TestBroadcastUpdateIsolatedPerListener 回归测试：同一更新广播到多个监听器
// 时，每个监听器必须持有独立的 Update 对象（独立 Done channel），否则
// 多个 listenUpdates 会对同一 channel 二次 close 导致
// panic: close of closed channel。
func TestBroadcastUpdateIsolatedPerListener(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.Message{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	hub := connhub.New()
	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, hub)
	srv.newServer("127.0.0.1:143", nil)
	srv.newServer("127.0.0.1:993", nil)

	srv.PushExpunged("alice@example.com", "INBOX", []uint32{1})

	srv.beMu.Lock()
	bes := append([]*imapBackend(nil), srv.bes...)
	srv.beMu.Unlock()

	// 每个监听器各收到一条更新
	var updates []backend.Update
	for i, b := range bes {
		select {
		case upd := <-b.updates:
			updates = append(updates, upd)
		case <-time.After(time.Second):
			t.Fatalf("backend %d: no update received", i)
		}
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(updates))
	}

	// 关键断言：两条更新必须拥有独立的 Done channel
	if updates[0].Done() == updates[1].Done() {
		t.Fatal("listeners share the same Done channel: double close would panic")
	}

	// 模拟两个 listenUpdates 各自执行 close(update.Done())：修复前必 panic
	for _, upd := range updates {
		close(upd.Done())
	}
}
