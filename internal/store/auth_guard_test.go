package store

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"mail_go/internal/db"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newTestStores(t *testing.T) *Stores {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.BanEntry{}, &db.OutboundMessage{}, &db.ProtocolLog{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStores(gdb)
}

// TestRecordAuthFailureBansAfterThreshold 验证连续认证失败达到阈值后封禁。
func TestRecordAuthFailureBansAfterThreshold(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.10"
	const maxFail = 3

	// 前两次失败不封禁
	for i := 1; i < maxFail; i++ {
		banned, count := s.RecordAuthFailure(ip, maxFail, 30)
		if banned {
			t.Fatalf("attempt %d should not be banned yet", i)
		}
		if count != i {
			t.Fatalf("attempt %d: fail count = %d, want %d", i, count, i)
		}
	}

	// 第三次失败触发封禁
	banned, count := s.RecordAuthFailure(ip, maxFail, 30)
	if !banned {
		t.Fatal("attempt reaching threshold should ban the IP")
	}
	if count != maxFail {
		t.Fatalf("fail count = %d, want %d", count, maxFail)
	}

	// IP 现在处于封禁状态
	banned, entry := s.Bans.IsBanned(ip)
	if !banned {
		t.Fatal("IP should be banned")
	}
	if entry.ExpiresAt.Before(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("ban expiry too short: %v", entry.ExpiresAt)
	}
}

// TestRecordAuthFailureEmptyIPSafe 空 IP 不应产生副作用。
func TestRecordAuthFailureEmptyIPSafe(t *testing.T) {
	s := newTestStores(t)
	banned, count := s.RecordAuthFailure("", 3, 30)
	if banned || count != 0 {
		t.Fatalf("empty IP must be a no-op: banned=%v count=%d", banned, count)
	}
	if _, err := s.Bans.GetByIP(""); err == nil {
		t.Fatal("empty IP should not be recorded")
	}
}

// TestRecordAuthFailureWebAndProtocolShared 协议层与 Web 层共用封禁记录。
func TestRecordAuthFailureWebAndProtocolShared(t *testing.T) {
	s := newTestStores(t)
	const ip = "198.51.100.20"

	// Web 层已封禁（直接建记录模拟），协议层认证必须被拒绝
	s.Bans.Create(&db.BanEntry{
		IPAddress: ip,
		Reason:    "web login failures",
		FailCount: 5,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if banned, _ := s.Bans.IsBanned(ip); !banned {
		t.Fatal("IP should be banned for both web and protocol auth")
	}
}

func TestClientIPFromAddr(t *testing.T) {
	cases := []struct {
		addr net.Addr
		want string
	}{
		{nil, ""},
		{addrMock("203.0.113.5:12345"), "203.0.113.5"},
		{addrMock("[2001:db8::1]:993"), "2001:db8::1"},
		{addrMock("bad-format"), "bad-format"},
	}
	for _, tc := range cases {
		if got := ClientIPFromAddr(tc.addr); got != tc.want {
			t.Errorf("ClientIPFromAddr(%v) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

// addrMock 实现 net.Addr 的最小桩。
type addrMock string

func (a addrMock) Network() string { return "tcp" }
func (a addrMock) String() string  { return string(a) }

// P3 #13：配额原子预扣——并发/超额场景下不得绕过配额。
func TestTryReserveQuota(t *testing.T) {
	s := newTestStores(t)

	// 用户配额 1000
	user := &db.User{
		Username:   "quota_user",
		PasswordHash: "x",
		DomainID:   0,
		QuotaBytes: 1000,
		IsActive:   true,
	}
	if err := s.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if user.ID == 0 {
		t.Fatal("user ID must be assigned")
	}

	// 预扣 600 成功
	ok, err := s.Users.TryReserveQuota(user.ID, 600)
	if err != nil || !ok {
		t.Fatalf("reserve 600: ok=%v err=%v", ok, err)
	}
	// 再扣 400 正好用完
	ok, err = s.Users.TryReserveQuota(user.ID, 400)
	if err != nil || !ok {
		t.Fatalf("reserve 400: ok=%v err=%v", ok, err)
	}
	// 超出配额被拒且不改变 used_bytes
	ok, err = s.Users.TryReserveQuota(user.ID, 1)
	if err != nil {
		t.Fatalf("reserve beyond quota: %v", err)
	}
	if ok {
		t.Fatal("reserve beyond quota must fail")
	}
	got, _ := s.Users.GetByID(user.ID)
	if got.UsedBytes != 1000 {
		t.Fatalf("used_bytes = %d, want 1000 (no partial charge)", got.UsedBytes)
	}

	// 释放后可以再次预扣
	if err := s.Users.UpdateUsedBytes(user.ID, -500); err != nil {
		t.Fatalf("release: %v", err)
	}
	ok, err = s.Users.TryReserveQuota(user.ID, 500)
	if err != nil || !ok {
		t.Fatalf("reserve after release: ok=%v err=%v", ok, err)
	}
}

// P3 #13：非正 delta 不允许（防御）。
func TestTryReserveQuotaNonPositiveDelta(t *testing.T) {
	s := newTestStores(t)
	user := &db.User{Username: "u", PasswordHash: "x", QuotaBytes: 100}
	if err := s.Users.Create(user); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int64{0, -10} {
		ok, err := s.Users.TryReserveQuota(user.ID, delta)
		if err != nil {
			t.Fatalf("delta %d: %v", delta, err)
		}
		if ok {
			t.Fatalf("delta %d must not reserve", delta)
		}
	}
	got, _ := s.Users.GetByID(user.ID)
	if got.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d, want 0", got.UsedBytes)
	}
}
