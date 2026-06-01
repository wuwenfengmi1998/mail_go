package store

import (
	"mail_go/internal/db"

	"gorm.io/gorm"
)

// AttachmentStore defines the interface for attachment data operations.
type AttachmentStore interface {
	Create(att *db.Attachment) error
	GetByID(id uint) (*db.Attachment, error)
	ListByMessage(messageID uint) ([]db.Attachment, error)
	Delete(id uint) error
	DeleteByMessage(messageID uint) error
}

// attachmentStoreGorm implements AttachmentStore using GORM.
type attachmentStoreGorm struct {
	db *gorm.DB
}

// newAttachmentStore creates a new GORM-backed AttachmentStore.
func newAttachmentStore(database *gorm.DB) AttachmentStore {
	return &attachmentStoreGorm{db: database}
}

// Create inserts a new attachment record.
func (s *attachmentStoreGorm) Create(att *db.Attachment) error {
	return s.db.Create(att).Error
}

// GetByID retrieves an attachment by primary key.
func (s *attachmentStoreGorm) GetByID(id uint) (*db.Attachment, error) {
	var att db.Attachment
	if err := s.db.First(&att, id).Error; err != nil {
		return nil, err
	}
	return &att, nil
}

// ListByMessage retrieves all attachments for a given message.
func (s *attachmentStoreGorm) ListByMessage(messageID uint) ([]db.Attachment, error) {
	var attachments []db.Attachment
	if err := s.db.Where("message_id = ?", messageID).Find(&attachments).Error; err != nil {
		return nil, err
	}
	return attachments, nil
}

// Delete removes an attachment by ID.
func (s *attachmentStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.Attachment{}, id).Error
}

// DeleteByMessage removes all attachments for a given message.
func (s *attachmentStoreGorm) DeleteByMessage(messageID uint) error {
	return s.db.Where("message_id = ?", messageID).Delete(&db.Attachment{}).Error
}
