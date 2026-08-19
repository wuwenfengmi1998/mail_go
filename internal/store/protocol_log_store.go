package store

import (
	"time"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// ProtocolLogFilter holds optional filters for listing protocol logs.
type ProtocolLogFilter struct {
	Protocol string    // smtp | imap | pop3，空表示全部
	Success  *bool     // nil 表示全部
	IP       string    // 客户端 IP 模糊匹配
	Username string    // 用户名模糊匹配
	From     time.Time // 起（含）
	To       time.Time // 止（含）
}

// ProtocolLogStore defines the interface for protocol call log operations.
type ProtocolLogStore interface {
	Create(log *db.ProtocolLog) error
	// UpdateDuration 回填会话时长（登录后连接关闭时调用）。
	UpdateDuration(id uint, durationMs int64) error
	List(page, size int, filter ProtocolLogFilter) ([]db.ProtocolLog, int64, error)
	// CountStats 汇总各协议的失败/成功记录数（用于页面统计卡片）。
	CountStats(from time.Time) (map[string]map[string]int64, error)
	// CleanupBefore 删除 created_at 早于 before 的记录。
	CleanupBefore(before time.Time) (int64, error)
}

// protocolLogStoreGorm implements ProtocolLogStore using GORM.
type protocolLogStoreGorm struct {
	db *gorm.DB
}

// newProtocolLogStore creates a new GORM-backed ProtocolLogStore.
func newProtocolLogStore(database *gorm.DB) ProtocolLogStore {
	return &protocolLogStoreGorm{db: database}
}

// Create inserts a new protocol log record.
func (s *protocolLogStoreGorm) Create(log *db.ProtocolLog) error {
	return s.db.Create(log).Error
}

// UpdateDuration 回填会话时长，仅更新 duration_ms 字段。
func (s *protocolLogStoreGorm) UpdateDuration(id uint, durationMs int64) error {
	return s.db.Model(&db.ProtocolLog{}).Where("id = ?", id).Update("duration_ms", durationMs).Error
}

// List retrieves a paginated list of protocol logs, newest first.
func (s *protocolLogStoreGorm) List(page, size int, filter ProtocolLogFilter) ([]db.ProtocolLog, int64, error) {
	var logs []db.ProtocolLog
	var total int64

	query := s.db.Model(&db.ProtocolLog{})
	query = s.applyFilter(query, filter)

	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := query.Order("id DESC").Offset(offset).Limit(size).Find(&logs).Error; err != nil {
		return nil, 0, err
	}
	return logs, total, nil
}

func (s *protocolLogStoreGorm) applyFilter(query *gorm.DB, filter ProtocolLogFilter) *gorm.DB {
	if filter.Protocol != "" {
		query = query.Where("protocol = ?", filter.Protocol)
	}
	if filter.Success != nil {
		query = query.Where("success = ?", *filter.Success)
	}
	if filter.IP != "" {
		query = query.Where("client_ip LIKE ?", "%"+filter.IP+"%")
	}
	if filter.Username != "" {
		query = query.Where("username LIKE ?", "%"+filter.Username+"%")
	}
	if !filter.From.IsZero() {
		query = query.Where("created_at >= ?", filter.From)
	}
	if !filter.To.IsZero() {
		query = query.Where("created_at <= ?", filter.To)
	}
	return query
}

// CountStats 返回自 from 以来的记录数，按 protocol 再按 success 分组：
// map[protocol]map[successKey]count。successKey 为 "success"/"fail"。
func (s *protocolLogStoreGorm) CountStats(from time.Time) (map[string]map[string]int64, error) {
	stats := make(map[string]map[string]int64)
	for _, proto := range []string{db.ProtocolSMTP, db.ProtocolIMAP, db.ProtocolPOP3} {
		stats[proto] = map[string]int64{"success": 0, "fail": 0}
	}

	type row struct {
		Protocol string
		Success  bool
		Count    int64
	}
	var rows []row

	query := s.db.Model(&db.ProtocolLog{}).
		Select("protocol, success, COUNT(*) AS count").
		Group("protocol, success")
	if !from.IsZero() {
		query = query.Where("created_at >= ?", from)
	}
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}

	for _, r := range rows {
		proto := stats[r.Protocol]
		if proto == nil {
			proto = map[string]int64{"success": 0, "fail": 0}
			stats[r.Protocol] = proto
		}
		key := "fail"
		if r.Success {
			key = "success"
		}
		proto[key] = r.Count
	}
	return stats, nil
}

// CleanupBefore deletes records older than the given time and returns the
// number of deleted rows.
func (s *protocolLogStoreGorm) CleanupBefore(before time.Time) (int64, error) {
	res := s.db.Where("created_at < ?", before).Delete(&db.ProtocolLog{})
	return res.RowsAffected, res.Error
}
