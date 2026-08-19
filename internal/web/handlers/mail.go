package handlers

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mail_go/internal/db"
	"mail_go/internal/outbound"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// pendingAttachment holds an uploaded attachment while the message is built.
type pendingAttachment struct {
	filename    string
	contentType string
	data        []byte
}

// base64LineWrap encodes data as base64 wrapped at 76 columns (RFC 2045).
func base64LineWrap(data []byte) string {
	enc := base64.StdEncoding.EncodeToString(data)
	if len(enc) <= 76 {
		return enc
	}
	var sb strings.Builder
	for len(enc) > 76 {
		sb.WriteString(enc[:76])
		sb.WriteString("\r\n")
		enc = enc[76:]
	}
	sb.WriteString(enc)
	return sb.String()
}

// MailHandler handles mail-related routes (inbox, compose, sent, view, etc.).
type MailHandler struct {
	stores   *store.Stores
	storage  *storage.AttachmentStorage
	outbound *outbound.Manager
}

// NewMailHandler creates a new MailHandler with the given stores, attachment
// storage and outbound delivery manager.
func NewMailHandler(stores *store.Stores, attStorage *storage.AttachmentStorage, ob *outbound.Manager) *MailHandler {
	return &MailHandler{stores: stores, storage: attStorage, outbound: ob}
}

// folderCounts returns sidebar badge counts for the current user.
func (h *MailHandler) folderCounts(userID uint) (inboxUnread, draftsTotal, sentTotal int64) {
	inboxUnread, _ = h.stores.Mails.CountUnread(userID, "INBOX")
	draftsTotal, _ = h.stores.Mails.CountByUserAndFolder(userID, "Drafts")
	sentTotal, _ = h.stores.Mails.CountByUserAndFolder(userID, "Sent")
	return
}

// Inbox renders the inbox page showing all messages in the user's INBOX folder.
func (h *MailHandler) Inbox(c *gin.Context) {
	userID := c.GetUint("userID")
	page := getPageParam(c, "page", 1)

	messages, total, err := h.stores.Mails.ListByUserAndFolder(userID, "INBOX", page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载收件箱失败: %v", err)
		return
	}

	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "inbox", gin.H{
		"currentUser":  currentUser,
		"messages":     messages,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       "INBOX",
		"activeFolder": "inbox",
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// View renders the email detail page for a specific message.
func (h *MailHandler) View(c *gin.Context) {
	userID := c.GetUint("userID")
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的邮件ID")
		return
	}

	msg, err := h.stores.Mails.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "邮件不存在")
		return
	}

	// Verify the message belongs to the current user
	if msg.UserID != userID {
		c.String(http.StatusForbidden, "禁止访问")
		return
	}

	// Load attachments
	attachments, _ := h.stores.Attachments.ListByMessage(uint(id))

	// Auto mark as read
	if !msg.IsRead {
		_ = h.stores.Mails.MarkRead(uint(id))
		msg.IsRead = true
	}

	currentUser, _ := c.Get("currentUser")
	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)

	c.HTML(200, "view", gin.H{
		"currentUser":  currentUser,
		"message":      msg,
		"attachments":  attachments,
		"activeFolder": resolveActiveFolder(msg.Folder),
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// Compose renders the email composition page.
func (h *MailHandler) Compose(c *gin.Context) {
	userID := c.GetUint("userID")
	currentUser, _ := c.Get("currentUser")

	// Get user quota info for display
	user, _ := h.stores.Users.GetByID(userID)
	var usedBytes int64
	var quotaBytes int64
	if user != nil {
		usedBytes = user.UsedBytes
		quotaBytes = user.QuotaBytes
	}

	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)

	c.HTML(200, "compose", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "compose",
		"error":        "",
		"to":           c.Query("to"),
		"subject":      c.Query("subject"),
		"bodyContent":  "",
		"usedBytes":    usedBytes,
		"quotaBytes":   quotaBytes,
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// DoSend processes the email composition form, sends the email via SMTP,
// and stores the message record.
func (h *MailHandler) DoSend(c *gin.Context) {
	userID := c.GetUint("userID")
	currentUserVal, _ := c.Get("currentUser")
	currentUser := currentUserVal.(*db.User)

	to := c.PostForm("to")
	subject := c.PostForm("subject")
	body := c.PostForm("body")
	htmlBody := c.PostForm("html_body")
	cc := c.PostForm("cc")

	if to == "" {
		c.HTML(http.StatusBadRequest, "compose", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "compose",
			"error":        "请输入收件人",
			"to":           to,
			"subject":      subject,
			"cc":           cc,
			"bodyContent":  htmlBody,
			"usedBytes":    currentUser.UsedBytes,
			"quotaBytes":   currentUser.QuotaBytes,
		})
		return
	}

	// Handle attachments and check quota
	form, multipartErr := c.MultipartForm()
	attachments := make([]pendingAttachment, 0)
	if multipartErr == nil {
		files := form.File["attachments"]
		if len(files) > 0 {
			// 原子预扣附件配额（单条 SQL：used_bytes + n <= quota_bytes 才生效），
			// 防止并发提交绕过配额检查（TOCTOU）。后续保存失败会补偿回退。
			var totalNewSize int64
			for _, file := range files {
				totalNewSize += file.Size
			}
			reserved, err := h.stores.Users.TryReserveQuota(userID, totalNewSize)
			if err != nil {
				c.HTML(http.StatusInternalServerError, "compose", gin.H{
					"currentUser":  currentUser,
					"activeFolder": "compose",
					"error":        "配额检查失败，请稍后重试",
					"to":           to,
					"subject":      subject,
					"cc":           cc,
					"bodyContent":  htmlBody,
					"usedBytes":    currentUser.UsedBytes,
					"quotaBytes":   currentUser.QuotaBytes,
				})
				return
			}
			if !reserved {
				user, _ := h.stores.Users.GetByID(userID)
				usedBytes, quotaBytes := currentUser.UsedBytes, currentUser.QuotaBytes
				if user != nil {
					usedBytes, quotaBytes = user.UsedBytes, user.QuotaBytes
				}
				c.HTML(http.StatusBadRequest, "compose", gin.H{
					"currentUser":  currentUser,
					"activeFolder": "compose",
					"error":        fmt.Sprintf("附件超出配额限制。已用 %s / 总配额 %s", formatBytes(usedBytes), formatBytes(quotaBytes)),
					"to":           to,
					"subject":      subject,
					"cc":           cc,
					"bodyContent":  htmlBody,
					"usedBytes":    usedBytes,
					"quotaBytes":   quotaBytes,
				})
				return
			}

			// Read all attachment files into memory once (used for both the
			// MIME message body and the stored attachment records).
			// 读取失败的文件回退已预扣的配额。
			for _, file := range files {
				f, err := file.Open()
				if err != nil {
					_ = h.stores.Users.UpdateUsedBytes(userID, -file.Size)
					continue
				}
				buf, readErr := io.ReadAll(f)
				f.Close()
				if readErr != nil {
					_ = h.stores.Users.UpdateUsedBytes(userID, -file.Size)
					continue
				}

				// Determine content type from extension
				contentType := "application/octet-stream"
				ext := strings.ToLower(filepath.Ext(file.Filename))
				if ct, ok := mimeTypes[ext]; ok {
					contentType = ct
				}

				attachments = append(attachments, pendingAttachment{
					filename:    file.Filename,
					contentType: contentType,
					data:        buf,
				})
			}
		}
	}

	// Build the email content
	fromAddr := fmt.Sprintf("%s@%s", currentUser.Username, currentUser.Domain.Name)
	messageID, rawMessage := buildOutgoingMessage(fromAddr, to, cc, subject, body, htmlBody, attachments)
	now := time.Now()

	allRecipients := append(parseAddressInput(to), parseAddressInput(cc)...)
	localUsers := make([]*db.User, 0, len(allRecipients))
	var externalRecipients []string
	for _, rcpt := range allRecipients {
		user, err := h.stores.Users.GetByEmail(rcpt)
		if err != nil {
			externalRecipients = append(externalRecipients, rcpt)
			continue
		}
		localUsers = append(localUsers, user)
	}

	// Queue external recipients for outbound delivery first, so that
	// failures (rate limit, invalid address, disabled outbound) abort
	// before any local copies are created.
	if len(externalRecipients) > 0 {
		ob := h.outbound
		if ob == nil || !ob.Enabled() {
			c.HTML(http.StatusBadRequest, "compose", gin.H{
				"currentUser":  currentUser,
				"activeFolder": "compose",
				"error":        "外部投递未启用",
				"to":           to,
				"subject":      subject,
				"cc":           cc,
				"bodyContent":  htmlBody,
				"usedBytes":    currentUser.UsedBytes,
				"quotaBytes":   currentUser.QuotaBytes,
			})
			return
		}
		if maxRcpt := ob.MaxRecipients(); maxRcpt > 0 && len(externalRecipients) > maxRcpt {
			c.HTML(http.StatusBadRequest, "compose", gin.H{
				"currentUser":  currentUser,
				"activeFolder": "compose",
				"error":        fmt.Sprintf("外部收件人过多：最多 %d 个", maxRcpt),
				"to":           to,
				"subject":      subject,
				"cc":           cc,
				"bodyContent":  htmlBody,
				"usedBytes":    currentUser.UsedBytes,
				"quotaBytes":   currentUser.QuotaBytes,
			})
			return
		}
		for _, rcpt := range externalRecipients {
			if _, err := ob.Enqueue(currentUser, fromAddr, rcpt, []byte(rawMessage)); err != nil {
				c.HTML(http.StatusBadRequest, "compose", gin.H{
					"currentUser":  currentUser,
					"activeFolder": "compose",
					"error":        fmt.Sprintf("外发邮件入队失败 (%s): %v", rcpt, err),
					"to":           to,
					"subject":      subject,
					"cc":           cc,
					"bodyContent":  htmlBody,
					"usedBytes":    currentUser.UsedBytes,
					"quotaBytes":   currentUser.QuotaBytes,
				})
				return
			}
		}
	}

	for _, rcptUser := range localUsers {
		inboxMsg := &db.Message{
			UserID:    rcptUser.ID,
			MessageID: messageID,
			Folder:    "INBOX",
			FromAddr:  fromAddr,
			ToAddr:    to,
			CcAddr:    cc,
			Subject:   subject,
			TextBody:  body,
			HtmlBody:  htmlBody,
			RawData:   rawMessage,
			Date:      now,
			IsRead:    false,
		}
		if createErr := h.stores.Mails.Create(inboxMsg); createErr != nil {
			c.HTML(http.StatusInternalServerError, "compose", gin.H{
				"currentUser":  currentUser,
				"activeFolder": "compose",
				"error":        fmt.Sprintf("投递邮件失败: %v", createErr),
				"to":           to,
				"subject":      subject,
				"cc":           cc,
				"bodyContent":  htmlBody,
				"usedBytes":    currentUser.UsedBytes,
				"quotaBytes":   currentUser.QuotaBytes,
			})
			return
		}
	}

	// Save to Sent folder
	msg := &db.Message{
		UserID:    userID,
		MessageID: messageID,
		Folder:    "Sent",
		FromAddr:  fromAddr,
		ToAddr:    to,
		CcAddr:    cc,
		Subject:   subject,
		TextBody:  body,
		HtmlBody:  htmlBody,
		RawData:   rawMessage,
		Date:      now,
		IsRead:    true,
	}

	if createErr := h.stores.Mails.Create(msg); createErr != nil {
		c.HTML(http.StatusInternalServerError, "compose", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "compose",
			"error":        fmt.Sprintf("保存邮件失败: %v", createErr),
			"to":           to,
			"subject":      subject,
			"cc":           cc,
			"bodyContent":  htmlBody,
			"usedBytes":    currentUser.UsedBytes,
			"quotaBytes":   currentUser.QuotaBytes,
		})
		return
	}

	// Save attachment records linked to the Sent copy (bytes were already
	// read during message construction). 配额已在前面原子预扣，
	// 保存/落库失败的附件需要补偿回退。
	for _, att := range attachments {
		relPath, err := h.storage.Save(att.filename, att.data)
		if err != nil {
			_ = h.stores.Users.UpdateUsedBytes(userID, -int64(len(att.data)))
			continue
		}

		attRecord := &db.Attachment{
			MessageID:   msg.ID,
			FileName:    att.filename,
			FilePath:    relPath,
			ContentType: att.contentType,
			FileSize:    int64(len(att.data)),
		}
		if err := h.stores.Attachments.Create(attRecord); err != nil {
			_ = h.stores.Users.UpdateUsedBytes(userID, -attRecord.FileSize)
			continue
		}
	}

	c.Redirect(http.StatusFound, "/sent")
}

func parseAddressInput(input string) []string {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	})
	addresses := make([]string, 0, len(parts))
	for _, part := range parts {
		addr := strings.TrimSpace(part)
		if addr != "" {
			addresses = append(addresses, addr)
		}
	}
	return addresses
}

// sanitizeHeaderField removes CR/LF/NUL from a value destined for an RFC 5322
// message header, preventing header injection (e.g. smuggling a Bcc or
// Reply-To header via a crafted subject or address list).
func sanitizeHeaderField(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, "\x00", "")
	return s
}

// encodeSubject prepares a subject for safe inclusion as a message header:
// header injection characters are stripped and non-ASCII content is encoded
// per RFC 2047.
func encodeSubject(s string) string {
	return mime.QEncoding.Encode("utf-8", sanitizeHeaderField(s))
}

// formatContentDisposition builds a Content-Disposition header value for the
// given filename, quoting/encoding it per RFC 2183/2231 (also neutralizes
// CR/LF injection through crafted filenames).
func formatContentDisposition(filename string) string {
	return mime.FormatMediaType("attachment", map[string]string{"filename": filename})
}

// buildOutgoingMessage constructs the raw RFC 5322 message for the web
// compose form and returns its Message-ID. All header values derived from
// user input are sanitized to prevent CRLF header injection.
func buildOutgoingMessage(from, to, cc, subject, body, htmlBody string, attachments []pendingAttachment) (messageID, raw string) {
	now := time.Now()
	messageID = fmt.Sprintf("<%s@mail_go>", uuid.New().String())

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("From: %s\r\n", sanitizeHeaderField(from)))
	sb.WriteString(fmt.Sprintf("To: %s\r\n", sanitizeHeaderField(to)))
	if cc != "" {
		sb.WriteString(fmt.Sprintf("Cc: %s\r\n", sanitizeHeaderField(cc)))
	}
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeSubject(subject)))
	sb.WriteString(fmt.Sprintf("Message-ID: %s\r\n", messageID))
	sb.WriteString(fmt.Sprintf("Date: %s\r\n", now.Format(time.RFC1123Z)))
	sb.WriteString("MIME-Version: 1.0\r\n")

	// Attachments are wrapped in an outer multipart/mixed container.
	outerBoundary := ""
	hasAttachments := len(attachments) > 0
	if hasAttachments {
		outerBoundary = fmt.Sprintf("----=_Mixed_%s", uuid.New().String())
		sb.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=\"%s\"\r\n", outerBoundary))
		sb.WriteString("\r\n")
		sb.WriteString(fmt.Sprintf("--%s\r\n", outerBoundary))
	}

	// Build message body with multipart/alternative if HTML is present
	if htmlBody != "" {
		boundary := fmt.Sprintf("----=_Part_%s", uuid.New().String())
		sb.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
		sb.WriteString("\r\n")
		sb.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		sb.WriteString(body)
		sb.WriteString(fmt.Sprintf("\r\n--%s\r\n", boundary))
		sb.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		sb.WriteString(htmlBody)
		sb.WriteString(fmt.Sprintf("\r\n--%s--\r\n", boundary))
	} else {
		sb.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
		sb.WriteString("\r\n")
		sb.WriteString(body)
		sb.WriteString("\r\n")
	}

	// Append attachment parts to the multipart/mixed container.
	for _, att := range attachments {
		contentType := mime.FormatMediaType(att.contentType, map[string]string{"name": att.filename})
		sb.WriteString(fmt.Sprintf("--%s\r\n", outerBoundary))
		sb.WriteString(fmt.Sprintf("Content-Type: %s\r\n", contentType))
		sb.WriteString("Content-Transfer-Encoding: base64\r\n")
		sb.WriteString(fmt.Sprintf("Content-Disposition: %s\r\n\r\n", formatContentDisposition(att.filename)))
		sb.WriteString(base64LineWrap(att.data))
		sb.WriteString("\r\n")
	}
	if hasAttachments {
		sb.WriteString(fmt.Sprintf("--%s--\r\n", outerBoundary))
	}

	return messageID, sb.String()
}

// mimeTypes maps common file extensions to MIME types.
var mimeTypes = map[string]string{
	".txt":  "text/plain",
	".html": "text/html",
	".htm":  "text/html",
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".zip":  "application/zip",
	".rar":  "application/x-rar-compressed",
	".csv":  "text/csv",
}

// Sent renders the sent mail folder page.
func (h *MailHandler) Sent(c *gin.Context) {
	userID := c.GetUint("userID")
	page := getPageParam(c, "page", 1)

	messages, total, err := h.stores.Mails.ListByUserAndFolder(userID, "Sent", page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载发件箱失败: %v", err)
		return
	}

	currentUser, _ := c.Get("currentUser")
	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "sent", gin.H{
		"currentUser":  currentUser,
		"messages":     messages,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       "Sent",
		"activeFolder": "sent",
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// safeRedirectPath 仅接受同站相对路径（以 / 开头且非 //），
// 防止把用户重定向到外部站点（开放重定向）。非法值返回空串，
// 调用方应回退到默认路径。
func safeRedirectPath(referer string) string {
	if referer == "" || !strings.HasPrefix(referer, "/") || strings.HasPrefix(referer, "//") {
		return ""
	}
	return referer
}

// Delete removes a message by ID after verifying ownership.
func (h *MailHandler) Delete(c *gin.Context) {
	userID := c.GetUint("userID")
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的邮件ID")
		return
	}

	msg, err := h.stores.Mails.GetByID(uint(id))
	if err != nil || msg.UserID != userID {
		c.String(http.StatusForbidden, "禁止访问")
		return
	}

	// Delete attachments on disk and in DB, and decrease UsedBytes
	attachments, _ := h.stores.Attachments.ListByMessage(uint(id))
	for _, att := range attachments {
		_ = h.storage.Delete(att.FilePath)
		_ = h.stores.Users.UpdateUsedBytes(userID, -att.FileSize)
	}
	_ = h.stores.Attachments.DeleteByMessage(uint(id))
	_ = h.stores.Mails.Delete(uint(id))

	// Redirect back based on the folder（仅同站相对路径，防开放重定向）
	referer := safeRedirectPath(c.GetHeader("Referer"))
	if referer == "" {
		referer = "/inbox"
	}
	c.Redirect(http.StatusFound, referer)
}

// MarkRead marks a message as read.
func (h *MailHandler) MarkRead(c *gin.Context) {
	userID := c.GetUint("userID")
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的邮件ID")
		return
	}

	msg, err := h.stores.Mails.GetByID(uint(id))
	if err != nil || msg.UserID != userID {
		c.String(http.StatusForbidden, "禁止访问")
		return
	}

	_ = h.stores.Mails.MarkRead(uint(id))

	// Redirect back based on the folder（仅同站相对路径，防开放重定向）
	referer := safeRedirectPath(c.GetHeader("Referer"))
	if referer == "" {
		referer = "/inbox"
	}
	c.Redirect(http.StatusFound, referer)
}

// DownloadAttachment serves an attachment file for download.
func (h *MailHandler) DownloadAttachment(c *gin.Context) {
	userID := c.GetUint("userID")
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的附件ID")
		return
	}

	att, err := h.stores.Attachments.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "附件不存在")
		return
	}

	// Verify the message belongs to the current user
	msg, err := h.stores.Mails.GetByID(att.MessageID)
	if err != nil || msg.UserID != userID {
		c.String(http.StatusForbidden, "禁止访问")
		return
	}

	data, err := h.storage.Read(att.FilePath)
	if err != nil {
		c.String(http.StatusInternalServerError, "读取附件失败")
		return
	}

	c.Header("Content-Disposition", formatContentDisposition(att.FileName))
	c.Data(http.StatusOK, att.ContentType, data)
}

// getPageParam extracts and validates a page query parameter.
// Returns defaultVal if the parameter is missing or invalid.
func getPageParam(c *gin.Context, key string, defaultVal int) int {
	pageStr := c.Query(key)
	if pageStr == "" {
		return defaultVal
	}
	page, err := strconv.Atoi(pageStr)
	if err != nil || page < 1 {
		return defaultVal
	}
	return page
}

// Drafts renders the drafts folder page.
func (h *MailHandler) Drafts(c *gin.Context) {
	userID := c.GetUint("userID")
	page := getPageParam(c, "page", 1)

	messages, total, err := h.stores.Mails.ListByUserAndFolder(userID, "Drafts", page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载草稿箱失败: %v", err)
		return
	}

	currentUser, _ := c.Get("currentUser")
	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "drafts", gin.H{
		"currentUser":  currentUser,
		"messages":     messages,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       "Drafts",
		"activeFolder": "drafts",
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// Settings renders the user settings page.
func (h *MailHandler) Settings(c *gin.Context) {
	currentUser, _ := c.Get("currentUser")
	userID := c.GetUint("userID")
	inboxUnread, draftsTotal, sentTotal := h.folderCounts(userID)
	c.HTML(200, "settings", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "settings",
		"error":        "",
		"success":      "",
		"mustChange":   c.Query("force") == "1",
		"inboxUnread":  inboxUnread,
		"draftsTotal":  draftsTotal,
		"sentTotal":    sentTotal,
	})
}

// UpdateSettings handles the password change form.
func (h *MailHandler) UpdateSettings(c *gin.Context) {
	userID := c.GetUint("userID")
	currentUserVal, _ := c.Get("currentUser")
	currentUser := currentUserVal.(*db.User)

	oldPassword := c.PostForm("old_password")
	newPassword := c.PostForm("new_password")
	confirmPassword := c.PostForm("confirm_password")

	// Verify old password
	if err := bcrypt.CompareHashAndPassword([]byte(currentUser.PasswordHash), []byte(oldPassword)); err != nil {
		c.HTML(http.StatusBadRequest, "settings", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "settings",
			"error":        "当前密码不正确",
			"success":      "",
		})
		return
	}

	if newPassword == "" {
		c.HTML(http.StatusBadRequest, "settings", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "settings",
			"error":        "新密码不能为空",
			"success":      "",
		})
		return
	}

	if newPassword != confirmPassword {
		c.HTML(http.StatusBadRequest, "settings", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "settings",
			"error":        "两次输入的密码不一致",
			"success":      "",
		})
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "settings", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "settings",
			"error":        "密码加密失败",
			"success":      "",
		})
		return
	}

	if err := h.stores.Users.UpdatePassword(userID, string(hashedPassword)); err != nil {
		c.HTML(http.StatusInternalServerError, "settings", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "settings",
			"error":        "密码更新失败",
			"success":      "",
		})
		return
	}

	c.HTML(http.StatusOK, "settings", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "settings",
		"error":        "",
		"success":      "密码修改成功",
	})
}

// resolveActiveFolder maps a folder name to a sidebar active state key.
func resolveActiveFolder(folder string) string {
	switch folder {
	case "INBOX":
		return "inbox"
	case "Sent":
		return "sent"
	case "Drafts":
		return "drafts"
	default:
		return folder
	}
}

// formatBytes converts a file size in bytes to a human-readable string.
// This is a handler-level utility that reuses the web package function.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
