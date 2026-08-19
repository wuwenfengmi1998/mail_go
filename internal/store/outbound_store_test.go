package store

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mail_go/internal/db"
)

// TestOutboundClaimAtomic 验证并发抢占同一队列项恰好只有一次成功。
func TestOutboundClaimAtomic(t *testing.T) {
	s := newTestStores(t)

	item := &db.OutboundMessage{
		MessageID:     "<t@test>",
		FromAddr:      "a@test.local",
		ToAddr:        "b@fake.test",
		RecipientDom:  "fake.test",
		RawData:       "raw",
		Status:        db.OutboundStatusPending,
		NextAttemptAt: time.Now(),
	}
	if err := s.Outbound.Create(item); err != nil {
		t.Fatalf("create: %v", err)
	}

	const n = 10
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Outbound.Claim(item.ID)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("claims won = %d, want exactly 1", wins)
	}
	got, err := s.Outbound.GetByID(item.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != db.OutboundStatusSending {
		t.Fatalf("status = %s, want sending", got.Status)
	}
}

// TestOutboundClaimStatuses 验证只有 pending/deferred 可被抢占。
func TestOutboundClaimStatuses(t *testing.T) {
	s := newTestStores(t)

	cases := []struct {
		status string
		want   bool
	}{
		{db.OutboundStatusPending, true},
		{db.OutboundStatusDeferred, true},
		{db.OutboundStatusSending, false},
		{db.OutboundStatusSent, false},
		{db.OutboundStatusFailed, false},
		{db.OutboundStatusCanceled, false},
	}
	for _, tc := range cases {
		item := &db.OutboundMessage{
			MessageID:     "<t@test>",
			FromAddr:      "a@test.local",
			ToAddr:        "b@fake.test",
			RecipientDom:  "fake.test",
			RawData:       "raw",
			Status:        tc.status,
			NextAttemptAt: time.Now(),
		}
		if err := s.Outbound.Create(item); err != nil {
			t.Fatalf("create %s: %v", tc.status, err)
		}
		ok, err := s.Outbound.Claim(item.ID)
		if err != nil {
			t.Fatalf("claim %s: %v", tc.status, err)
		}
		if ok != tc.want {
			t.Fatalf("claim %s = %v, want %v", tc.status, ok, tc.want)
		}
		if tc.want {
			got, _ := s.Outbound.GetByID(item.ID)
			if got.Status != db.OutboundStatusSending {
				t.Fatalf("claimed %s item must become sending", tc.status)
			}
		}
	}
}
