package handlers

import (
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mail_go/internal/db"
	"mail_go/internal/imap_server"
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
	// svc 邮箱服务层（IMAP 层共用）：文件夹目录与消息操作同源。
	svc *imap_server.MailboxService
	// pusher 邮件状态变化推送（IMAP 客户端实时同步），可空
	pusher imap_server.Pusher
}

// NewMailHandler creates a new MailHandler with the given stores, attachment
// storage, mailbox service and outbound delivery manager.
func NewMailHandler(stores *store.Stores, attStorage *storage.AttachmentStorage, ob *outbound.Manager, svc *imap_server.MailboxService, pusher imap_server.Pusher) *MailHandler {
	return &MailHandler{stores: stores, storage: attStorage, outbound: ob, svc: svc, pusher: pusher}
}

// foldersFor 返回当前用户的侧边栏文件夹列表（与 IMAP LIST 同源：
// IMAP 返回什么文件夹，Web 就显示什么）。
func (h *MailHandler) foldersFor(userID uint) []imap_server.FolderInfo {
	infos, err := h.svc.List(userID)
	if err != nil {
		log.Printf("web: 加载文件夹列表失败 user=%d: %v", userID, err)
		return nil
	}
	return infos
}

// userEmailOf 从 context 取当前用户完整邮箱（推送用），失败返回空串。
func userEmailOf(c *gin.Context) string {
	if cu, ok := c.Get("currentUser"); ok {
		if u, ok := cu.(*db.User); ok {
			return u.Username + "@" + u.Domain.Name
		}
	}
	return ""
}

// seqOfFolder 返回消息在文件夹中的序号（1 基，与 IMAP 序号排序一致）。
func (h *MailHandler) seqOfFolder(userID uint, folder string, msgID uint) uint32 {
	msgs, err := h.stores.Mails.ListAllByUserAndFolder(userID, folder)
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

// purgeMessages 永久删除邮件（含附件文件与配额回退）。
func (h *MailHandler) purgeMessages(userID uint, msgs []db.Message) {
	ids := make([]uint, 0, len(msgs))
	for i := range msgs {
		attachments, _ := h.stores.Attachments.ListByMessage(msgs[i].ID)
		for _, att := range attachments {
			_ = h.storage.Delete(att.FilePath)
			_ = h.stores.Users.UpdateUsedBytes(userID, -att.FileSize)
		}
		if err := h.stores.Attachments.DeleteByMessage(msgs[i].ID); err != nil {
			log.Printf("web: 删除附件记录失败 msg=%d: %v", msgs[i].ID, err)
		}
		ids = append(ids, msgs[i].ID)
	}
	if err := h.stores.Mails.DeleteMany(ids); err != nil {
		log.Printf("web: 删除邮件失败: %v", err)
	}
}

// Folder renders the generic mailbox page for any folder the IMAP layer
// exposes (INBOX / Sent / Drafts / Trash / custom mailboxes).
func (h *MailHandler) Folder(c *gin.Context) {
	userID := c.GetUint("userID")
	name, ok := h.svc.Canonical(userID, c.Param("name"))
	if !ok {
		c.String(http.StatusNotFound, "邮箱不存在")
		return
	}
	page := getPageParam(c, "page", 1)

	messages, total, err := h.svc.Messages(userID, name, page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载邮件列表失败: %v", err)
		return
	}

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	currentUser, _ := c.Get("currentUser")
	c.HTML(200, "folder", gin.H{
		"currentUser":  currentUser,
		"messages":     messages,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       name,
		"activeFolder": name,
		"isTrash":      name == "Trash",
		"folders":      h.foldersFor(userID),
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
		if err := h.stores.Mails.MarkRead(uint(id)); err != nil {
			log.Printf("web: 标记已读失败 msg=%d: %v", id, err)
		}
		msg.IsRead = true
	}

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "view", gin.H{
		"currentUser":  currentUser,
		"message":      msg,
		"attachments":  attachments,
		"activeFolder": msg.Folder,
		"inTrash":      msg.Folder == "Trash",
		"folders":      h.foldersFor(userID),
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

	c.HTML(200, "compose", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "compose",
		"error":        "",
		"to":           c.Query("to"),
		"subject":      c.Query("subject"),
		"bodyContent":  "",
		"usedBytes":    usedBytes,
		"quotaBytes":   quotaBytes,
		"folders":      h.foldersFor(userID),
	})
}

// composeData builds the shared template context for the compose page.
func (h *MailHandler) composeData(userID uint, user *db.User, errMsg, to, subject, cc, body string) gin.H {
	return gin.H{
		"currentUser":  user,
		"activeFolder": "compose",
		"error":        errMsg,
		"to":           to,
		"subject":      subject,
		"cc":           cc,
		"bodyContent":  body,
		"usedBytes":    user.UsedBytes,
		"quotaBytes":   user.QuotaBytes,
		"folders":      h.foldersFor(userID),
	}
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
		c.HTML(http.StatusBadRequest, "compose", h.composeData(userID, currentUser, "请输入收件人", to, subject, cc, htmlBody))
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
				c.HTML(http.StatusInternalServerError, "compose", h.composeData(userID, currentUser, "配额检查失败，请稍后重试", to, subject, cc, htmlBody))
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
					"folders":      h.foldersFor(userID),
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
			c.HTML(http.StatusBadRequest, "compose", h.composeData(userID, currentUser, "外部投递未启用", to, subject, cc, htmlBody))
			return
		}
		if maxRcpt := ob.MaxRecipients(); maxRcpt > 0 && len(externalRecipients) > maxRcpt {
			c.HTML(http.StatusBadRequest, "compose", h.composeData(userID, currentUser, fmt.Sprintf("外部收件人过多：最多 %d 个", maxRcpt), to, subject, cc, htmlBody))
			return
		}
		for _, rcpt := range externalRecipients {
			if _, err := ob.Enqueue(currentUser, fromAddr, rcpt, []byte(rawMessage)); err != nil {
				c.HTML(http.StatusBadRequest, "compose", h.composeData(userID, currentUser, fmt.Sprintf("外发邮件入队失败 (%s): %v", rcpt, err), to, subject, cc, htmlBody))
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
			c.HTML(http.StatusInternalServerError, "compose", h.composeData(userID, currentUser, fmt.Sprintf("投递邮件失败: %v", createErr), to, subject, cc, htmlBody))
			return
		}
		// 本地投递成功 → IMAP 新邮件推送（IDLE 客户端实时收到通知）
		if h.pusher != nil {
			h.pusher.PushNewMessage(rcptUser.Username+"@"+rcptUser.Domain.Name, inboxMsg)
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
		c.HTML(http.StatusInternalServerError, "compose", h.composeData(userID, currentUser, fmt.Sprintf("保存邮件失败: %v", createErr), to, subject, cc, htmlBody))
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

// safeRedirectPath 仅接受同站相对路径（以 / 开头且非 //），
// 防止把用户重定向到外部站点（开放重定向）。非法值返回空串，
// 调用方应回退到默认路径。
func safeRedirectPath(referer string) string {
	if referer == "" || !strings.HasPrefix(referer, "/") || strings.HasPrefix(referer, "//") {
		return ""
	}
	return referer
}

// Delete 删除邮件（IMAP 语义）：非 Trash 文件夹 → 移入 Trash；
// 已在 Trash → 彻底删除。
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
	userEmail := userEmailOf(c)

	if msg.Folder == "Trash" {
		// 垃圾箱中删除 = 彻底删除
		seq := h.seqOfFolder(userID, msg.Folder, msg.ID)
		h.purgeMessages(userID, []db.Message{*msg})
		if h.pusher != nil && userEmail != "" {
			h.pusher.PushExpunged(userEmail, msg.Folder, []uint32{seq})
		}
	} else {
		// 其余文件夹删除 = 移入垃圾箱（与 IMAP MOVE 同源语义）
		seq := h.seqOfFolder(userID, msg.Folder, msg.ID)
		if err := h.svc.Move(userID, []uint{msg.ID}, "Trash"); err != nil {
			log.Printf("web: 移入垃圾箱失败 msg=%d: %v", id, err)
			c.String(http.StatusInternalServerError, "删除失败")
			return
		}
		if h.pusher != nil && userEmail != "" {
			h.pusher.PushExpunged(userEmail, msg.Folder, []uint32{seq})
			h.pusher.PushNewMessage(userEmail, &db.Message{UserID: userID, Folder: "Trash"})
		}
	}

	// Redirect back based on the folder（仅同站相对路径，防开放重定向）
	referer := safeRedirectPath(c.GetHeader("Referer"))
	if referer == "" {
		referer = "/folder/" + msg.Folder
	}
	c.Redirect(http.StatusFound, referer)
}

// Restore 把垃圾箱中的邮件恢复到收件箱。
func (h *MailHandler) Restore(c *gin.Context) {
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
	if msg.Folder != "Trash" {
		c.Redirect(http.StatusFound, "/folder/"+msg.Folder)
		return
	}

	seq := h.seqOfFolder(userID, "Trash", msg.ID)
	if err := h.svc.Move(userID, []uint{msg.ID}, "INBOX"); err != nil {
		log.Printf("web: 恢复邮件失败 msg=%d: %v", id, err)
		c.String(http.StatusInternalServerError, "恢复失败")
		return
	}
	if h.pusher != nil {
		if email := userEmailOf(c); email != "" {
			h.pusher.PushExpunged(email, "Trash", []uint32{seq})
			h.pusher.PushNewMessage(email, &db.Message{UserID: userID, Folder: "INBOX"})
		}
	}
	c.Redirect(http.StatusFound, "/folder/Trash")
}

// Purge 彻底删除一封邮件（任意文件夹）。
func (h *MailHandler) Purge(c *gin.Context) {
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

	seq := h.seqOfFolder(userID, msg.Folder, msg.ID)
	h.purgeMessages(userID, []db.Message{*msg})
	if h.pusher != nil {
		if email := userEmailOf(c); email != "" {
			h.pusher.PushExpunged(email, msg.Folder, []uint32{seq})
		}
	}

	referer := safeRedirectPath(c.GetHeader("Referer"))
	if referer == "" {
		referer = "/folder/" + msg.Folder
	}
	c.Redirect(http.StatusFound, referer)
}

// EmptyFolder 清空文件夹（永久删除其中全部邮件）。
func (h *MailHandler) EmptyFolder(c *gin.Context) {
	userID := c.GetUint("userID")
	name, ok := h.svc.Canonical(userID, c.Param("name"))
	if !ok {
		c.String(http.StatusNotFound, "邮箱不存在")
		return
	}

	msgs, err := h.stores.Mails.ListAllByUserAndFolder(userID, name)
	if err != nil {
		c.String(http.StatusInternalServerError, "清空文件夹失败: %v", err)
		return
	}
	seqs := make([]uint32, 0, len(msgs))
	for i := range msgs {
		seqs = append(seqs, uint32(i+1))
	}
	h.purgeMessages(userID, msgs)
	if h.pusher != nil {
		if email := userEmailOf(c); email != "" {
			h.pusher.PushExpunged(email, name, seqs)
		}
	}
	c.Redirect(http.StatusFound, "/folder/"+name)
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

	// 已读变化 → 推送给该用户的其他 IMAP 客户端
	if h.pusher != nil {
		msg.IsRead = true
		userEmail := ""
		if cu, ok := c.Get("currentUser"); ok {
			if u, ok := cu.(*db.User); ok {
				userEmail = u.Username + "@" + u.Domain.Name
			}
		}
		h.pusher.PushFlagsChanged(userEmail, msg.Folder, msg)
	}

	// Redirect back based on the folder（仅同站相对路径，防开放重定向）
	referer := safeRedirectPath(c.GetHeader("Referer"))
	if referer == "" {
		referer = "/folder/INBOX"
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

// Settings renders the user settings page.
func (h *MailHandler) Settings(c *gin.Context) {
	currentUser, _ := c.Get("currentUser")
	userID := c.GetUint("userID")
	c.HTML(200, "settings", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "settings",
		"error":        "",
		"success":      "",
		"mustChange":   c.Query("force") == "1",
		"folders":      h.foldersFor(userID),
	})
}

// settingsData builds the shared template context for the settings page.
func (h *MailHandler) settingsData(userID uint, user *db.User, errMsg, success string) gin.H {
	return gin.H{
		"currentUser":  user,
		"activeFolder": "settings",
		"error":        errMsg,
		"success":      success,
		"folders":      h.foldersFor(userID),
	}
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
		c.HTML(http.StatusBadRequest, "settings", h.settingsData(userID, currentUser, "当前密码不正确", ""))
		return
	}

	if newPassword == "" {
		c.HTML(http.StatusBadRequest, "settings", h.settingsData(userID, currentUser, "新密码不能为空", ""))
		return
	}

	if newPassword != confirmPassword {
		c.HTML(http.StatusBadRequest, "settings", h.settingsData(userID, currentUser, "两次输入的密码不一致", ""))
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		c.HTML(http.StatusInternalServerError, "settings", h.settingsData(userID, currentUser, "密码加密失败", ""))
		return
	}

	if err := h.stores.Users.UpdatePassword(userID, string(hashedPassword)); err != nil {
		c.HTML(http.StatusInternalServerError, "settings", h.settingsData(userID, currentUser, "密码更新失败", ""))
		return
	}

	c.HTML(http.StatusOK, "settings", h.settingsData(userID, currentUser, "", "密码修改成功"))
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
