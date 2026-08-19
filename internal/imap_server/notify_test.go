package imap_server

import (
	"path/filepath"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// fakeSession 构造一个挂接在推送中心上的裸会话（无网络连接），用于
// 验证 Pusher 的跨会话推送内容与来源排除语义。
func fakeSession() *imapSession {
	return &imapSession{notify: make(chan struct{}, 1)}
}

func newTestServer(t *testing.T) (*IMAPServer, *store.Stores) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.MailboxState{}); err != nil {
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

	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, connhub.New())
	return srv, stores
}

// TestPushNewMessage 验证本地投递成功后推送给已选中会话的 EXISTS 更新。
func TestPushNewMessage(t *testing.T) {
	srv, stores := newTestServer(t)

	// 已有一封旧邮件；新邮件投递后 INBOX 计数应为 2
	old := &db.Message{UserID: 1, Folder: "INBOX", FromAddr: "x@y", Subject: "old", Date: time.Now().Add(-time.Hour)}
	if err := stores.Mails.Create(old); err != nil {
		t.Fatalf("create old message: %v", err)
	}
	inboxMsg := &db.Message{
		UserID:   1,
		Folder:   "INBOX",
		FromAddr: "sender@other.com",
		ToAddr:   "alice@example.com",
		Subject:  "新邮件",
		Date:     time.Now(),
	}
	if err := stores.Mails.Create(inboxMsg); err != nil {
		t.Fatalf("create message: %v", err)
	}

	hub := srv.hubForOrCreate("alice@example.com", "INBOX")
	sess := fakeSession()
	hub.add(sess)

	srv.PushNewMessage("alice@example.com", inboxMsg)

	updates := sess.takeUpdates(true)
	if len(updates) != 1 || updates[0].exists == nil {
		t.Fatalf("updates = %+v, want 1 条 EXISTS", updates)
	}
	if *updates[0].exists != 2 {
		t.Fatalf("EXISTS = %d, want 2", *updates[0].exists)
	}
}

// TestPushNewMessageNoSession 验证无会话选中时推送为 no-op（不 panic）。
func TestPushNewMessageNoSession(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.PushNewMessage("alice@example.com", &db.Message{ID: 1, UserID: 1, Folder: "INBOX", Date: time.Now()})
}

// TestPushNewMessageNilSafe 验证空参数/空指针安全。
func TestPushNewMessageNilSafe(t *testing.T) {
	var srv *IMAPServer
	srv.PushNewMessage("a@b", &db.Message{ID: 1}) // 不应 panic
	srv = NewIMAPServer(config.IMAPConfig{}, nil, nil, config.BanConfig{}, nil)
	srv.PushNewMessage("", &db.Message{ID: 1}) // 空邮箱
	srv.PushNewMessage("a@b", nil)             // 空消息
	srv.PushFlagsChanged("", "", nil)
	srv.PushExpunged("", "", nil)
}

// TestPushFlagsChanged 验证标志变化（已读/星标）推送内容正确。
func TestPushFlagsChanged(t *testing.T) {
	srv, stores := newTestServer(t)

	msg := &db.Message{UserID: 1, Folder: "INBOX", FromAddr: "x@y", Subject: "s", Date: time.Now()}
	if err := stores.Mails.Create(msg); err != nil {
		t.Fatalf("create message: %v", err)
	}
	msg.IsRead = true
	msg.IsFlagged = true

	hub := srv.hubForOrCreate("alice@example.com", "INBOX")
	sess := fakeSession()
	hub.add(sess)

	srv.PushFlagsChanged("alice@example.com", "INBOX", msg)

	updates := sess.takeUpdates(true)
	if len(updates) != 1 || updates[0].fetch == nil {
		t.Fatalf("updates = %+v, want 1 条 FETCH", updates)
	}
	f := updates[0].fetch
	if f.uid != imap.UID(msg.ID) {
		t.Fatalf("uid = %d, want %d", f.uid, msg.ID)
	}
	got := make(map[imap.Flag]bool)
	for _, fl := range f.flags {
		got[fl] = true
	}
	if !got[imap.FlagSeen] || !got[imap.FlagFlagged] {
		t.Fatalf("flags = %v, want \\Seen and \\Flagged", f.flags)
	}
}

// TestPushExpunged 验证删除推送：每条序号一个 EXPUNGE 更新。
func TestPushExpunged(t *testing.T) {
	srv, _ := newTestServer(t)

	hub := srv.hubForOrCreate("alice@example.com", "INBOX")
	sess := fakeSession()
	hub.add(sess)

	srv.PushExpunged("alice@example.com", "INBOX", []uint32{2, 5})

	updates := sess.takeUpdates(true)
	if len(updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(updates))
	}
	if updates[0].expunge == nil || updates[1].expunge == nil {
		t.Fatalf("updates = %+v, want expunge updates", updates)
	}
	if *updates[0].expunge != 2 || *updates[1].expunge != 5 {
		t.Fatalf("seqs = %d,%d, want 2,5", *updates[0].expunge, *updates[1].expunge)
	}
}

// TestHubExcludesSource 验证来源会话不会收到自己动作的回声推送：
// 会话 A 的 STORE/EXPUNGE 只分发给同邮箱的其他会话（本会话的响应已由
// 命令本身写回）。回归：v2 库自带 MailboxTracker 对 EXPUNGE/EXISTS 无法
// 排除来源，曾导致本会话在后续 Poll 时收到重复 EXPUNGE。
func TestHubExcludesSource(t *testing.T) {
	srv, _ := newTestServer(t)

	hub := srv.hubForOrCreate("alice@example.com", "INBOX")
	source := fakeSession()
	other := fakeSession()
	hub.add(source)
	hub.add(other)

	hub.enqueue(sessionUpdate{expunge: ptrU32(3)}, source)

	if got := source.takeUpdates(true); len(got) != 0 {
		t.Fatalf("source 收到 %d 条回声更新，want 0", len(got))
	}
	got := other.takeUpdates(true)
	if len(got) != 1 || got[0].expunge == nil || *got[0].expunge != 3 {
		t.Fatalf("other updates = %+v, want 1 条 expunge(3)", got)
	}
}

// TestPollAllowExpunge 验证 FETCH/STORE/SEARCH 期间不下发 EXPUNGE
// （allowExpunge=false 时遇到 EXPUNGE 停止），与 RFC 一致。
func TestPollAllowExpunge(t *testing.T) {
	srv, _ := newTestServer(t)

	hub := srv.hubForOrCreate("alice@example.com", "INBOX")
	sess := fakeSession()
	hub.add(sess)

	hub.enqueue(sessionUpdate{fetch: &sessionFetchUpdate{seq: 1, uid: 1, flags: []imap.Flag{imap.FlagSeen}}}, nil)
	hub.enqueue(sessionUpdate{expunge: ptrU32(2)}, nil)
	hub.enqueue(sessionUpdate{fetch: &sessionFetchUpdate{seq: 3, uid: 3, flags: []imap.Flag{imap.FlagSeen}}}, nil)

	// allowExpunge=false：只取到第一条 FETCH，EXPUNGE 及其后的留在队列
	updates := sess.takeUpdates(false)
	if len(updates) != 1 || updates[0].fetch == nil {
		t.Fatalf("updates = %+v, want 仅 1 条 FETCH", updates)
	}
	// allowExpunge=true：剩余全部下发
	rest := sess.takeUpdates(true)
	if len(rest) != 2 {
		t.Fatalf("rest = %d, want 2", len(rest))
	}
	if rest[0].expunge == nil || *rest[0].expunge != 2 {
		t.Fatalf("rest[0] = %+v, want expunge(2)", rest[0])
	}
}
