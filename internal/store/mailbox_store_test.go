package store

import (
	"testing"

	"mail_go/internal/db"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// newMailboxTestStore 返回基于内存库的 MailboxStore（含 mailboxes 表）。
func newMailboxTestStore(t *testing.T) (MailboxStore, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&db.Mailbox{}, &db.Message{}, &db.MailboxState{}); err != nil {
		t.Fatal(err)
	}
	return newMailboxStore(gdb), gdb
}

// TestMailboxEnsureSystem 验证系统文件夹幂等创建与规范排序。
func TestMailboxEnsureSystem(t *testing.T) {
	s, _ := newMailboxTestStore(t)

	if err := s.EnsureSystem(1); err != nil {
		t.Fatalf("EnsureSystem: %v", err)
	}
	// 幂等：再次调用不报错、不产生重复行
	if err := s.EnsureSystem(1); err != nil {
		t.Fatalf("EnsureSystem 2nd: %v", err)
	}

	mbs, err := s.List(1)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mbs) != 4 {
		t.Fatalf("len(List) = %d, want 4", len(mbs))
	}
	want := []string{"INBOX", "Sent", "Drafts", "Trash"}
	for i, name := range want {
		if mbs[i].Name != name {
			t.Fatalf("List[%d] = %q, want %q", i, mbs[i].Name, name)
		}
	}
	if mbs[3].SpecialUse != "Trash" {
		t.Fatalf("Trash SpecialUse = %q, want Trash", mbs[3].SpecialUse)
	}
}

// TestMailboxCustomFolderLifecycle 验证自定义文件夹：创建/排序/非空删除拒绝。
func TestMailboxCustomFolderLifecycle(t *testing.T) {
	s, gdb := newMailboxTestStore(t)
	if err := s.EnsureSystem(1); err != nil {
		t.Fatal(err)
	}

	if err := s.Create(&db.Mailbox{UserID: 1, Name: "工作", IsSubscribed: true}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mbs, err := s.List(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(mbs) != 5 || mbs[4].Name != "工作" {
		t.Fatalf("custom folder not last: %+v", mbs)
	}

	// 放一封邮件进去 → 非空文件夹不可删除
	msg := &db.Message{UserID: 1, Folder: "工作", FromAddr: "a@b", Subject: "s"}
	if err := gdb.Create(msg).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(1, "工作"); err != ErrMailboxNotEmpty {
		t.Fatalf("Delete non-empty mailbox = %v, want ErrMailboxNotEmpty", err)
	}
	// 清空后可删除
	if err := gdb.Where("id = ?", msg.ID).Delete(&db.Message{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(1, "工作"); err != nil {
		t.Fatalf("Delete empty mailbox: %v", err)
	}
	if _, err := s.GetByName(1, "工作"); err == nil {
		t.Fatal("mailbox should be gone after Delete")
	}
}

// TestMailboxRenameMovesMessages 验证重命名同步迁移邮件。
func TestMailboxRenameMovesMessages(t *testing.T) {
	s, gdb := newMailboxTestStore(t)
	if err := s.EnsureSystem(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(&db.Mailbox{UserID: 1, Name: "旧名字", IsSubscribed: true}); err != nil {
		t.Fatal(err)
	}
	msg := &db.Message{UserID: 1, Folder: "旧名字", FromAddr: "a@b", Subject: "s"}
	if err := gdb.Create(msg).Error; err != nil {
		t.Fatal(err)
	}

	if err := s.Rename(1, "旧名字", "新名字"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	var count int64
	if err := gdb.Model(&db.Message{}).
		Where("user_id = 1 AND folder = ?", "新名字").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("messages in 新名字 = %d, want 1", count)
	}
	if _, err := s.GetByName(1, "旧名字"); err == nil {
		t.Fatal("old mailbox name should be gone")
	}
}

// TestMailboxSubscribed 验证订阅状态写入。
func TestMailboxSubscribed(t *testing.T) {
	s, _ := newMailboxTestStore(t)
	if err := s.EnsureSystem(1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscribed(1, "Trash", false); err != nil {
		t.Fatalf("SetSubscribed: %v", err)
	}
	mb, err := s.GetByName(1, "Trash")
	if err != nil {
		t.Fatal(err)
	}
	if mb.IsSubscribed {
		t.Fatal("Trash should be unsubscribed")
	}
}
