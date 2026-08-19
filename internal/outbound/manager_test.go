package outbound

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newTestManagerStores 创建带 outbound_messages 表的测试数据库。
func newTestManagerStores(t *testing.T) *store.Stores {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.OutboundMessage{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.NewStores(gdb)
}

// seedPending 写入 n 条立即可投递的 pending 队列项。
func seedPending(t *testing.T, stores *store.Stores, n int) []uint {
	t.Helper()
	ids := make([]uint, 0, n)
	for i := 0; i < n; i++ {
		item := &db.OutboundMessage{
			MessageID:     "<seed@test>",
			FromAddr:      "sender@test.local",
			ToAddr:        "rcpt@fake.test",
			RecipientDom:  "fake.test",
			RawData:       "From: sender@test.local\r\nTo: rcpt@fake.test\r\nSubject: t\r\n\r\nbody\r\n",
			Status:        db.OutboundStatusPending,
			Attempts:      0,
			NextAttemptAt: time.Now(),
		}
		if err := stores.Outbound.Create(item); err != nil {
			t.Fatalf("create item: %v", err)
		}
		ids = append(ids, item.ID)
	}
	return ids
}

// countByStatus 统计队列中指定状态的项数。
func countByStatus(t *testing.T, stores *store.Stores, status string) int64 {
	t.Helper()
	n, err := stores.Outbound.CountByStatus(status)
	if err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}

// waitFor 轮询等待条件满足或超时。
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// concurrencyTracker 统计并发调用数（用于验证 worker 池与每域信号量）。
type concurrencyTracker struct {
	mu         sync.Mutex
	active     int
	peak       int
	deliveries int
	err        error
}

// deliverWithDelay 返回一个带固定延迟的注入投递函数，并统计并发峰值。
func deliverWithDelay(tr *concurrencyTracker, delay time.Duration) func(string, string, []byte) (string, error) {
	return func(from, to string, data []byte) (string, error) {
		tr.mu.Lock()
		tr.active++
		if tr.active > tr.peak {
			tr.peak = tr.active
		}
		tr.mu.Unlock()

		time.Sleep(delay)

		tr.mu.Lock()
		tr.active--
		tr.deliveries++
		tr.mu.Unlock()
		return "250 2.0.0 queued", nil
	}
}

// TestManagerConcurrentDelivery 验证 worker 池真并发投递且每封只投一次。
func TestManagerConcurrentDelivery(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 12)

	tr := &concurrencyTracker{}
	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    5,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        4,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = deliverWithDelay(tr, 150*time.Millisecond)
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "all items sent", func() bool {
		return countByStatus(t, stores, db.OutboundStatusSent) == 12
	})

	if tr.peak < 2 {
		t.Fatalf("expected concurrent deliveries (peak=%d), got serial behavior", tr.peak)
	}
	if tr.deliveries != 12 {
		t.Fatalf("deliveries = %d, want 12 (each message exactly once)", tr.deliveries)
	}
	if n := countByStatus(t, stores, db.OutboundStatusPending) + countByStatus(t, stores, db.OutboundStatusDeferred); n != 0 {
		t.Fatalf("%d items still pending/deferred", n)
	}
}

// TestManagerSerialFallback 验证 workers=1 时退化为串行（旧行为）。
func TestManagerSerialFallback(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 6)

	tr := &concurrencyTracker{}
	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    5,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        1,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = deliverWithDelay(tr, 50*time.Millisecond)
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "all items sent", func() bool {
		return countByStatus(t, stores, db.OutboundStatusSent) == 6
	})
	if tr.peak > 1 {
		t.Fatalf("workers=1 must be serial, peak=%d", tr.peak)
	}
}

// TestManagerDomainLimit 验证同一收件域的并发连接数不超过上限。
func TestManagerDomainLimit(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 8)

	tr := &concurrencyTracker{}
	cfg := config.OutboundConfig{
		PollInterval:           1,
		MaxAttempts:            5,
		RetryBaseMin:           1,
		MaxPerDay:              10000,
		Workers:                8,
		BatchSize:              50,
		MaxConcurrentPerDomain: 1,
		ConnectTimeout:         10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = deliverWithDelay(tr, 100*time.Millisecond)
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "all items sent", func() bool {
		return countByStatus(t, stores, db.OutboundStatusSent) == 8
	})
	if tr.peak > 1 {
		t.Fatalf("per-domain limit 1 violated: peak=%d", tr.peak)
	}
	if tr.deliveries != 8 {
		t.Fatalf("deliveries = %d, want 8", tr.deliveries)
	}
}

// TestManagerRetriesTemporaryFailure 验证临时失败进入退避重试（deferred）。
func TestManagerRetriesTemporaryFailure(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 1)

	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    3,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        2,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = func(from, to string, data []byte) (string, error) {
		return "", newTempError("connection refused")
	}
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "item deferred", func() bool {
		return countByStatus(t, stores, db.OutboundStatusDeferred) == 1
	})

	items, _, err := stores.Outbound.List(1, 10, db.OutboundStatusDeferred)
	if err != nil || len(items) != 1 {
		t.Fatalf("list deferred: %v (n=%d)", err, len(items))
	}
	if items[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", items[0].Attempts)
	}
	if items[0].LastError == "" {
		t.Fatal("expected last error recorded")
	}
}

// TestManagerPermanentFailureBouncesAndFails 验证永久失败直接标记 failed。
func TestManagerPermanentFailureBouncesAndFails(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 1)

	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    3,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        2,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = func(from, to string, data []byte) (string, error) {
		return "", newPermError("550 recipient rejected")
	}
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "item failed", func() bool {
		return countByStatus(t, stores, db.OutboundStatusFailed) == 1
	})
	if n := countByStatus(t, stores, db.OutboundStatusDeferred); n != 0 {
		t.Fatalf("permanent failure must not defer: %d deferred", n)
	}
}

// TestManagerDomainLimitNoLimit 验证 max_concurrent_per_domain=0 不限制并发。
func TestManagerDomainLimitNoLimit(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 8)

	tr := &concurrencyTracker{}
	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    5,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        8,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = deliverWithDelay(tr, 80*time.Millisecond)
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "all items sent", func() bool {
		return countByStatus(t, stores, db.OutboundStatusSent) == 8
	})
	if tr.peak < 2 {
		t.Fatalf("expected concurrent deliveries without domain limit, peak=%d", tr.peak)
	}
}

// TestManagerDeliverErrorsPropagate 防御：投递函数报错不影响 worker 存活，
// 错误项进入重试（deferred）。
func TestManagerDeliverErrorsPropagate(t *testing.T) {
	stores := newTestManagerStores(t)
	seedPending(t, stores, 3)

	var calls atomic.Int32
	cfg := config.OutboundConfig{
		PollInterval:   1,
		MaxAttempts:    2,
		RetryBaseMin:   1,
		MaxPerDay:      10000,
		Workers:        2,
		BatchSize:      50,
		ConnectTimeout: 10,
	}
	m := NewManager(cfg, "test.local", stores)
	m.deliver = func(from, to string, data []byte) (string, error) {
		n := calls.Add(1)
		if n%2 == 0 {
			return "", errors.New("boom")
		}
		return "250 ok", nil
	}
	m.Start()
	t.Cleanup(m.Stop)

	waitFor(t, 15*time.Second, "queue settled", func() bool {
		return countByStatus(t, stores, db.OutboundStatusSent)+countByStatus(t, stores, db.OutboundStatusDeferred) == 3
	})
	if calls.Load() != 3 {
		t.Fatalf("deliver calls = %d, want 3 (once per item)", calls.Load())
	}
	if n := countByStatus(t, stores, db.OutboundStatusFailed); n != 0 {
		t.Fatalf("unexpected failed items: %d", n)
	}
}
