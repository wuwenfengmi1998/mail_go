package store

import (
	"errors"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// Common store errors
var (
	ErrInvalidEmail       = errors.New("无效的邮箱地址格式")
	ErrInvalidCredentials = errors.New("用户名或密码错误")
	ErrUserInactive       = errors.New("用户已被禁用")
	ErrRecordNotFound     = errors.New("记录不存在")
)

// MailStore defines the interface for mail message operations.
type MailStore interface {
	Create(msg *db.Message) error
	GetByID(id uint) (*db.Message, error)
	ListByUserAndFolder(userID uint, folder string, page, size int) ([]db.Message, int64, error)
	ListAllByUserAndFolder(userID uint, folder string) ([]db.Message, error)
	CountByUserAndFolder(userID uint, folder string) (int64, error)
	MarkRead(id uint) error
	MarkFlagged(id uint, flagged bool) error
	MoveToFolder(id uint, folder string) error
	Delete(id uint) error
	CountUnread(userID uint, folder string) (int64, error)
}

// mailStoreGorm implements MailStore using GORM.
type mailStoreGorm struct {
	db *gorm.DB
}

// newMailStore creates a new GORM-backed MailStore.
func newMailStore(database *gorm.DB) MailStore {
	return &mailStoreGorm{db: database}
}

// Create inserts a new message record.
func (s *mailStoreGorm) Create(msg *db.Message) error {
	return s.db.Create(msg).Error
}

// GetByID retrieves a message by primary key.
func (s *mailStoreGorm) GetByID(id uint) (*db.Message, error) {
	var msg db.Message
	if err := s.db.First(&msg, id).Error; err != nil {
		return nil, err
	}
	return &msg, nil
}

// ListByUserAndFolder retrieves a paginated list of messages for a user and folder.
func (s *mailStoreGorm) ListByUserAndFolder(userID uint, folder string, page, size int) ([]db.Message, int64, error) {
	var messages []db.Message
	var total int64

	query := s.db.Where("user_id = ? AND folder = ?", userID, folder)
	if err := query.Model(&db.Message{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := query.Order("date DESC").Offset(offset).Limit(size).Find(&messages).Error; err != nil {
		return nil, 0, err
	}
	return messages, total, nil
}

// MarkRead sets the IsRead flag to true for a message.
func (s *mailStoreGorm) MarkRead(id uint) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("is_read", true).Error
}

// MarkFlagged sets the IsFlagged flag for a message.
func (s *mailStoreGorm) MarkFlagged(id uint, flagged bool) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("is_flagged", flagged).Error
}

// MoveToFolder changes the folder of a message.
func (s *mailStoreGorm) MoveToFolder(id uint, folder string) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("folder", folder).Error
}

// Delete removes a message by ID.
func (s *mailStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.Message{}, id).Error
}

// CountUnread returns the count of unread messages for a user in a folder.
func (s *mailStoreGorm) CountUnread(userID uint, folder string) (int64, error) {
	var count int64
	if err := s.db.Model(&db.Message{}).
		Where("user_id = ? AND folder = ? AND is_read = ?", userID, folder, false).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// ListAllByUserAndFolder retrieves all messages for a user in a folder without pagination.
// Messages are ordered by ID ascending so that sequence numbers are stable.
func (s *mailStoreGorm) ListAllByUserAndFolder(userID uint, folder string) ([]db.Message, error) {
	var messages []db.Message
	if err := s.db.Where("user_id = ? AND folder = ?", userID, folder).
		Order("id ASC").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// CountByUserAndFolder returns the total count of messages for a user in a folder.
func (s *mailStoreGorm) CountByUserAndFolder(userID uint, folder string) (int64, error) {
	var count int64
	if err := s.db.Model(&db.Message{}).
		Where("user_id = ? AND folder = ?", userID, folder).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
