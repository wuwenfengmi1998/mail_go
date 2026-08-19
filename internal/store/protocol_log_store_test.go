package store

import (
	"testing"
	"time"

	"mail_go/internal/db"
)

func TestProtocolLogCreateAndUpdateDuration(t *testing.T) {
	s := newTestStores(t)

	entry := &db.ProtocolLog{
		Protocol:  db.ProtocolIMAP,
		Port:      143,
		ClientIP:  "203.0.113.9",
		Username:  "alice",
		Success:   true,
		Detail:    "LOGIN 成功",
		CreatedAt: time.Now(),
	}
	if err := s.ProtocolLogs.Create(entry); err != nil {
		t.Fatalf("create: %v", err)
	}
	if entry.ID == 0 {
		t.Fatal("expected generated ID")
	}

	if err := s.ProtocolLogs.UpdateDuration(entry.ID, 3210); err != nil {
		t.Fatalf("update duration: %v", err)
	}

	logs, total, err := s.ProtocolLogs.List(1, 10, ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("expected 1 log, got total=%d len=%d", total, len(logs))
	}
	if logs[0].DurationMs != 3210 {
		t.Fatalf("duration = %d, want 3210", logs[0].DurationMs)
	}
	if logs[0].Success != true || logs[0].Username != "alice" {
		t.Fatalf("unexpected log: %+v", logs[0])
	}
}

func TestProtocolLogListFilters(t *testing.T) {
	s := newTestStores(t)

	now := time.Now()
	entries := []*db.ProtocolLog{
		{Protocol: db.ProtocolSMTP, Port: 25, ClientIP: "10.0.0.1", Username: "", Success: true, FailReason: "", Detail: "投递", CreatedAt: now.Add(-3 * time.Hour)},
		{Protocol: db.ProtocolSMTP, Port: 25, ClientIP: "10.0.0.2", Username: "admin", Success: false, FailReason: "中继访问被拒绝", Detail: "RCPT", CreatedAt: now.Add(-2 * time.Hour)},
		{Protocol: db.ProtocolIMAP, Port: 993, ClientIP: "10.0.0.2", Username: "admin", Success: false, FailReason: "用户名或密码错误", Detail: "LOGIN 失败", CreatedAt: now.Add(-1 * time.Hour)},
		{Protocol: db.ProtocolPOP3, Port: 110, ClientIP: "10.0.0.3", Username: "bob", Success: true, FailReason: "", Detail: "STAT", CreatedAt: now},
	}
	for _, e := range entries {
		if err := s.ProtocolLogs.Create(e); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	cases := []struct {
		name   string
		filter ProtocolLogFilter
		want   int64
	}{
		{"全部", ProtocolLogFilter{}, 4},
		{"按协议", ProtocolLogFilter{Protocol: db.ProtocolSMTP}, 2},
		{"按失败", ProtocolLogFilter{Success: boolPtr(false)}, 2},
		{"按成功", ProtocolLogFilter{Success: boolPtr(true)}, 2},
		{"协议+失败", ProtocolLogFilter{Protocol: db.ProtocolIMAP, Success: boolPtr(false)}, 1},
		{"按IP模糊", ProtocolLogFilter{IP: "10.0.0.2"}, 2},
		{"按用户名", ProtocolLogFilter{Username: "admin"}, 2},
		{"按时间起", ProtocolLogFilter{From: now.Add(-90 * time.Minute)}, 2},
		{"按时间止", ProtocolLogFilter{To: now.Add(-2 * time.Hour)}, 2},
		{"无匹配", ProtocolLogFilter{Protocol: db.ProtocolPOP3, Success: boolPtr(false)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, total, err := s.ProtocolLogs.List(1, 50, tc.filter)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if total != tc.want {
				t.Fatalf("total = %d, want %d", total, tc.want)
			}
		})
	}
}

func TestProtocolLogListPagination(t *testing.T) {
	s := newTestStores(t)
	for i := 0; i < 5; i++ {
		if err := s.ProtocolLogs.Create(&db.ProtocolLog{
			Protocol: db.ProtocolSMTP, ClientIP: "10.0.0.1",
			Success: true, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	page1, total, err := s.ProtocolLogs.List(1, 2, ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 5 || len(page1) != 2 {
		t.Fatalf("page1 total=%d len=%d", total, len(page1))
	}
	// 新记录在前
	if page1[0].ID < page1[1].ID {
		t.Fatal("expected newest first")
	}

	page3, _, err := s.ProtocolLogs.List(3, 2, ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list page3: %v", err)
	}
	if len(page3) != 1 {
		t.Fatalf("page3 len = %d, want 1", len(page3))
	}
}

func TestProtocolLogCountStatsAndCleanup(t *testing.T) {
	s := newTestStores(t)

	now := time.Now()
	entries := []*db.ProtocolLog{
		{Protocol: db.ProtocolSMTP, ClientIP: "a", Success: true, CreatedAt: now.Add(-10 * time.Minute)},
		{Protocol: db.ProtocolSMTP, ClientIP: "b", Success: false, CreatedAt: now.Add(-20 * time.Minute)},
		{Protocol: db.ProtocolIMAP, ClientIP: "c", Success: false, CreatedAt: now.Add(-30 * time.Minute)},
		{Protocol: db.ProtocolIMAP, ClientIP: "d", Success: false, CreatedAt: now.AddDate(0, 0, -40)},
	}
	for _, e := range entries {
		if err := s.ProtocolLogs.Create(e); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	stats, err := s.ProtocolLogs.CountStats(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats[db.ProtocolSMTP]["success"] != 1 || stats[db.ProtocolSMTP]["fail"] != 1 {
		t.Fatalf("smtp stats: %+v", stats[db.ProtocolSMTP])
	}
	if stats[db.ProtocolIMAP]["fail"] != 1 {
		t.Fatalf("imap fail stats: %+v", stats[db.ProtocolIMAP])
	}

	// 清理 30 天前的记录
	n, err := s.ProtocolLogs.CleanupBefore(now.AddDate(0, 0, -30))
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	_, total, err := s.ProtocolLogs.List(1, 50, ProtocolLogFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
}

func boolPtr(v bool) *bool {
	return &v
}
