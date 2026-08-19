package imap_server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net/mail"
	"strings"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/mailutil"
	"mail_go/internal/store"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/emersion/go-imap/backend/backendutil"
	asgomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// ---------- imapBackend ----------

// imapBackend implements backend.Backend and backend.BackendUpdater.
type imapBackend struct {
	stores *store.Stores
	banCfg config.BanConfig
	port   int
	hub    *connhub.Hub

	// updates 承载新邮件等后端更新，由 go-imap 服务器广播给相关客户端。
	updates chan backend.Update
	// disconnectAddr 强制断开指定远端地址的连接（管理后台断开封禁用）。
	disconnectAddr func(addr string)
}

// Updates 实现 backend.BackendUpdater：新邮件推送通道（广播按用户名与
// 邮箱过滤，只送达已选中对应邮箱的客户端）。
func (b *imapBackend) Updates() <-chan backend.Update {
	return b.updates
}

// buildNewMessageUpdate 为一条新投递到 mailbox 的邮件构造 IMAP 更新。
// seq 取该邮件在邮箱中的实际序号（最新在前，通常为 1）。
func buildNewMessageUpdate(stores *store.Stores, userEmail, mailbox string, msg *db.Message) *backend.MessageUpdate {
	if stores == nil || msg == nil || userEmail == "" || mailbox == "" {
		return nil
	}
	seq := seqOf(stores, msg.UserID, mailbox, msg.ID)
	if seq == 0 {
		seq = 1
	}

	imapMsg := imap.NewMessage(seq, []imap.FetchItem{imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size, imap.FetchEnvelope})
	imapMsg.Uid = uint32(msg.ID)
	imapMsg.Flags = flagsOf(msg.IsRead, msg.IsFlagged, false)
	imapMsg.InternalDate = msg.Date
	imapMsg.Size = uint32(len(msg.RawData))
	imapMsg.Envelope = &imap.Envelope{
		Date:      msg.Date,
		Subject:   msg.Subject,
		From:      parseAddressList(msg.FromAddr),
		Sender:    parseAddressList(msg.FromAddr),
		ReplyTo:   parseAddressList(msg.FromAddr),
		To:        parseAddressList(msg.ToAddr),
		Cc:        parseAddressList(msg.CcAddr),
		MessageId: msg.MessageID,
	}

	return &backend.MessageUpdate{
		Update:  backend.NewUpdate(userEmail, mailbox),
		Message: imapMsg,
	}
}

// buildFlagsUpdate 为一条消息的标志变化构造 IMAP 更新（已读/星标/删除标记）。
// deleted 为会话内 \Deleted 标记（IMAP STORE 会话状态，不入库）。
func buildFlagsUpdate(stores *store.Stores, userEmail, mailbox string, msg *db.Message, deleted bool) *backend.MessageUpdate {
	if stores == nil || msg == nil || userEmail == "" || mailbox == "" {
		return nil
	}
	imapMsg := imap.NewMessage(seqOf(stores, msg.UserID, mailbox, msg.ID),
		[]imap.FetchItem{imap.FetchUid, imap.FetchFlags})
	imapMsg.Uid = uint32(msg.ID)
	imapMsg.Flags = flagsOf(msg.IsRead, msg.IsFlagged, deleted)
	return &backend.MessageUpdate{
		Update:  backend.NewUpdate(userEmail, mailbox),
		Message: imapMsg,
	}
}

// seqOf 返回消息在文件夹中的序号（1 基），未找到返回 0。
func seqOf(stores *store.Stores, userID uint, mailbox string, msgID uint) uint32 {
	msgs, err := stores.Mails.ListAllByUserAndFolder(userID, mailbox)
	if err != nil {
		return 0
	}
	for i := range msgs {
		if msgs[i].ID == msgID {
			return uint32(i + 1)
		}
	}
	return 0
}

// flagsOf 按数据库状态生成 IMAP 标志列表（deleted 为会话内 \Deleted 标记）。
func flagsOf(read, flagged, deleted bool) []string {
	flags := make([]string, 0, 3)
	if read {
		flags = append(flags, "\\Seen")
	}
	if flagged {
		flags = append(flags, "\\Flagged")
	}
	if deleted {
		flags = append(flags, "\\Deleted")
	}
	return flags
}

// pushUpdate 非阻塞地把一条后端更新送入推送通道（满则丢弃并记日志）。
func pushUpdate(ch chan backend.Update, u backend.Update) {
	if ch == nil {
		return
	}
	select {
	case ch <- u:
	default:
		log.Printf("IMAP: 推送通道已满，丢弃更新")
	}
}

// Login authenticates a user by email and password.
func (b *imapBackend) Login(connInfo *imap.ConnInfo, username, password string) (backend.User, error) {
	clientIP := store.ClientIPFromAddr(connInfo.RemoteAddr)
	now := time.Now()

	// 已封禁 IP 一律拒绝认证（防协议层暴力破解）
	if banned, _ := b.stores.Bans.IsBanned(clientIP); banned {
		b.recordLogin(clientIP, username, false, "IP已被封禁", "认证被拒绝（IP 已封禁）", 0, now)
		return nil, backend.ErrInvalidCredentials
	}

	user, err := b.stores.Users.Authenticate(username, password)
	if err != nil {
		// 认证失败计数，达到阈值按档位封禁（与 Web 登录共用 ban_entries）
		b.stores.RecordAuthFailure(clientIP, b.banCfg.MaxFailAttempts, b.banCfg.BanDurationMin, "邮件协议认证失败次数过多")
		b.recordLogin(clientIP, username, false, "用户名或密码错误", "LOGIN 失败", 0, now)
		return nil, fmt.Errorf("invalid credentials: %w", err)
	}

	email := user.Username + "@"
	domain, err := b.stores.Domains.GetByID(user.DomainID)
	if err == nil {
		email = user.Username + "@" + domain.Name
	}

	logID := b.recordLogin(clientIP, username, true, "", "LOGIN 成功", 0, now)

	// 连接追踪：注册到当前连接中心，Logout 时注销；
	// 注册强制断开回调（按远端地址匹配，断开时由 go-imap 走正常收尾）。
	conn := b.hub.Register("imap", clientIP, b.port, connInfo.TLS != nil)
	if conn != nil {
		conn.SetUser(email)
		remoteAddr := ""
		if connInfo.RemoteAddr != nil {
			remoteAddr = connInfo.RemoteAddr.String()
		}
		if b.disconnectAddr != nil && remoteAddr != "" {
			addr := remoteAddr
			conn.SetDisconnect(func() { b.disconnectAddr(addr) })
		}
	}

	return &imapUser{
		stores:    b.stores,
		id:        user.ID,
		email:     email,
		logID:     logID,
		clientIP:  clientIP,
		startedAt: now,
		conn:      conn,
		updates:   b.updates,
	}, nil
}

// recordLogin 写入一条 IMAP 登录日志，返回新记录的 ID（失败时为 0）。
func (b *imapBackend) recordLogin(ip, username string, success bool, failReason, detail string, durationMs int64, at time.Time) uint {
	entry := &db.ProtocolLog{
		Protocol:   db.ProtocolIMAP,
		Port:       b.port,
		ClientIP:   ip,
		Username:   username,
		Success:    success,
		FailReason: failReason,
		Detail:     detail,
		DurationMs: durationMs,
		CreatedAt:  at,
	}
	if err := b.stores.ProtocolLogs.Create(entry); err != nil {
		log.Printf("IMAP: 写入协议日志失败: %v", err)
		return 0
	}
	return entry.ID
}

// ---------- imapUser ----------

// imapUser implements backend.User.
type imapUser struct {
	stores    *store.Stores
	id        uint
	email     string
	logID     uint
	clientIP  string
	startedAt time.Time
	conn      *connhub.Conn
	// updates 所在 backend 的推送通道（STORE/EXPUNGE 等实时同步用）。
	updates chan backend.Update
}

// Username returns the user's email address.
func (u *imapUser) Username() string {
	return u.email
}

// ListMailboxes returns the standard mailbox list.
func (u *imapUser) ListMailboxes(subscribed bool) ([]backend.Mailbox, error) {
	folders := []struct {
		name       string
		delimiter  string
		attributes []string
	}{
		{"INBOX", "/", nil},
		{"Sent", "/", []string{"\\Sent"}},
		{"Drafts", "/", []string{"\\Drafts"}},
		{"Trash", "/", []string{"\\Trash"}},
	}

	mailboxes := make([]backend.Mailbox, 0, len(folders))
	for _, f := range folders {
		mailboxes = append(mailboxes, &imapMailbox{
			stores:     u.stores,
			user:       u,
			name:       f.name,
			delimiter:  f.delimiter,
			attributes: f.attributes,
		})
	}
	return mailboxes, nil
}

// GetMailbox returns a mailbox by name.
func (u *imapUser) GetMailbox(name string) (backend.Mailbox, error) {
	normalized, ok := canonicalMailboxName(name)
	if !ok {
		return nil, backend.ErrNoSuchMailbox
	}

	return &imapMailbox{
		stores:    u.stores,
		user:      u,
		name:      normalized,
		delimiter: "/",
	}, nil
}

func canonicalMailboxName(name string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INBOX":
		return "INBOX", true
	case "SENT":
		return "Sent", true
	case "DRAFTS":
		return "Drafts", true
	case "TRASH":
		return "Trash", true
	default:
		return "", false
	}
}

// CreateMailbox creates a new mailbox (not supported in this version).
func (u *imapUser) CreateMailbox(name string) error {
	return fmt.Errorf("mailbox creation not supported")
}

// DeleteMailbox deletes a mailbox (not supported in this version).
func (u *imapUser) DeleteMailbox(name string) error {
	return fmt.Errorf("mailbox deletion not supported")
}

// RenameMailbox renames a mailbox (not supported in this version).
func (u *imapUser) RenameMailbox(existingName, newName string) error {
	return fmt.Errorf("mailbox rename not supported")
}

// Logout is called when the user session ends.
func (u *imapUser) Logout() error {
	// 回填会话时长，登录记录在 Login 时已写入
	if u.logID == 0 {
		return nil
	}
	durationMs := time.Since(u.startedAt).Milliseconds()
	if err := u.stores.ProtocolLogs.UpdateDuration(u.logID, durationMs); err != nil {
		log.Printf("IMAP: 更新协议日志失败: %v", err)
	}
	u.conn.Close()
	return nil
}

// ---------- imapMailbox ----------

// imapMailbox implements backend.Mailbox.
type imapMailbox struct {
	stores     *store.Stores
	user       *imapUser
	name       string
	delimiter  string
	attributes []string
	// deleted tracks messages marked as \Deleted in this session
	deleted map[uint]bool
}

// Name returns the mailbox name.
func (m *imapMailbox) Name() string {
	return m.name
}

// Info returns mailbox metadata.
func (m *imapMailbox) Info() (*imap.MailboxInfo, error) {
	attrs := m.attributes
	if attrs == nil {
		attrs = []string{}
	}
	return &imap.MailboxInfo{
		Name:       m.name,
		Delimiter:  m.delimiter,
		Attributes: attrs,
	}, nil
}

// Status returns mailbox status information.
func (m *imapMailbox) Status(items []imap.StatusItem) (*imap.MailboxStatus, error) {
	status := imap.NewMailboxStatus(m.name, items)
	status.Flags = []string{"\\Answered", "\\Flagged", "\\Deleted", "\\Seen", "\\Draft"}
	status.PermanentFlags = []string{"\\Answered", "\\Flagged", "\\Deleted", "\\Seen", "\\Draft", "\\*"}

	messages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return nil, err
	}
	status.Messages = uint32(len(messages))

	var unseenCount uint32
	for _, msg := range messages {
		if !msg.IsRead {
			unseenCount++
		}
	}
	status.Unseen = unseenCount
	status.Recent = 0
	maxID, err := m.stores.Mails.MaxIDByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return nil, err
	}
	status.UidNext = uint32(maxID + 1)
	// UIDVALIDITY 持久化随机值（RFC 3501）：数据库重建导致消息 ID 空间
	// 变化时该值随之改变，客户端才会丢弃旧缓存全量重同步。此前硬编码 1，
	// 数据库重建后 Thunderbird 等客户端缓存永不失效（只下载"新增"的
	// UID），表现为列表只剩少量邮件。
	uidValidity, err := m.stores.MailboxState.UidValidity(m.user.id, m.name)
	if err != nil {
		log.Printf("IMAP: 获取 UIDVALIDITY 失败 folder=%s: %v", m.name, err)
		uidValidity = 1
	}
	status.UidValidity = uidValidity

	return status, nil
}

// SetSubscribed sets the subscribed status (no-op for now).
func (m *imapMailbox) SetSubscribed(subscribed bool) error {
	return nil
}

// Check is a no-op checkpoint.
func (m *imapMailbox) Check() error {
	return nil
}

// ListMessages returns messages matching the sequence set and fetch items.
func (m *imapMailbox) ListMessages(uid bool, seqset *imap.SeqSet, items []imap.FetchItem, ch chan<- *imap.Message) error {
	defer close(ch)

	// Fetch all messages in this mailbox
	dbMessages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return err
	}
	if len(dbMessages) == 0 {
		return nil
	}

	// Build a mapping of sequence number (1-based) to db.Message
	type seqEntry struct {
		seqNum uint32
		msg    *db.Message
	}
	entries := make([]seqEntry, len(dbMessages))
	for i := range dbMessages {
		entries[i] = seqEntry{
			seqNum: uint32(i + 1),
			msg:    &dbMessages[i],
		}
	}

	for _, entry := range entries {
		var match bool
		if uid {
			match = seqset.Contains(uint32(entry.msg.ID))
		} else {
			match = seqset.Contains(entry.seqNum)
		}
		if !match {
			continue
		}

		imapMsg, err := m.buildIMAPMessage(entry.msg, entry.seqNum, items)
		if err != nil {
			log.Printf("IMAP: error building message %d: %v", entry.msg.ID, err)
			continue
		}
		ch <- imapMsg
	}

	return nil
}

// buildIMAPMessage constructs an imap.Message from a db.Message with the requested items.
func (m *imapMailbox) buildIMAPMessage(dbMsg *db.Message, seqNum uint32, items []imap.FetchItem) (*imap.Message, error) {
	imapMsg := imap.NewMessage(seqNum, items)
	imapMsg.Uid = uint32(dbMsg.ID)
	imapMsg.Flags = m.getMessageFlags(dbMsg)
	imapMsg.InternalDate = dbMsg.Date

	rawMsg := messageRawData(dbMsg)
	imapMsg.Size = uint32(len(rawMsg))

	for _, item := range items {
		switch item {
		case imap.FetchUid, imap.FetchFlags, imap.FetchInternalDate, imap.FetchRFC822Size:
			continue
		case imap.FetchEnvelope:
			hdr, _, err := headerAndBody(rawMsg)
			if err == nil {
				imapMsg.Envelope, _ = backendutil.FetchEnvelope(hdr)
			}
			if imapMsg.Envelope == nil {
				imapMsg.Envelope = m.buildEnvelope(dbMsg)
			}
		case imap.FetchBody, imap.FetchBodyStructure:
			hdr, body, err := headerAndBody(rawMsg)
			if err == nil {
				imapMsg.BodyStructure, _ = backendutil.FetchBodyStructure(hdr, body, item == imap.FetchBodyStructure)
			}
			// 防御：FetchBodyStructure 对部分合法/畸形 MIME 会失败并返回
			// nil（典型：message/rfc822 附件为 base64 编码时库内不解码
			// 直接按嵌套消息解析头；或 multipart 边界截断）。BodyStructure
			// 为 nil 时 go-imap 格式化 FETCH 响应会在 send() 协程 panic
			// （nil 指针解引用），连接中断导致客户端只收到部分邮件甚至
			// 一直卡在同步。解析失败时降级为 text/plain 单段结构。
			if imapMsg.BodyStructure == nil {
				imapMsg.BodyStructure = fallbackBodyStructure(rawMsg)
			}
		default:
			section, err := imap.ParseBodySectionName(item)
			if err != nil {
				continue
			}
			hdr, body, err := headerAndBody(rawMsg)
			if err != nil {
				return nil, err
			}
			literal, _ := backendutil.FetchBodySection(hdr, body, section)
			if literal != nil {
				imapMsg.Body[section] = literal
			}
		}
	}

	return imapMsg, nil
}

// fallbackBodyStructure 构造一个 text/plain 单段 BodyStructure，用于
// MIME 解析失败的消息（保证 FETCH BODY/BODYSTRUCTURE 不因 nil 崩溃）。
func fallbackBodyStructure(raw []byte) *imap.BodyStructure {
	size := uint32(len(raw))
	lines := uint32(bytes.Count(raw, []byte{'\n'}))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		lines++
	}
	return &imap.BodyStructure{
		MIMEType:    "text",
		MIMESubType: "plain",
		Params:      map[string]string{"charset": "utf-8"},
		Encoding:    "8bit",
		Size:        size,
		Lines:       lines,
	}
}

func messageRawData(msg *db.Message) []byte {
	if msg.RawData != "" {
		return []byte(msg.RawData)
	}
	return buildRawMessage(msg)
}

func headerAndBody(raw []byte) (textproto.Header, io.Reader, error) {
	body := bufio.NewReader(bytes.NewReader(raw))
	hdr, err := textproto.ReadHeader(body)
	return hdr, body, err
}

// getMessageFlags returns IMAP flags for a database message.
func (m *imapMailbox) getMessageFlags(dbMsg *db.Message) []string {
	flags := make([]string, 0)
	if dbMsg.IsRead {
		flags = append(flags, "\\Seen")
	}
	if dbMsg.IsFlagged {
		flags = append(flags, "\\Flagged")
	}
	if m.deleted != nil && m.deleted[dbMsg.ID] {
		flags = append(flags, "\\Deleted")
	}
	return flags
}

// buildEnvelope constructs an imap.Envelope from a db.Message.
func (m *imapMailbox) buildEnvelope(dbMsg *db.Message) *imap.Envelope {
	env := &imap.Envelope{
		Date:      dbMsg.Date,
		Subject:   dbMsg.Subject,
		From:      parseAddressList(dbMsg.FromAddr),
		Sender:    parseAddressList(dbMsg.FromAddr),
		ReplyTo:   parseAddressList(dbMsg.FromAddr),
		To:        parseAddressList(dbMsg.ToAddr),
		Cc:        parseAddressList(dbMsg.CcAddr),
		MessageId: dbMsg.MessageID,
	}
	return env
}

// SearchMessages returns sequence numbers or UIDs of messages matching the criteria.
func (m *imapMailbox) SearchMessages(uid bool, criteria *imap.SearchCriteria) ([]uint32, error) {
	dbMessages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return nil, err
	}

	var results []uint32
	for i, dbMsg := range dbMessages {
		if m.matchesCriteria(&dbMsg, criteria) {
			if uid {
				results = append(results, uint32(dbMsg.ID))
			} else {
				results = append(results, uint32(i+1))
			}
		}
	}
	return results, nil
}

// matchesCriteria checks if a message matches the given search criteria.
func (m *imapMailbox) matchesCriteria(msg *db.Message, criteria *imap.SearchCriteria) bool {
	// Check WithFlags (messages must have all these flags)
	for _, flag := range criteria.WithFlags {
		switch flag {
		case "\\Seen":
			if !msg.IsRead {
				return false
			}
		case "\\Flagged":
			if !msg.IsFlagged {
				return false
			}
		case "\\Deleted":
			if m.deleted == nil || !m.deleted[msg.ID] {
				return false
			}
		}
	}

	// Check WithoutFlags (messages must NOT have any of these flags)
	for _, flag := range criteria.WithoutFlags {
		switch flag {
		case "\\Seen":
			if msg.IsRead {
				return false
			}
		case "\\Flagged":
			if msg.IsFlagged {
				return false
			}
		case "\\Deleted":
			if m.deleted != nil && m.deleted[msg.ID] {
				return false
			}
		}
	}

	// Check date range
	if !criteria.Since.IsZero() && msg.Date.Before(criteria.Since) {
		return false
	}
	if !criteria.Before.IsZero() && !msg.Date.Before(criteria.Before) {
		return false
	}

	// Check header fields
	if criteria.Header != nil {
		if subject := criteria.Header.Get("Subject"); subject != "" {
			if !strings.Contains(strings.ToLower(msg.Subject), strings.ToLower(subject)) {
				return false
			}
		}
		if from := criteria.Header.Get("From"); from != "" {
			if !strings.Contains(strings.ToLower(msg.FromAddr), strings.ToLower(from)) {
				return false
			}
		}
		if to := criteria.Header.Get("To"); to != "" {
			if !strings.Contains(strings.ToLower(msg.ToAddr), strings.ToLower(to)) {
				return false
			}
		}
	}

	// Check body text
	for _, text := range criteria.Body {
		bodyText := strings.ToLower(msg.TextBody + " " + msg.HtmlBody)
		if !strings.Contains(bodyText, strings.ToLower(text)) {
			return false
		}
	}

	// Check generic text (searches headers + body)
	for _, text := range criteria.Text {
		allText := strings.ToLower(msg.Subject + " " + msg.FromAddr + " " + msg.ToAddr + " " + msg.TextBody + " " + msg.HtmlBody)
		if !strings.Contains(allText, strings.ToLower(text)) {
			return false
		}
	}

	// Check NOT criteria
	for _, notCrit := range criteria.Not {
		if m.matchesCriteria(msg, notCrit) {
			return false
		}
	}

	// Check OR criteria (at least one must match)
	for _, orPair := range criteria.Or {
		if !m.matchesCriteria(msg, orPair[0]) && !m.matchesCriteria(msg, orPair[1]) {
			return false
		}
	}

	return true
}

// CreateMessage appends a new message to the mailbox (IMAP APPEND command).
func (m *imapMailbox) CreateMessage(flags []string, date time.Time, body imap.Literal) error {
	// Read the message literal
	data, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("failed to read message body: %w", err)
	}

	// Parse as MIME message
	mr, err := asgomail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to parse MIME message: %w", err)
	}

	header := mr.Header
	fromAddr := mailutil.FormatAddressList(&header, "From")
	toAddr := mailutil.FormatAddressList(&header, "To")
	ccAddr := mailutil.FormatAddressList(&header, "Cc")
	subject, _ := header.Subject()
	messageID, _ := header.MessageID()
	msgDate, _ := header.Date()
	if msgDate.IsZero() {
		msgDate = date
	}
	if msgDate.IsZero() {
		msgDate = time.Now()
	}

	var textBody, htmlBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}

		switch h := p.Header.(type) {
		case *asgomail.InlineHeader:
			contentType, params, _ := h.ContentType()
			buf, _ := io.ReadAll(p.Body)
			// 检测并转换字符集
			charset := ""
			if cs, ok := params["charset"]; ok {
				charset = cs
			}
			decoded := mailutil.DecodeCharset(buf, charset)
			if strings.HasPrefix(contentType, "text/plain") {
				textBody = decoded
			} else if strings.HasPrefix(contentType, "text/html") {
				htmlBody = decoded
			}
		case *asgomail.AttachmentHeader:
			// Attachments from APPEND are not saved in this simple implementation
		}
	}

	if textBody == "" && htmlBody == "" {
		textBody = string(data)
	}

	// Determine initial flag state
	isRead := false
	isFlagged := false
	for _, flag := range flags {
		switch flag {
		case "\\Seen":
			isRead = true
		case "\\Flagged":
			isFlagged = true
		}
	}

	msg := &db.Message{
		UserID:    m.user.id,
		MessageID: messageID,
		Folder:    m.name,
		FromAddr:  fromAddr,
		ToAddr:    toAddr,
		CcAddr:    ccAddr,
		Subject:   subject,
		TextBody:  textBody,
		HtmlBody:  htmlBody,
		RawData:   string(data),
		IsRead:    isRead,
		IsFlagged: isFlagged,
		Date:      msgDate,
	}

	if err := m.stores.Mails.Create(msg); err != nil {
		return fmt.Errorf("failed to create message: %w", err)
	}

	// 新邮件（IMAP APPEND）→ 推送给同用户其他已选中该邮箱的客户端
	pushUpdate(m.user.updates, buildNewMessageUpdate(m.stores, m.user.email, m.name, msg))

	return nil
}

// UpdateMessagesFlags modifies flags on messages.
func (m *imapMailbox) UpdateMessagesFlags(uid bool, seqset *imap.SeqSet, op imap.FlagsOp, flags []string) error {
	if m.deleted == nil {
		m.deleted = make(map[uint]bool)
	}

	dbMessages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return err
	}

	// 记录首个持久化错误：SQLite 忙/锁等瞬时失败必须让客户端感知
	// （返回 NO 触发重试），否则已读/星标会静默丢失。
	var firstErr error

	for i, dbMsg := range dbMessages {
		var match bool
		if uid {
			match = seqset.Contains(uint32(dbMsg.ID))
		} else {
			match = seqset.Contains(uint32(i + 1))
		}
		if !match {
			continue
		}

		flagSet := make(map[string]bool, len(flags))
		for _, flag := range flags {
			flagSet[flag] = true
		}

		applyFlag := func(flag string, enabled bool) {
			switch flag {
			case "\\Seen":
				if err := m.stores.Mails.MarkReadState(dbMsg.ID, enabled); err != nil && firstErr == nil {
					log.Printf("IMAP: mark read state for msg %d failed: %v", dbMsg.ID, err)
					firstErr = err
				}
			case "\\Flagged":
				if err := m.stores.Mails.MarkFlagged(dbMsg.ID, enabled); err != nil && firstErr == nil {
					log.Printf("IMAP: mark flagged for msg %d failed: %v", dbMsg.ID, err)
					firstErr = err
				}
			case "\\Deleted":
				if enabled {
					m.deleted[dbMsg.ID] = true
				} else {
					delete(m.deleted, dbMsg.ID)
				}
			}
		}

		switch op {
		case imap.SetFlags:
			applyFlag("\\Seen", flagSet["\\Seen"])
			applyFlag("\\Flagged", flagSet["\\Flagged"])
			applyFlag("\\Deleted", flagSet["\\Deleted"])
		case imap.AddFlags:
			for flag := range flagSet {
				applyFlag(flag, true)
			}
		case imap.RemoveFlags:
			for flag := range flagSet {
				applyFlag(flag, false)
			}
		}

		// 标志变化（已读/星标/删除）→ 推送给同用户其他客户端
		// （重新读库取最新状态，\Deleted 取会话内状态）
		fresh, err := m.stores.Mails.GetByID(dbMsg.ID)
		if err != nil {
			continue
		}
		deleted := m.deleted != nil && m.deleted[dbMsg.ID]
		pushUpdate(m.user.updates, buildFlagsUpdate(m.stores, m.user.email, m.name, fresh, deleted))
	}

	return firstErr
}

// CopyMessages copies messages to another mailbox.
func (m *imapMailbox) CopyMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	dest, ok := canonicalMailboxName(dest)
	if !ok {
		return backend.ErrNoSuchMailbox
	}

	dbMessages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return err
	}

	for i, dbMsg := range dbMessages {
		var match bool
		if uid {
			match = seqset.Contains(uint32(dbMsg.ID))
		} else {
			match = seqset.Contains(uint32(i + 1))
		}
		if !match {
			continue
		}

		// Create a copy in the destination mailbox
		copyMsg := &db.Message{
			UserID:    m.user.id,
			MessageID: dbMsg.MessageID,
			Folder:    dest,
			FromAddr:  dbMsg.FromAddr,
			ToAddr:    dbMsg.ToAddr,
			CcAddr:    dbMsg.CcAddr,
			Subject:   dbMsg.Subject,
			TextBody:  dbMsg.TextBody,
			HtmlBody:  dbMsg.HtmlBody,
			RawData:   dbMsg.RawData,
			IsRead:    dbMsg.IsRead,
			IsFlagged: dbMsg.IsFlagged,
			Date:      dbMsg.Date,
		}
		if err := m.stores.Mails.Create(copyMsg); err != nil {
			log.Printf("IMAP: failed to copy message %d to %s: %v", dbMsg.ID, dest, err)
			continue
		}
		// 目标邮箱新增 → 推送给同用户其他客户端
		pushUpdate(m.user.updates, buildNewMessageUpdate(m.stores, m.user.email, dest, copyMsg))
	}

	return nil
}

// MoveMessages moves messages to another mailbox.
func (m *imapMailbox) MoveMessages(uid bool, seqset *imap.SeqSet, dest string) error {
	dest, ok := canonicalMailboxName(dest)
	if !ok {
		return backend.ErrNoSuchMailbox
	}

	dbMessages, err := m.stores.Mails.ListAllByUserAndFolder(m.user.id, m.name)
	if err != nil {
		return err
	}

	for i, dbMsg := range dbMessages {
		var match bool
		if uid {
			match = seqset.Contains(uint32(dbMsg.ID))
		} else {
			match = seqset.Contains(uint32(i + 1))
		}
		if !match {
			continue
		}
		if err := m.stores.Mails.MoveToFolder(dbMsg.ID, dest); err != nil {
			log.Printf("IMAP: failed to move message %d to %s: %v", dbMsg.ID, dest, err)
			continue
		}
		// 目标邮箱新增（移动后消息在 dest）→ 推送给同用户其他客户端
		if moved, err := m.stores.Mails.GetByID(dbMsg.ID); err == nil {
			pushUpdate(m.user.updates, buildNewMessageUpdate(m.stores, m.user.email, dest, moved))
		}
	}
	return nil
}

// Expunge permanently removes messages marked as \Deleted.
func (m *imapMailbox) Expunge() error {
	if m.deleted == nil {
		return nil
	}

	// 删除前计算各消息的序号（Expunge 响应序号为删除前状态下的序号）
	var seqs []uint32
	for msgID := range m.deleted {
		if seq := seqOf(m.stores, m.user.id, m.name, msgID); seq > 0 {
			seqs = append(seqs, seq)
		}
	}

	for msgID := range m.deleted {
		if err := m.stores.Mails.Delete(msgID); err != nil {
			log.Printf("IMAP: failed to expunge message %d: %v", msgID, err)
		}
	}
	m.deleted = make(map[uint]bool)

	// 删除 → 推送给同用户其他客户端（每条序号一个 ExpungeUpdate）
	for _, seq := range seqs {
		pushUpdate(m.user.updates, &backend.ExpungeUpdate{
			Update: backend.NewUpdate(m.user.email, m.name),
			SeqNum: seq,
		})
	}
	return nil
}

// ---------- Helper functions ----------

// parseAddressList parses a comma-separated address string into imap.Address slice.
func parseAddressList(addrStr string) []*imap.Address {
	if addrStr == "" {
		return nil
	}

	addresses, err := mail.ParseAddressList(addrStr)
	if err != nil {
		// Fallback: treat the whole string as a single address
		return []*imap.Address{{
			MailboxName: addrStr,
			HostName:    "",
		}}
	}

	result := make([]*imap.Address, 0, len(addresses))
	for _, addr := range addresses {
		parts := strings.SplitN(addr.Address, "@", 2)
		mailbox := parts[0]
		host := ""
		if len(parts) > 1 {
			host = parts[1]
		}
		result = append(result, &imap.Address{
			PersonalName: addr.Name,
			MailboxName:  mailbox,
			HostName:     host,
		})
	}
	return result
}

// buildRawMessage reconstructs a raw RFC822 message from a db.Message.
// If RawData is available, it uses the original raw data directly.
func buildRawMessage(msg *db.Message) []byte {
	// 优先使用原始邮件数据
	if msg.RawData != "" {
		return []byte(msg.RawData)
	}

	// 降级：从字段重建
	var buf bytes.Buffer

	// Write headers
	buf.WriteString(fmt.Sprintf("From: %s\r\n", msg.FromAddr))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", msg.ToAddr))
	if msg.CcAddr != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", msg.CcAddr))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	if msg.MessageID != "" {
		buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	// Write body
	if msg.HtmlBody != "" && msg.TextBody != "" {
		boundary := fmt.Sprintf("mailgo_%d", msg.ID)
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	} else if msg.TextBody != "" {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
	} else if msg.HtmlBody != "" {
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
	} else {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	}

	return buf.Bytes()
}

// buildRawHeader reconstructs just the header portion of a raw RFC822 message.
func buildRawHeader(msg *db.Message) []byte {
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("From: %s\r\n", msg.FromAddr))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", msg.ToAddr))
	if msg.CcAddr != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", msg.CcAddr))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	if msg.MessageID != "" {
		buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")
	if msg.HtmlBody != "" && msg.TextBody != "" {
		boundary := fmt.Sprintf("mailgo_%d", msg.ID)
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	} else if msg.TextBody != "" {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	} else if msg.HtmlBody != "" {
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n")
	}
	return buf.Bytes()
}
