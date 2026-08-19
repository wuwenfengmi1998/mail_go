package handlers

import (
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// performPost 发送 POST 请求并返回响应（用于处理器测试）。
func performPost(r *gin.Engine, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestDisconnectConnection 验证「断开并封禁」：创建黑名单记录并断开该 IP 全部连接。
func TestDisconnectConnection(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.BanEntry{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)
	hub := connhub.New()

	var closed atomic.Int32
	// 目标 IP 两个连接（模拟多协议在线）
	c1 := hub.Register("smtp", "203.0.113.77", 25, false)
	c1.SetDisconnect(func() { closed.Add(1) })
	c2 := hub.Register("imap", "203.0.113.77", 993, true)
	c2.SetDisconnect(func() { closed.Add(1) })
	// 其他 IP 不应受影响
	c3 := hub.Register("pop3", "203.0.113.78", 110, false)
	c3.SetDisconnect(func() { closed.Add(1) })

	h := &AdminHandler{stores: stores, hub: hub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/admin/connections/:id/disconnect", h.DisconnectConnection)

	rec := performPost(r, "/admin/connections/1/disconnect")
	if rec.Code != 302 {
		t.Fatalf("status = %d, want 302", rec.Code)
	}

	// 该 IP 的两个连接都被断开，其他连接不受影响
	if closed.Load() != 2 {
		t.Fatalf("closed = %d, want 2", closed.Load())
	}
	if n := hub.Counts()["pop3"]; n != 1 {
		t.Fatalf("pop3 count = %d, want 1 (unaffected)", n)
	}

	// 黑名单记录：180 天封禁
	banned, entry := stores.Bans.IsBanned("203.0.113.77")
	if !banned {
		t.Fatal("IP should be banned")
	}
	if entry.Reason != "管理员手动封禁（连接断开）" {
		t.Fatalf("reason = %q", entry.Reason)
	}
	wantExpiry := time.Now().Add(180 * 24 * time.Hour)
	if entry.ExpiresAt.Before(wantExpiry.Add(-time.Minute)) || entry.ExpiresAt.After(wantExpiry.Add(time.Minute)) {
		t.Fatalf("expiry = %v, want ~180 days", entry.ExpiresAt)
	}
}

// TestDisconnectConnectionNotFound 验证不存在的连接返回 404。
func TestDisconnectConnectionNotFound(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.BanEntry{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := &AdminHandler{stores: store.NewStores(gdb), hub: connhub.New()}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/admin/connections/:id/disconnect", h.DisconnectConnection)

	rec := performPost(r, "/admin/connections/999/disconnect")
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
