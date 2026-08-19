package store

import (
	"time"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// OutboundStore defines the interface for outbound queue operations.
type OutboundStore interface {
	Create(msg *db.OutboundMessage) error
	GetByID(id uint) (*db.OutboundMessage, error)
	ListDue(now time.Time, limit int) ([]db.OutboundMessage, error)
	List(page, size int, status string) ([]db.OutboundMessage, int64, error)
	Update(msg *db.OutboundMessage) error
	// Claim 原子地将一项待投递邮件置为 sending；仅当该项仍处于
	// pending/deferred 时成功（并发 worker 抢占，防重复投递）。
	Claim(id uint) (bool, error)
	Delete(id uint) error
	CountByStatus(status string) (int64, error)
}

// outboundStoreGorm implements OutboundStore using GORM.
type outboundStoreGorm struct {
	db *gorm.DB
}

// newOutboundStore creates a new GORM-backed OutboundStore.
func newOutboundStore(database *gorm.DB) OutboundStore {
	return &outboundStoreGorm{db: database}
}

// Create inserts a new outbound queue record.
func (s *outboundStoreGorm) Create(msg *db.OutboundMessage) error {
	return s.db.Create(msg).Error
}

// GetByID retrieves an outbound queue record by primary key.
func (s *outboundStoreGorm) GetByID(id uint) (*db.OutboundMessage, error) {
	var msg db.OutboundMessage
	if err := s.db.First(&msg, id).Error; err != nil {
		return nil, err
	}
	return &msg, nil
}

// ListDue retrieves messages that are due for a delivery attempt.
func (s *outboundStoreGorm) ListDue(now time.Time, limit int) ([]db.OutboundMessage, error) {
	var msgs []db.OutboundMessage
	if err := s.db.
		Where("status IN (?, ?) AND next_attempt_at <= ?", db.OutboundStatusPending, db.OutboundStatusDeferred, now).
		Order("next_attempt_at ASC").
		Limit(limit).
		Find(&msgs).Error; err != nil {
		return nil, err
	}
	return msgs, nil
}

// List retrieves a paginated list of outbound messages, optionally filtered by status.
func (s *outboundStoreGorm) List(page, size int, status string) ([]db.OutboundMessage, int64, error) {
	var msgs []db.OutboundMessage
	var total int64

	query := s.db.Model(&db.OutboundMessage{})
	if status != "" {
		query = query.Where("status = ?", status)
	}
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if status != "" {
		err := s.db.Where("status = ?", status).Order("id DESC").Offset(offset).Limit(size).Find(&msgs).Error
		return msgs, total, err
	}
	if err := s.db.Order("id DESC").Offset(offset).Limit(size).Find(&msgs).Error; err != nil {
		return nil, 0, err
	}
	return msgs, total, nil
}

// Update saves changes to an existing outbound queue record.
func (s *outboundStoreGorm) Update(msg *db.OutboundMessage) error {
	return s.db.Save(msg).Error
}

// Claim 原子抢占：把 pending/deferred 项置为 sending。
// 返回是否抢占成功（false 表示已被其他 worker 抢先或状态已变化）。
func (s *outboundStoreGorm) Claim(id uint) (bool, error) {
	res := s.db.Model(&db.OutboundMessage{}).
		Where("id = ? AND status IN (?, ?)", id, db.OutboundStatusPending, db.OutboundStatusDeferred).
		Update("status", db.OutboundStatusSending)
	return res.RowsAffected == 1, res.Error
}

// Delete removes an outbound queue record by ID.
func (s *outboundStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.OutboundMessage{}, id).Error
}

// CountByStatus returns the number of outbound messages in a given status.
func (s *outboundStoreGorm) CountByStatus(status string) (int64, error) {
	var count int64
	if err := s.db.Model(&db.OutboundMessage{}).Where("status = ?", status).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}
