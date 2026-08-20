package store

import (
	"sync"
	"net"
	"path/filepath"
	"strings"
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

// TestRecordAuthFailureFreeTriggers 验证前 3 次达到阈值只计数不封禁，
// 第 4 次起封禁（第 1 次封禁 = 配置时长）。
func TestRecordAuthFailureFreeTriggers(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.10"
	const maxFail = 2

	failOnce := func() bool {
		banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", true)
		return banned
	}

	// 第 1 次触发需要 maxFail 次失败
	for f := 0; f < maxFail; f++ {
		if failOnce() {
			t.Fatalf("trigger 1 (fail %d) should not ban yet", f+1)
		}
	}
	// 达到阈值后失败计数持续累计，之后每次失败都会再次触发
	for i := 2; i <= 3; i++ {
		if failOnce() {
			t.Fatalf("trigger %d should not ban yet", i)
		}
	}

	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	if entry.BanCount != 3 {
		t.Fatalf("ban_count = %d, want 3", entry.BanCount)
	}
	if !entry.ExpiresAt.IsZero() {
		t.Fatal("observation record must not have expiry")
	}

	// 第 4 次触发封禁，时长 = firstBanMin（30 分钟）
	if !failOnce() {
		t.Fatal("4th trigger should ban the IP")
	}

	entry, err = s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	wantExpiry := time.Now().Add(30 * time.Minute)
	if entry.ExpiresAt.Before(wantExpiry.Add(-time.Minute)) || entry.ExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Fatalf("ban expiry = %v, want ~%v", entry.ExpiresAt, wantExpiry)
	}
	if !strings.Contains(entry.Reason, "第1次封禁") {
		t.Fatalf("reason = %q, want 第1次封禁", entry.Reason)
	}
	if entry.BanCount != 4 {
		t.Fatalf("ban_count = %d, want 4", entry.BanCount)
	}

	// IP 现在处于封禁状态
	if banned, _ := s.Bans.IsBanned(ip); !banned {
		t.Fatal("IP should be banned")
	}
}

// TestStagedBanEscalation 验证封禁档位递增：30分钟 → 3小时 → 3个月 → 半年（上限）。
func TestStagedBanEscalation(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.11"
	const maxFail = 2

	failOnce := func() bool {
		banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", true)
		return banned
	}

	// 第 1 次触发需要 maxFail 次失败；此后每次失败即触发下一轮
	if failOnce() {
		t.Fatal("fail 1 must not trigger")
	}
	if failOnce() { // 触发 1
		t.Fatal("trigger 1 must not ban")
	}
	if failOnce() { // 触发 2
		t.Fatal("trigger 2 must not ban")
	}
	if failOnce() { // 触发 3
		t.Fatal("trigger 3 must not ban")
	}

	// 第 4 次触发：30 分钟
	if !failOnce() {
		t.Fatal("trigger 4 should ban")
	}
	expectBanDuration(t, s, ip, 4, 30*time.Minute)

	// 第 5 次：3 小时
	expireBan(t, s, ip)
	if !failOnce() {
		t.Fatal("trigger 5 should ban")
	}
	expectBanDuration(t, s, ip, 5, 3*time.Hour)

	// 第 6 次：3 个月
	expireBan(t, s, ip)
	if !failOnce() {
		t.Fatal("trigger 6 should ban")
	}
	expectBanDuration(t, s, ip, 6, 90*24*time.Hour)

	// 第 7 次：半年
	expireBan(t, s, ip)
	if !failOnce() {
		t.Fatal("trigger 7 should ban")
	}
	expectBanDuration(t, s, ip, 7, 180*24*time.Hour)

	// 第 8 次：仍为半年（上限）
	expireBan(t, s, ip)
	if !failOnce() {
		t.Fatal("trigger 8 should ban")
	}
	expectBanDuration(t, s, ip, 8, 180*24*time.Hour)

	entry, _ := s.Bans.GetByIP(ip)
	if !strings.Contains(entry.Reason, "第5次封禁") {
		t.Fatalf("reason = %q, want 第5次封禁", entry.Reason)
	}
}

// expectBanDuration 断言该 IP 当前封禁时长约为 min（允许 2 分钟误差）。
func expectBanDuration(t *testing.T, s *Stores, ip string, trigger int, min time.Duration) {
	t.Helper()
	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("trigger %d: %v", trigger, err)
	}
	diff := entry.ExpiresAt.Sub(time.Now())
	if diff < min-2*time.Minute || diff > min+2*time.Minute {
		t.Fatalf("trigger %d: ban duration = %v, want ~%v", trigger, diff, min)
	}
}

// expireBan 把该 IP 的封禁记录改成已过期（模拟时间流逝）。
func expireBan(t *testing.T, s *Stores, ip string) {
	t.Helper()
	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("get entry: %v", err)
	}
	entry.ExpiresAt = time.Now().Add(-time.Minute)
	if err := s.Bans.Update(entry); err != nil {
		t.Fatalf("update entry: %v", err)
	}
}

// TestBanListOnlyBannedOrExpired 验证列表只返回封禁（含已过期）记录，
// 仅计数的观察记录不出现。
func TestBanListOnlyBannedOrExpired(t *testing.T) {
	s := newTestStores(t)

	// 观察记录：失败计数，未封禁（无到期时间）
	if _, err := s.Bans.IncrementFail("203.0.113.20"); err != nil {
		t.Fatalf("increment: %v", err)
	}
	// 当前生效的封禁
	if err := s.Bans.Create(&db.BanEntry{
		IPAddress: "198.51.100.21",
		Reason:    "第1次封禁：登录失败次数过多（第4次触发，失败5次）",
		FailCount: 5,
		BanCount:  4,
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}); err != nil {
		t.Fatalf("create ban: %v", err)
	}
	// 已过期的封禁（历史）
	if err := s.Bans.Create(&db.BanEntry{
		IPAddress: "198.51.100.22",
		Reason:    "第2次封禁：登录失败次数过多（第5次触发，失败6次）",
		FailCount: 6,
		BanCount:  5,
		ExpiresAt: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatalf("create expired ban: %v", err)
	}

	entries, total, err := s.Bans.List(1, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	for _, e := range entries {
		if e.ExpiresAt.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("observation record leaked into list: %+v", e)
		}
	}
}

// TestRecordAuthFailureEmptyIPSafe 空 IP 不应产生副作用。
func TestRecordAuthFailureEmptyIPSafe(t *testing.T) {
	s := newTestStores(t)
	banned, count := s.RecordAuthFailure("", 3, 30, "登录失败次数过多", true)
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

// P5 #18 方案 A：未知用户名（枚举型爆破）跳过宽限，首次触发即封。
func TestRecordAuthFailureUnknownUserSkipsGrace(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.77"
	const maxFail = 3

	// 未知用户名：第 1 次触发（累计失败 3 次）即封第 1 档（30 分钟）
	for i := 1; i <= maxFail; i++ {
		banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", false)
		if i < maxFail && banned {
			t.Fatalf("attempt %d should not ban before threshold", i)
		}
		if i == maxFail && !banned {
			t.Fatal("unknown user: first trigger must ban immediately")
		}
	}
	banned, entry := s.Bans.IsBanned(ip)
	if !banned {
		t.Fatal("IP should be banned")
	}
	// 第 1 档 = 30 分钟
	if entry.ExpiresAt.Before(time.Now().Add(29 * time.Minute)) {
		t.Fatalf("first-stage ban duration wrong: expires %v", entry.ExpiresAt)
	}
	if !strings.Contains(entry.Reason, "未知用户名") {
		t.Fatalf("reason should note unknown-user skip: %q", entry.Reason)
	}
}

// P5 #18 方案 A：已知用户名（真实用户输错）保留前 3 次宽限（回归）。
// 触发语义与 TestRecordAuthFailureFreeTriggers 一致：达到阈值后每次失败
// 都会触发一次，前 3 次触发（第 2-4 次失败）不封禁，第 4 次触发
// （第 5 次失败）封第 1 档。
func TestRecordAuthFailureKnownUserKeepsGrace(t *testing.T) {
	s := newTestStores(t)
	const ip = "198.51.100.88"
	const maxFail = 2

	// 第 1 次失败：计数，未达阈值
	if banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", true); banned {
		t.Fatal("failure 1 must not ban")
	}
	// 第 2-4 次失败 = 触发 1-3，宽限期内不封禁
	for i := 2; i <= 4; i++ {
		banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", true)
		if banned {
			t.Fatalf("failure %d (trigger within grace) should not ban", i)
		}
	}
	if banned, _ := s.Bans.IsBanned(ip); banned {
		t.Fatal("known user must not be banned within 3 free triggers")
	}
	entry, _ := s.Bans.GetByIP(ip)
	if entry.BanCount != 3 {
		t.Fatalf("ban_count = %d, want 3", entry.BanCount)
	}

	// 第 5 次失败 = 触发 4 -> 第 1 档封禁
	banned, _ := s.RecordAuthFailure(ip, maxFail, 30, "登录失败次数过多", true)
	if !banned {
		t.Fatal("4th trigger should ban (stage 1)")
	}
	entry, _ = s.Bans.GetByIP(ip)
	if entry.BanCount != 4 {
		t.Fatalf("ban count = %d, want 4", entry.BanCount)
	}
	if !strings.Contains(entry.Reason, "第1次封禁") {
		t.Fatalf("reason = %q, want 第1次封禁", entry.Reason)
	}
}

// LoginExists：完整邮箱与裸用户名两种形态。
func TestLoginExists(t *testing.T) {
	s := newTestStores(t)
	domain := &db.Domain{Name: "example.com"}
	if err := s.Domains.Create(domain); err != nil {
		t.Fatal(err)
	}
	if err := s.Users.Create(&db.User{Username: "alice", PasswordHash: "x", DomainID: domain.ID}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		login string
		want  bool
	}{
		{"alice@example.com", true},
		{"alice", true},
		{"bob@example.com", false},
		{"bob", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := s.Users.LoginExists(tc.login); got != tc.want {
			t.Errorf("LoginExists(%q) = %v, want %v", tc.login, got, tc.want)
		}
	}
}

// P4 #17：BanIP 为 upsert 语义，已有观察记录的 IP 手动封禁后仅一条记录。
func TestBanIPUpsertSingleRow(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.99"

	// 先产生观察计数记录（未封禁）
	for i := 0; i < 2; i++ {
		_, _ = s.RecordAuthFailure(ip, 10, 30, "登录失败次数过多", true)
	}
	if banned, _ := s.Bans.IsBanned(ip); banned {
		t.Fatal("should be observation-only at this point")
	}

	// 手动封禁 180 天
	if err := s.Bans.BanIP(ip, "管理员手动封禁", 180*24*time.Hour); err != nil {
		t.Fatalf("BanIP: %v", err)
	}

	// 仅一条记录，且处于封禁状态、计数清零
	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("GetByIP: %v", err)
	}
	if entry.Reason != "管理员手动封禁" {
		t.Fatalf("reason = %q", entry.Reason)
	}
	if entry.FailCount != 0 || entry.BanCount != 0 {
		t.Fatalf("manual ban should reset counters, got fail=%d ban=%d", entry.FailCount, entry.BanCount)
	}
	if !entry.ExpiresAt.After(time.Now().Add(179 * 24 * time.Hour)) {
		t.Fatalf("manual ban duration wrong: %v", entry.ExpiresAt)
	}
}

// P4 #17：ip_address 唯一索引生效——同一 IP 不允许第二条记录
// （历史重复行由 InitDB 的 dedupeBanEntries 在 AutoMigrate 前清理，
// BanIP 的事务内“先删后插”兼容既有脏数据）。
func TestBanEntryUniqueIndex(t *testing.T) {
	s := newTestStores(t)
	const ip = "198.51.100.3"

	if err := s.Bans.Create(&db.BanEntry{IPAddress: ip, Reason: "first", ExpiresAt: time.Time{}}); err != nil {
		t.Fatal(err)
	}
	// 第二条同 IP 记录必须被唯一约束拒绝
	if err := s.Bans.Create(&db.BanEntry{IPAddress: ip, Reason: "second", ExpiresAt: time.Time{}}); err == nil {
		t.Fatal("duplicate ban entry for same IP should be rejected by unique index")
	}

	// 唯一记录上的 BanIP/IncrementFail 读写一致（无错位）
	if err := s.Bans.BanIP(ip, "管理员手动封禁", time.Hour); err != nil {
		t.Fatalf("BanIP: %v", err)
	}
	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Reason != "管理员手动封禁" || entry.FailCount != 0 {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	cnt, err := s.Bans.IncrementFail(ip)
	if err != nil || cnt != 1 {
		t.Fatalf("IncrementFail after BanIP = %d, %v; want 1, nil", cnt, err)
	}
}

// P4 #17：IncrementFail 并发安全（-race 下不产生重复行、计数准确）。
func TestIncrementFailConcurrent(t *testing.T) {
	s := newTestStores(t)
	const ip = "203.0.113.100"
	const goroutines = 16
	const perG = 5

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if _, err := s.Bans.IncrementFail(ip); err != nil {
					t.Errorf("IncrementFail: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	entry, err := s.Bans.GetByIP(ip)
	if err != nil {
		t.Fatalf("GetByIP: %v", err)
	}
	want := goroutines * perG
	if entry.FailCount != want {
		t.Fatalf("fail count = %d, want %d (lost updates or duplicate rows)", entry.FailCount, want)
	}
}
