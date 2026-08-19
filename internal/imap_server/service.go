package imap_server

// MailboxService 是邮箱（文件夹）与消息操作的服务层：
// IMAP 会话与 Web handler 共用同一份实现，保证「IMAP LIST 返回什么，
// Web 就显示什么」，且移动/删除等操作走同一语义（如移入 Trash）。

import (
	"log"
	"strings"

	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/v2"
)

// FolderInfo 是一个文件夹在列表页（IMAP LIST / Web 侧边栏）
// 展示所需的信息。
type FolderInfo struct {
	Name       string
	SpecialUse string
	Subscribed bool
	Total      int64
	Unseen     int64
}

// MailboxService 提供用户级邮箱操作。
type MailboxService struct {
	stores *store.Stores
}

// NewMailboxService creates a MailboxService backed by stores.
func NewMailboxService(stores *store.Stores) *MailboxService {
	return &MailboxService{stores: stores}
}

// ListAll 返回用户全部文件夹（先确保系统文件夹存在）。
func (s *MailboxService) ListAll(userID uint) ([]db.Mailbox, error) {
	if err := s.stores.Mailboxes.EnsureSystem(userID); err != nil {
		return nil, err
	}
	return s.stores.Mailboxes.List(userID)
}

// List 返回用户全部文件夹及统计信息（IMAP LIST / Web 侧边栏同源）。
func (s *MailboxService) List(userID uint) ([]FolderInfo, error) {
	mbs, err := s.ListAll(userID)
	if err != nil {
		return nil, err
	}
	infos := make([]FolderInfo, 0, len(mbs))
	for _, mb := range mbs {
		total, err := s.stores.Mails.CountByUserAndFolder(userID, mb.Name)
		if err != nil {
			log.Printf("mailbox: 统计 %s 邮件数失败: %v", mb.Name, err)
		}
		unseen, err := s.stores.Mails.CountUnread(userID, mb.Name)
		if err != nil {
			log.Printf("mailbox: 统计 %s 未读数失败: %v", mb.Name, err)
		}
		infos = append(infos, FolderInfo{
			Name:       mb.Name,
			SpecialUse: mb.SpecialUse,
			Subscribed: mb.IsSubscribed,
			Total:      total,
			Unseen:     unseen,
		})
	}
	return infos, nil
}

// Canonical 规范化邮箱名：INBOX 大小写不敏感，其余按 DB 实际名称
// 精确匹配。会先确保系统文件夹存在（客户端可能不 LIST 直接 SELECT）。
func (s *MailboxService) Canonical(userID uint, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if err := s.stores.Mailboxes.EnsureSystem(userID); err != nil {
		log.Printf("mailbox: EnsureSystem 失败 user=%d: %v", userID, err)
		return "", false
	}
	if strings.EqualFold(name, "INBOX") {
		return "INBOX", true
	}
	mb, err := s.stores.Mailboxes.GetByName(userID, name)
	if err != nil {
		return "", false
	}
	return mb.Name, true
}

// Messages 分页返回用户某文件夹的邮件。
func (s *MailboxService) Messages(userID uint, name string, page, size int) ([]db.Message, int64, error) {
	return s.stores.Mails.ListByUserAndFolder(userID, name, page, size)
}

// Select 计算 SELECT 响应数据（会话负责 hub 绑定与选中状态）。
func (s *MailboxService) Select(userID uint, name string) (*imap.SelectData, error) {
	msgs, err := s.stores.Mails.ListAllByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	maxID, err := s.stores.Mails.MaxIDByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	uidValidity, err := s.stores.MailboxState.UidValidity(userID, name)
	if err != nil {
		log.Printf("IMAP: 获取 UIDVALIDITY 失败 folder=%s: %v", name, err)
		uidValidity = 1
	}
	flags := []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagSeen, imap.FlagDraft}
	return &imap.SelectData{
		Flags:          flags,
		PermanentFlags: append(flags, imap.FlagWildcard),
		NumMessages:    uint32(len(msgs)),
		NumRecent:      0,
		UIDNext:        imap.UID(maxID + 1),
		UIDValidity:    uidValidity,
	}, nil
}

// Status 计算 STATUS 响应数据。
func (s *MailboxService) Status(userID uint, name string, options *imap.StatusOptions) (*imap.StatusData, error) {
	msgs, err := s.stores.Mails.ListAllByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	data := &imap.StatusData{Mailbox: name}

	if options.NumMessages || options.NumUnseen || options.NumDeleted || options.Size {
		var unseen, deleted uint32
		var size int64
		for i := range msgs {
			if !msgs[i].IsRead {
				unseen++
			}
			if msgs[i].IsDeleted {
				deleted++
			}
			size += int64(len(messageRawData(&msgs[i])))
		}
		if options.NumMessages {
			n := uint32(len(msgs))
			data.NumMessages = &n
		}
		if options.NumUnseen {
			data.NumUnseen = &unseen
		}
		if options.NumDeleted {
			data.NumDeleted = &deleted
		}
		if options.Size {
			data.Size = &size
		}
	}
	if options.NumRecent {
		zero := uint32(0)
		data.NumRecent = &zero
	}
	if options.UIDNext {
		maxID, err := s.stores.Mails.MaxIDByUserAndFolder(userID, name)
		if err != nil {
			return nil, err
		}
		data.UIDNext = imap.UID(maxID + 1)
	}
	if options.UIDValidity {
		uidValidity, err := s.stores.MailboxState.UidValidity(userID, name)
		if err != nil {
			return nil, err
		}
		data.UIDValidity = uidValidity
	}
	return data, nil
}

// validateMailboxName 校验自定义文件夹名（IMAP CREATE/RENAME 用）。
func validateMailboxName(name string) error {
	if name == "" || len(name) > 64 {
		return store.ErrMailboxInvalid
	}
	if strings.Contains(name, "/") || name == "." || name == ".." {
		return store.ErrMailboxInvalid
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return store.ErrMailboxInvalid
		}
	}
	return nil
}

// Create 创建自定义文件夹。
func (s *MailboxService) Create(userID uint, name string) error {
	name = strings.TrimSpace(name)
	if err := validateMailboxName(name); err != nil {
		return err
	}
	if strings.EqualFold(name, "INBOX") {
		return store.ErrMailboxExists
	}
	if err := s.stores.Mailboxes.EnsureSystem(userID); err != nil {
		return err
	}
	if _, err := s.stores.Mailboxes.GetByName(userID, name); err == nil {
		return store.ErrMailboxExists
	}
	return s.stores.Mailboxes.Create(&db.Mailbox{
		UserID:       userID,
		Name:         name,
		IsSubscribed: true,
	})
}

// Delete 删除空的自定义文件夹（系统文件夹拒绝）。
func (s *MailboxService) Delete(userID uint, name string) error {
	name, ok := s.Canonical(userID, name)
	if !ok {
		return store.ErrMailboxNotFound
	}
	if isSystemMailboxName(name) {
		return store.ErrMailboxSystem
	}
	return s.stores.Mailboxes.Delete(userID, name)
}

// Rename 重命名自定义文件夹（系统文件夹拒绝）。
func (s *MailboxService) Rename(userID uint, oldName, newName string) error {
	oldName, ok := s.Canonical(userID, oldName)
	if !ok {
		return store.ErrMailboxNotFound
	}
	if isSystemMailboxName(oldName) {
		return store.ErrMailboxSystem
	}
	newName = strings.TrimSpace(newName)
	if err := validateMailboxName(newName); err != nil {
		return err
	}
	if strings.EqualFold(newName, "INBOX") || isSystemMailboxName(newName) {
		return store.ErrMailboxInvalid
	}
	if _, err := s.stores.Mailboxes.GetByName(userID, newName); err == nil {
		return store.ErrMailboxExists
	}
	return s.stores.Mailboxes.Rename(userID, oldName, newName)
}

// SetSubscribed 更新文件夹订阅状态（LSUB 过滤用）。
func (s *MailboxService) SetSubscribed(userID uint, name string, subscribed bool) error {
	name, ok := s.Canonical(userID, name)
	if !ok {
		return store.ErrMailboxNotFound
	}
	return s.stores.Mailboxes.SetSubscribed(userID, name, subscribed)
}

// Move 把多封邮件移动到目标文件夹（web 删除=移入 Trash、恢复等共用）。
// 不属于该用户的邮件被跳过。
func (s *MailboxService) Move(userID uint, msgIDs []uint, dest string) error {
	if len(msgIDs) == 0 {
		return nil
	}
	if _, ok := s.Canonical(userID, dest); !ok {
		return store.ErrMailboxNotFound
	}
	for _, id := range msgIDs {
		msg, err := s.stores.Mails.GetByID(id)
		if err != nil || msg.UserID != userID {
			continue
		}
		if err := s.stores.Mails.MoveToFolder(id, dest); err != nil {
			return err
		}
	}
	return nil
}

// Purge 永久删除文件夹中的邮件；msgIDs 为空时删除全部。
// 返回被删除前的邮件列表（调用方据此推送 EXPUNGE）。
func (s *MailboxService) Purge(userID uint, name string, msgIDs []uint) ([]db.Message, error) {
	var msgs []db.Message
	var err error
	if len(msgIDs) == 0 {
		msgs, err = s.stores.Mails.ListAllByUserAndFolder(userID, name)
		if err != nil {
			return nil, err
		}
		msgIDs = make([]uint, 0, len(msgs))
		for i := range msgs {
			msgIDs = append(msgIDs, msgs[i].ID)
		}
	} else {
		for _, id := range msgIDs {
			msg, err := s.stores.Mails.GetByID(id)
			if err != nil || msg.UserID != userID || msg.Folder != name {
				continue
			}
			msgs = append(msgs, *msg)
		}
	}
	if len(msgIDs) == 0 {
		return nil, nil
	}
	if err := s.stores.Mails.DeleteMany(msgIDs); err != nil {
		return nil, err
	}
	return msgs, nil
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

// mailboxAttrs 把 SpecialUse 映射为 RFC 6154 的 IMAP 属性。
func mailboxAttrs(mb db.Mailbox) []imap.MailboxAttr {
	switch mb.SpecialUse {
	case "Sent":
		return []imap.MailboxAttr{imap.MailboxAttrSent}
	case "Drafts":
		return []imap.MailboxAttr{imap.MailboxAttrDrafts}
	case "Trash":
		return []imap.MailboxAttr{imap.MailboxAttrTrash}
	default:
		return nil
	}
}
