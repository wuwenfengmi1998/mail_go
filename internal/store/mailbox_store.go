package store

import (
	"errors"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// Mailbox store errors（英文文案：IMAP 响应直接使用，Web 侧另行提示）。
var (
	ErrMailboxNotFound = errors.New("No such mailbox")
	ErrMailboxExists   = errors.New("mailbox already exists")
	ErrMailboxInvalid  = errors.New("invalid mailbox name")
	ErrMailboxSystem   = errors.New("system mailbox cannot be deleted or renamed")
	ErrMailboxNotEmpty = errors.New("mailbox is not empty")
)

// MailboxStore defines the interface for mailbox (folder) operations.
type MailboxStore interface {
	// EnsureSystem 幂等创建用户的系统文件夹（INBOX/Sent/Drafts/Trash）。
	EnsureSystem(userID uint) error
	// List 返回用户全部文件夹：系统文件夹按规范顺序在前，
	// 自定义文件夹按名称升序在后。
	List(userID uint) ([]db.Mailbox, error)
	GetByName(userID uint, name string) (*db.Mailbox, error)
	Create(mb *db.Mailbox) error
	// Delete 删除空的自定义文件夹（含对应的 mailbox_states 记录）。
	Delete(userID uint, name string) error
	// Rename 重命名文件夹并同步迁移其邮件与 mailbox_states 记录。
	Rename(userID uint, oldName, newName string) error
	SetSubscribed(userID uint, name string, subscribed bool) error
}

// mailboxStoreGorm implements MailboxStore using GORM.
type mailboxStoreGorm struct {
	db *gorm.DB
}

// newMailboxStore creates a new GORM-backed MailboxStore.
func newMailboxStore(database *gorm.DB) MailboxStore {
	return &mailboxStoreGorm{db: database}
}

// isSystemMailboxName reports whether name is one of the system mailboxes.
func isSystemMailboxName(name string) bool {
	for _, def := range db.SystemMailboxes {
		if def.Name == name {
			return true
		}
	}
	return false
}

// EnsureSystem creates missing system mailboxes for the user.
// 已存在的行不覆盖：用户此前若已退订某系统文件夹（LSUB），状态保留。
func (s *mailboxStoreGorm) EnsureSystem(userID uint) error {
	var existing []db.Mailbox
	if err := s.db.Where("user_id = ?", userID).Find(&existing).Error; err != nil {
		return err
	}
	have := make(map[string]bool, len(existing))
	for _, mb := range existing {
		have[mb.Name] = true
	}
	for _, def := range db.SystemMailboxes {
		if have[def.Name] {
			continue
		}
		mb := &db.Mailbox{
			UserID:       userID,
			Name:         def.Name,
			SpecialUse:   def.SpecialUse,
			IsSubscribed: true,
		}
		if err := s.db.Create(mb).Error; err != nil {
			return err
		}
	}
	return nil
}

// List returns all folders for a user with system folders first.
func (s *mailboxStoreGorm) List(userID uint) ([]db.Mailbox, error) {
	var mbs []db.Mailbox
	if err := s.db.Where("user_id = ?", userID).Order("name").Find(&mbs).Error; err != nil {
		return nil, err
	}
	byName := make(map[string]db.Mailbox, len(mbs))
	for _, mb := range mbs {
		byName[mb.Name] = mb
	}
	sys := make([]db.Mailbox, 0, len(db.SystemMailboxes))
	for _, def := range db.SystemMailboxes {
		if mb, ok := byName[def.Name]; ok {
			sys = append(sys, mb)
		}
	}
	custom := make([]db.Mailbox, 0, len(mbs))
	for _, mb := range mbs {
		if !isSystemMailboxName(mb.Name) {
			custom = append(custom, mb)
		}
	}
	return append(sys, custom...), nil
}

// GetByName returns a folder by its exact name.
func (s *mailboxStoreGorm) GetByName(userID uint, name string) (*db.Mailbox, error) {
	var mb db.Mailbox
	if err := s.db.Where("user_id = ? AND name = ?", userID, name).First(&mb).Error; err != nil {
		return nil, err
	}
	return &mb, nil
}

// Create inserts a new mailbox row.
func (s *mailboxStoreGorm) Create(mb *db.Mailbox) error {
	return s.db.Create(mb).Error
}

// Delete removes an empty mailbox and its mailbox_states record.
func (s *mailboxStoreGorm) Delete(userID uint, name string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&db.Message{}).
			Where("user_id = ? AND folder = ?", userID, name).
			Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return ErrMailboxNotEmpty
		}
		if err := tx.Where("user_id = ? AND name = ?", userID, name).
			Delete(&db.Mailbox{}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND folder = ?", userID, name).
			Delete(&db.MailboxState{}).Error
	})
}

// Rename renames a mailbox, moving its messages along with it.
func (s *mailboxStoreGorm) Rename(userID uint, oldName, newName string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&db.Mailbox{}).
			Where("user_id = ? AND name = ?", userID, oldName).
			Update("name", newName).Error; err != nil {
			return err
		}
		if err := tx.Model(&db.Message{}).
			Where("user_id = ? AND folder = ?", userID, oldName).
			Update("folder", newName).Error; err != nil {
			return err
		}
		// 旧 UIDVALIDITY 不再适用：删除状态记录，下次访问重新生成，
		// 客户端据此丢弃缓存并全量重同步。
		return tx.Where("user_id = ? AND folder = ?", userID, oldName).
			Delete(&db.MailboxState{}).Error
	})
}

// SetSubscribed updates the subscription flag of a mailbox.
func (s *mailboxStoreGorm) SetSubscribed(userID uint, name string, subscribed bool) error {
	return s.db.Model(&db.Mailbox{}).
		Where("user_id = ? AND name = ?", userID, name).
		Update("is_subscribed", subscribed).Error
}
