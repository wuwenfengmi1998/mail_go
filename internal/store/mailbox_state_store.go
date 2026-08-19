package store

import (
	"crypto/rand"
	"encoding/binary"

	"mail_go/internal/db"

	"gorm.io/gorm"
)

// MailboxStateStore 提供 IMAP 邮箱持久化状态（UIDVALIDITY）的存取。
type MailboxStateStore interface {
	// UidValidity 返回邮箱的持久化 UIDVALIDITY；首次访问时随机生成并落库。
	UidValidity(userID uint, folder string) (uint32, error)
}

type mailboxStateStoreGorm struct {
	db *gorm.DB
}

func newMailboxStateStore(database *gorm.DB) *mailboxStateStoreGorm {
	return &mailboxStateStoreGorm{db: database}
}

// UidValidity 返回邮箱的持久化 UIDVALIDITY；首次访问时随机生成并落库。
// 随机值保证：邮箱内容身份变化（如数据库重建导致消息 ID 空间变化）时，
// 新库生成的新值会让客户端丢弃旧缓存全量重同步（RFC 3501 UIDVALIDITY
// 语义）。绝不返回 0（0 不是合法 UIDVALIDITY）。
func (s *mailboxStateStoreGorm) UidValidity(userID uint, folder string) (uint32, error) {
	var st db.MailboxState
	err := s.db.Where("user_id = ? AND folder = ?", userID, folder).First(&st).Error
	if err == nil {
		if st.UidValidity != 0 {
			return st.UidValidity, nil
		}
	} else if err != gorm.ErrRecordNotFound {
		return 0, err
	}

	// 首次访问（或旧数据为 0）：随机生成并持久化
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return 0, err
	}
	v := binary.BigEndian.Uint32(buf[:])
	if v == 0 {
		v = 1
	}

	st = db.MailboxState{UserID: userID, Folder: folder, UidValidity: v}
	if err := s.db.Save(&st).Error; err != nil {
		return 0, err
	}
	return v, nil
}
