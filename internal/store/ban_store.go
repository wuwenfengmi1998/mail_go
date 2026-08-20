package store

import (
	"fmt"
	"strings"
	"time"

	"mail_go/internal/db"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 阶段封禁档位（分钟/天），从第 4 次触发阈值开始封禁：
// 第 4 次 = ban_duration_min（默认 30 分钟）→ 第 5 次 = 3 小时 →
// 第 6 次 = 3 个月 → 第 7 次起 = 半年（上限）。
const (
	// freeTriggers 达到失败阈值但暂不封禁的触发次数（前 3 次只计数）。
	freeTriggers = 3
	banStage2Min = 3 * 60 // 3 小时
	banStage3Day = 90     // 3 个月
	banStage4Day = 180    // 半年（上限）
	banMaxDay    = banStage4Day
)

// stageDuration 返回第 banCount 次封禁（banCount 从 1 开始）的时长。
// firstBanMin 是第一次封禁的分钟数（来自配置 [ban] ban_duration_min）。
func stageDuration(banCount int, firstBanMin int) time.Duration {
	switch banCount {
	case 1:
		if firstBanMin <= 0 {
			firstBanMin = 30
		}
		return time.Duration(firstBanMin) * time.Minute
	case 2:
		return time.Duration(banStage2Min) * time.Minute
	case 3:
		return time.Duration(banStage3Day) * 24 * time.Hour
	default:
		return time.Duration(banMaxDay) * 24 * time.Hour
	}
}

// BanStore defines the interface for IP ban operations.
type BanStore interface {
	Create(entry *db.BanEntry) error
	// BanIP 手动/直接封禁某 IP 指定时长：该 IP 只保留一条记录
	// （既有观察记录一并清理，计数清零），避免产生重复行导致
	// IncrementFail 与 GetByIP 读写错位。
	BanIP(ip, reason string, duration time.Duration) error
	GetByIP(ip string) (*db.BanEntry, error)
	Update(entry *db.BanEntry) error
	Delete(id uint) error
	// List 返回已封禁或曾封禁的记录（不含仅计数未封禁的观察记录）。
	List(page, size int) ([]db.BanEntry, int64, error)
	IsBanned(ip string) (bool, *db.BanEntry)
	// IncrementFail 累加该 IP 的失败次数（无记录时创建），保留 BanCount。
	IncrementFail(ip string) (int, error)
	ResetFail(ip string) error
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

// BanIP 手动/直接封禁：事务内删除该 IP 的全部既有记录（含历史 bug
// 产生的重复行与观察计数记录）后插入一条封禁记录，计数清零。
// 与"管理员解封清零"语义一致：手动封禁视为对档位的重新评估。
func (s *banStoreGorm) BanIP(ip, reason string, duration time.Duration) error {
	if ip == "" {
		return fmt.Errorf("empty ip")
	}
	if duration <= 0 {
		return fmt.Errorf("invalid ban duration: %v", duration)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("ip_address = ?", ip).Delete(&db.BanEntry{}).Error; err != nil {
			return err
		}
		return tx.Create(&db.BanEntry{
			IPAddress: ip,
			Reason:    reason,
			FailCount: 0,
			BanCount:  0,
			ExpiresAt: time.Now().Add(duration),
		}).Error
	})
}

// GetByIP retrieves the most recent ban entry for a given IP address.
func (s *banStoreGorm) GetByIP(ip string) (*db.BanEntry, error) {
	var entry db.BanEntry
	if err := s.db.Where("ip_address = ?", ip).Order("id DESC").First(&entry).Error; err != nil {
		return nil, err
	}
	return &entry, nil
}

// Update saves changes to an existing ban entry record.
func (s *banStoreGorm) Update(entry *db.BanEntry) error {
	return s.db.Save(entry).Error
}

// Delete removes a ban entry by ID.
func (s *banStoreGorm) Delete(id uint) error {
	return s.db.Delete(&db.BanEntry{}, id).Error
}

// banEpochSentinel 用于区分“未封禁的计数记录”（expires_at 为零值）：
// 所有实际封禁记录的到期时间都晚于 2000 年。
var banEpochSentinel = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// List retrieves a paginated list of ban entries that are or have been
// banned (expires_at set). 仅计数的观察记录（零到期时间、无原因）不返回。
func (s *banStoreGorm) List(page, size int) ([]db.BanEntry, int64, error) {
	var entries []db.BanEntry
	var total int64

	query := s.db.Model(&db.BanEntry{}).Where("expires_at > ?", banEpochSentinel)
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := query.Order("id DESC").Offset(offset).Limit(size).Find(&entries).Error; err != nil {
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

// IncrementFail increments the fail count for an IP address atomically
// (SQL-side increment, avoiding read-modify-write races). If no record
// exists it creates one with fail_count=1, ban_count=0 and a zero
// expires_at (not yet banned); a concurrent creator wins and the loser's
// insert becomes a no-op via the unique index. Existing BanCount is
// preserved. Returns the current fail count.
func (s *banStoreGorm) IncrementFail(ip string) (int, error) {
	res := s.db.Model(&db.BanEntry{}).
		Where("ip_address = ?", ip).
		Update("fail_count", gorm.Expr("fail_count + 1"))
	if res.Error != nil {
		return 0, res.Error
	}
	if res.RowsAffected == 0 {
		// 无记录：插入首条；ip_address 唯一索引下并发插入用
		// OnConflict DoNothing 兜底，失败方继续走下面的回读。
		err := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&db.BanEntry{
			IPAddress: ip,
			FailCount: 1,
			BanCount:  0,
			ExpiresAt: time.Time{}, // Zero time, not yet banned
		}).Error
		if err != nil && !isUniqueConflictErr(err) {
			return 0, err
		}
	}

	// 回读计数（并发下取数据库最终值）
	var count int64
	if err := s.db.Model(&db.BanEntry{}).Where("ip_address = ?", ip).
		Select("fail_count").Scan(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

// isUniqueConflictErr 判断是否为唯一约束冲突（并发插入竞态的预期结果）。
func isUniqueConflictErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint") || // SQLite
		strings.Contains(msg, "Duplicate entry") || // MySQL
		strings.Contains(msg, "unique constraint") // generic
}

// ResetFail resets the fail count for an IP address by deleting its record.
// 成功登录（或管理员解封）后调用：封禁档位历史随之清零。
func (s *banStoreGorm) ResetFail(ip string) error {
	return s.db.Where("ip_address = ?", ip).Delete(&db.BanEntry{}).Error
}
