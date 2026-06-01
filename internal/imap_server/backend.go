package imap_server

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/mail"
	"strings"
	"time"

	"mail_go/internal/db"
	"mail_go/internal/mailutil"
	"mail_go/internal/store"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	asgomail "github.com/emersion/go-message/mail"
)

// ---------- imapBackend ----------

// imapBackend implements backend.Backend.
type imapBackend struct {
	stores *store.Stores
}

// Login authenticates a user by email and password.
func (b *imapBackend) Login(connInfo *imap.ConnInfo, username, password string) (backend.User, error) {
	user, err := b.stores.Users.Authenticate(username, password)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials: %w", err)
	}

	email := user.Username + "@"
	domain, err := b.stores.Domains.GetByID(user.DomainID)
	if err == nil {
		email = user.Username + "@" + domain.Name
	}

	return &imapUser{
		stores: b.stores,
		id:     user.ID,
		email:  email,
	}, nil
}

// ---------- imapUser ----------

// imapUser implements backend.User.
type imapUser struct {
	stores *store.Stores
	id     uint
	email  string
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
		{"Sent", "/", nil},
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
	validNames := map[string]bool{
		"INBOX":  true,
		"Sent":   true,
		"Drafts": true,
		"Trash":  true,
	}

	normalized := name
	upper := strings.ToUpper(name)
	if upper == "INBOX" {
		normalized = "INBOX"
	} else if validNames[upper] {
		normalized = upper
	} else {
		// Try title case
		normalized = strings.Title(strings.ToLower(name))
	}

	if !validNames[normalized] {
		return nil, backend.ErrNoSuchMailbox
	}

	return &imapMailbox{
		stores:    u.stores,
		user:      u,
		name:      normalized,
		delimiter: "/",
	}, nil
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
	status := &imap.MailboxStatus{
		Name:           m.name,
		Flags:          []string{"\\Answered", "\\Flagged", "\\Deleted", "\\Seen", "\\Draft"},
		PermanentFlags: []string{"\\Answered", "\\Flagged", "\\Deleted", "\\Seen", "\\Draft", "\\*"},
	}

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
	status.UidNext = uint32(len(messages) + 1)
	status.UidValidity = 1

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
	imapMsg := &imap.Message{
		SeqNum:       seqNum,
		Uid:          uint32(dbMsg.ID),
		Flags:        m.getMessageFlags(dbMsg),
		InternalDate: dbMsg.Date,
		Body:         make(map[*imap.BodySectionName]imap.Literal),
	}

	rawMsg := buildRawMessage(dbMsg)
	imapMsg.Size = uint32(len(rawMsg))

	for _, item := range items {
		switch item {
		case imap.FetchUid:
			// UID is already set on the struct
		case imap.FetchFlags:
			// Flags are already set on the struct
		case imap.FetchInternalDate:
			// InternalDate is already set on the struct
		case imap.FetchRFC822Size:
			// Size is already set on the struct
		case imap.FetchEnvelope:
			imapMsg.Envelope = m.buildEnvelope(dbMsg)
		case imap.FetchRFC822:
			section := &imap.BodySectionName{BodyPartName: imap.BodyPartName{}}
			imapMsg.Body[section] = bytes.NewReader(rawMsg)
		case imap.FetchRFC822Header:
			headerBytes := buildRawHeader(dbMsg)
			section := &imap.BodySectionName{
				BodyPartName: imap.BodyPartName{
					Specifier: imap.HeaderSpecifier,
				},
			}
			imapMsg.Body[section] = bytes.NewReader(headerBytes)
		case imap.FetchRFC822Text:
			var bodyText string
			if dbMsg.TextBody != "" {
				bodyText = dbMsg.TextBody
			} else if dbMsg.HtmlBody != "" {
				bodyText = dbMsg.HtmlBody
			}
			section := &imap.BodySectionName{
				BodyPartName: imap.BodyPartName{
					Specifier: imap.TextSpecifier,
				},
			}
			imapMsg.Body[section] = bytes.NewReader([]byte(bodyText))
		default:
			// Handle BODY[] and BODY.PEEK[] sections
			itemStr := string(item)
			if strings.HasPrefix(itemStr, "BODY[]") || strings.HasPrefix(itemStr, "BODY.PEEK[]") {
				section := &imap.BodySectionName{BodyPartName: imap.BodyPartName{}}
				imapMsg.Body[section] = bytes.NewReader(rawMsg)
			} else if strings.HasPrefix(itemStr, "BODY[HEADER") || strings.HasPrefix(itemStr, "BODY.PEEK[HEADER") {
				headerBytes := buildRawHeader(dbMsg)
				section := &imap.BodySectionName{
					BodyPartName: imap.BodyPartName{
						Specifier: imap.HeaderSpecifier,
					},
				}
				imapMsg.Body[section] = bytes.NewReader(headerBytes)
			} else if strings.HasPrefix(itemStr, "BODY[TEXT") || strings.HasPrefix(itemStr, "BODY.PEEK[TEXT") {
				var bodyText string
				if dbMsg.TextBody != "" {
					bodyText = dbMsg.TextBody
				} else if dbMsg.HtmlBody != "" {
					bodyText = dbMsg.HtmlBody
				}
				section := &imap.BodySectionName{
					BodyPartName: imap.BodyPartName{
						Specifier: imap.TextSpecifier,
					},
				}
				imapMsg.Body[section] = bytes.NewReader([]byte(bodyText))
			}
		}
	}

	return imapMsg, nil
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

		for _, flag := range flags {
			switch flag {
			case "\\Seen":
				if op == imap.AddFlags || op == imap.SetFlags {
					_ = m.stores.Mails.MarkRead(dbMsg.ID)
				} else if op == imap.RemoveFlags {
					// Mark as unread — update directly
				}
			case "\\Flagged":
				if op == imap.AddFlags || op == imap.SetFlags {
					_ = m.stores.Mails.MarkFlagged(dbMsg.ID, true)
				} else if op == imap.RemoveFlags {
					_ = m.stores.Mails.MarkFlagged(dbMsg.ID, false)
				}
			case "\\Deleted":
				if op == imap.AddFlags || op == imap.SetFlags {
					m.deleted[dbMsg.ID] = true
				} else if op == imap.RemoveFlags {
					delete(m.deleted, dbMsg.ID)
				}
			}
		}
	}

	return nil
}

// CopyMessages copies messages to another mailbox.
func (m *imapMailbox) CopyMessages(uid bool, seqset *imap.SeqSet, dest string) error {
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
			IsRead:    dbMsg.IsRead,
			IsFlagged: dbMsg.IsFlagged,
			Date:      dbMsg.Date,
		}
		if err := m.stores.Mails.Create(copyMsg); err != nil {
			log.Printf("IMAP: failed to copy message %d to %s: %v", dbMsg.ID, dest, err)
		}
	}

	return nil
}

// Expunge permanently removes messages marked as \Deleted.
func (m *imapMailbox) Expunge() error {
	if m.deleted == nil {
		return nil
	}

	for msgID := range m.deleted {
		if err := m.stores.Mails.Delete(msgID); err != nil {
			log.Printf("IMAP: failed to expunge message %d: %v", msgID, err)
		}
	}
	m.deleted = make(map[uint]bool)
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
