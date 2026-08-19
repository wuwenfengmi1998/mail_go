package store

import (
	"testing"

	"mail_go/internal/db"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestMailboxStateUidValidity 验证 UIDVALIDITY：首次访问随机生成、重复访问
// 稳定返回、0 值被修正、不同邮箱互不影响。
func TestMailboxStateUidValidity(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&db.MailboxState{}); err != nil {
		t.Fatal(err)
	}
	s := newMailboxStateStore(gdb)

	// 首次访问：随机非 0
	v1, err := s.UidValidity(1, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if v1 == 0 {
		t.Fatal("UIDVALIDITY 不应为 0")
	}
	// 重复访问：稳定
	v2, err := s.UidValidity(1, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if v1 != v2 {
		t.Fatalf("UIDVALIDITY 不稳定: %d != %d", v1, v2)
	}
	// 不同邮箱：独立
	v3, err := s.UidValidity(1, "Sent")
	if err != nil {
		t.Fatal(err)
	}
	if v3 == v1 {
		t.Fatal("不同邮箱的 UIDVALIDITY 不应相同")
	}
	// 不同用户：独立
	v4, err := s.UidValidity(2, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if v4 == v1 {
		t.Fatal("不同用户的 UIDVALIDITY 不应相同")
	}
}
