package store

import (
	"errors"
	"time"

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
	MaxIDByUserAndFolder(userID uint, folder string) (uint, error)
	MarkRead(id uint) error
	MarkReadState(id uint, read bool) error
	MarkFlagged(id uint, flagged bool) error
	// SetReadStates 批量设置多封邮件的已读状态（单条 UPDATE ... IN）。
	SetReadStates(ids []uint, read bool) error
	// SetFlaggedStates 批量设置多封邮件的星标状态（单条 UPDATE ... IN）。
	SetFlaggedStates(ids []uint, flagged bool) error
	MoveToFolder(id uint, folder string) error
	// SetDeletedStates 批量设置多封邮件的 \Deleted 标记（单条 UPDATE ... IN）。
	SetDeletedStates(ids []uint, deleted bool) error
	// ListDeletedByUserAndFolder 列出某文件夹中所有已标记 \Deleted 的邮件
	// （按 id ASC 排序，与全量列表一致，序号映射全链路相同）。
	ListDeletedByUserAndFolder(userID uint, folder string) ([]db.Message, error)
	// DeleteMany 批量硬删除多封邮件（单条 DELETE ... IN）。
	DeleteMany(ids []uint) error
	Delete(id uint) error
	CountUnread(userID uint, folder string) (int64, error)
	CountByFolder(folder string) (int64, error)
	CountAll() (int64, error)
	TotalSizeByFolder(folder string) (int64, error)
	TotalSize() (int64, error)
	CountByFolderSince(folder string, since time.Time) (int64, error)
	ListAll(page, size int) ([]db.Message, int64, error)
	ListAllByFolder(folder string, page, size int) ([]db.Message, int64, error)
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
	// 日期统一为 UTC 存储：date 列在 SQLite 中是文本，混合时区偏移
	// （+08:00/-04:00 等）会让 ORDER BY date 变成错误的字典序（Web 列表
	// 排序错乱、IMAP 序号与客户端日期视图不一致）。统一 UTC 后字典序
	// 即时间序，所有排序路径（Web/IMAP/seqOf）全链路一致。
	if msg != nil {
		msg.Date = msg.Date.UTC()
	}
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
	if err := query.Order("date DESC, id DESC").Offset(offset).Limit(size).Find(&messages).Error; err != nil {
		return nil, 0, err
	}
	return messages, total, nil
}

// MarkRead sets the IsRead flag to true for a message.
func (s *mailStoreGorm) MarkRead(id uint) error {
	return s.MarkReadState(id, true)
}

// MarkReadState sets the IsRead flag for a message.
func (s *mailStoreGorm) MarkReadState(id uint, read bool) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("is_read", read).Error
}

// MarkFlagged sets the IsFlagged flag for a message.
func (s *mailStoreGorm) MarkFlagged(id uint, flagged bool) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("is_flagged", flagged).Error
}

// SetReadStates 批量设置多封邮件的已读状态。
// 客户端整批标记已读（手机同步后 STORE +FLAGS \Seen）时，逐条 UPDATE
// 会产生大量写事务并占住连接，这里合并为单条 SQL。
func (s *mailStoreGorm) SetReadStates(ids []uint, read bool) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Model(&db.Message{}).Where("id IN ?", ids).Update("is_read", read).Error
}

// SetFlaggedStates 批量设置多封邮件的星标状态。
func (s *mailStoreGorm) SetFlaggedStates(ids []uint, flagged bool) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Model(&db.Message{}).Where("id IN ?", ids).Update("is_flagged", flagged).Error
}

// MoveToFolder changes the folder of a message.
func (s *mailStoreGorm) MoveToFolder(id uint, folder string) error {
	return s.db.Model(&db.Message{}).Where("id = ?", id).Update("folder", folder).Error
}

// SetDeletedStates 批量设置多封邮件的 \Deleted 标记。
func (s *mailStoreGorm) SetDeletedStates(ids []uint, deleted bool) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Model(&db.Message{}).Where("id IN ?", ids).Update("is_deleted", deleted).Error
}

// ListDeletedByUserAndFolder 列出某文件夹中所有已标记 \Deleted 的邮件
// （按 id ASC 排序，与全量列表一致，序号映射全链路相同）。
func (s *mailStoreGorm) ListDeletedByUserAndFolder(userID uint, folder string) ([]db.Message, error) {
	var messages []db.Message
	if err := s.db.Where("user_id = ? AND folder = ? AND is_deleted = ?", userID, folder, true).
		Order("id ASC").Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

// DeleteMany 批量硬删除多封邮件。
func (s *mailStoreGorm) DeleteMany(ids []uint) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.Where("id IN ?", ids).Delete(&db.Message{}).Error
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
// 按 id ASC（到达顺序，最早在前）排序：新邮件永远获得最大序号（seq =
// EXISTS 数），与主流服务器（Dovecot/Courier）行为一致，依赖「新邮件 =
// seq N+1」做增量同步的客户端不会漏收或标错邮件；新邮件到达不会使既有
// 邮件序号位移（只有 EXPUNGE 才会，属正常行为）。所有 IMAP 序号相关路径
// （Status/Fetch/Search/Store/Copy/Move/Expunge/推送/seqOf）共用本排序，
// 保证序号全链路一致。INTERNALDATE 使用 CreatedAt（到达时间），与排序一致。
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

// MaxIDByUserAndFolder returns the highest message ID for a user folder.
func (s *mailStoreGorm) MaxIDByUserAndFolder(userID uint, folder string) (uint, error) {
	var maxID uint
	if err := s.db.Model(&db.Message{}).
		Where("user_id = ? AND folder = ?", userID, folder).
		Select("COALESCE(MAX(id), 0)").
		Scan(&maxID).Error; err != nil {
		return 0, err
	}
	return maxID, nil
}

// CountByFolder returns the total count of messages in a given folder.
func (s *mailStoreGorm) CountByFolder(folder string) (int64, error) {
	var count int64
	if err := s.db.Model(&db.Message{}).Where("folder = ?", folder).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// CountAll returns the total count of all messages.
func (s *mailStoreGorm) CountAll() (int64, error) {
	var count int64
	if err := s.db.Model(&db.Message{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// TotalSizeByFolder returns the total size (in bytes) of message bodies in a given folder.
func (s *mailStoreGorm) TotalSizeByFolder(folder string) (int64, error) {
	var total int64
	err := s.db.Model(&db.Message{}).
		Where("folder = ?", folder).
		Select("COALESCE(SUM(LENGTH(text_body) + LENGTH(html_body)), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// TotalSize returns the total size (in bytes) of all message bodies.
func (s *mailStoreGorm) TotalSize() (int64, error) {
	var total int64
	err := s.db.Model(&db.Message{}).
		Select("COALESCE(SUM(LENGTH(text_body) + LENGTH(html_body)), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// CountByFolderSince returns the count of messages in a folder since a given time.
func (s *mailStoreGorm) CountByFolderSince(folder string, since time.Time) (int64, error) {
	var count int64
	if err := s.db.Model(&db.Message{}).
		Where("folder = ? AND created_at >= ?", folder, since).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// ListAll retrieves a paginated list of all messages across all users.
func (s *mailStoreGorm) ListAll(page, size int) ([]db.Message, int64, error) {
	var messages []db.Message
	var total int64

	if err := s.db.Model(&db.Message{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Preload("User").Order("date DESC").Offset(offset).Limit(size).Find(&messages).Error; err != nil {
		return nil, 0, err
	}
	return messages, total, nil
}

// ListAllByFolder retrieves a paginated list of all messages in a given folder across all users.
func (s *mailStoreGorm) ListAllByFolder(folder string, page, size int) ([]db.Message, int64, error) {
	var messages []db.Message
	var total int64

	query := s.db.Where("folder = ?", folder)
	if err := query.Model(&db.Message{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * size
	if err := s.db.Preload("User").Where("folder = ?", folder).Order("date DESC").Offset(offset).Limit(size).Find(&messages).Error; err != nil {
		return nil, 0, err
	}
	return messages, total, nil
}
