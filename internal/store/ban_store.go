package store

import (
	"time"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// BanStore defines the interface for IP ban operations.
type BanStore interface {
	Create(entry *db.BanEntry) error
	GetByIP(ip string) (*db.BanEntry, error)
	Delete(id uint) error
	List(page, size int) ([]db.BanEntry, int64, error)
	IsBanned(ip string) (bool, *db.BanEntry)
	IncrementFail(ip string) (int, error)
	ResetFail(ip string) error
	Cleanup() error
}

// banStoreGorm implements BanStore using GORM.
type banStoreGorm struct {
	db *gorm.DB
}

// newBanStore creates a new GORM-backed BanStore.
func newBanStore(database *gorm.DB) BanStore {
	return &banStoreGorm{db: database}
}

// Create inserts a new ban entry record.
func (s *banStoreGorm) Create(entry *db.BanEntry) error {
	return s.db.Create(entry).Error
}

// GetByIP retrieves the most recent ban entry for a given IP address.
func (s *banStoreGorm) GetByIP(ip string) (*db.BanEntry, error) {
	var entry db.BanEntry
	if err := s.db.Where("ip_address = ?", ip).Order("id DESC").First(&entry).Error; err != nil {
		return nil, err
	}
	return &entry, nil
}

// Delete removes a ban entry by ID.
func (s *banStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.BanEntry{}, id).Error
}

// List retrieves a paginated list of ban entries.
func (s *banStoreGorm) List(page, size int) ([]db.BanEntry, int64, error) {
	var entries []db.BanEntry
	var total int64

	if err := s.db.Model(&db.BanEntry{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Order("id DESC").Offset(offset).Limit(size).Find(&entries).Error; err != nil {
		return nil, 0, err
	}
	return entries, total, nil
}

// IsBanned checks whether an IP address is currently banned.
// An IP is considered banned if there is a record with expires_at in the future.
func (s *banStoreGorm) IsBanned(ip string) (bool, *db.BanEntry) {
	var entry db.BanEntry
	if err := s.db.Where("ip_address = ? AND expires_at > ?", ip, time.Now()).First(&entry).Error; err != nil {
		return false, nil
	}
	return true, &entry
}

// IncrementFail increments the fail count for an IP address.
// If no record exists, it creates one with fail_count=1 and a zero expires_at.
// Returns the updated fail count.
func (s *banStoreGorm) IncrementFail(ip string) (int, error) {
	var entry db.BanEntry
	err := s.db.Where("ip_address = ?", ip).First(&entry).Error
	if err != nil {
		// No record exists, create a new one
		entry = db.BanEntry{
			IPAddress: ip,
			FailCount: 1,
			ExpiresAt: time.Time{}, // Zero time, not yet banned
		}
		if createErr := s.db.Create(&entry).Error; createErr != nil {
			return 0, createErr
		}
		return 1, nil
	}

	// Record exists, increment fail count
	newCount := entry.FailCount + 1
	if updateErr := s.db.Model(&entry).Update("fail_count", newCount).Error; updateErr != nil {
		return 0, updateErr
	}
	return newCount, nil
}

// ResetFail resets the fail count for an IP address by deleting its record.
func (s *banStoreGorm) ResetFail(ip string) error {
	return s.db.Where("ip_address = ?", ip).Delete(&db.BanEntry{}).Error
}

// Cleanup removes expired ban entries.
// It deletes records where expires_at is in the past and is not zero
// (preserving records that have fail counts but are not yet banned).
func (s *banStoreGorm) Cleanup() error {
	return s.db.Where("expires_at < ? AND expires_at > ?", time.Now(), time.Time{}).Delete(&db.BanEntry{}).Error
}
