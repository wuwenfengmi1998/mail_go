package handlers

import (
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mail_go/internal/db"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// MailHandler handles mail-related routes (inbox, compose, sent, view, etc.).
type MailHandler struct {
	stores  *store.Stores
	storage *storage.AttachmentStorage
}

// NewMailHandler creates a new MailHandler with the given stores and attachment storage.
func NewMailHandler(stores *store.Stores, attStorage *storage.AttachmentStorage) *MailHandler {
	return &MailHandler{stores: stores, storage: attStorage}
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

	unreadCount, _ := h.stores.Mails.CountUnread(userID, "INBOX")

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
		"unreadCount":  unreadCount,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       "INBOX",
		"activeFolder": "inbox",
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

	c.HTML(200, "view", gin.H{
		"currentUser":  currentUser,
		"message":      msg,
		"attachments":  attachments,
		"activeFolder": resolveActiveFolder(msg.Folder),
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
	if multipartErr == nil {
		files := form.File["attachments"]
		if len(files) > 0 {
			// Check attachment quota before saving
			user, _ := h.stores.Users.GetByID(userID)
			if user != nil {
				var totalNewSize int64
				for _, file := range files {
					totalNewSize += file.Size
				}
				if user.UsedBytes+totalNewSize > user.QuotaBytes {
					c.HTML(http.StatusBadRequest, "compose", gin.H{
						"currentUser":  currentUser,
						"activeFolder": "compose",
						"error":        fmt.Sprintf("附件超出配额限制。已用 %s / 总配额 %s", formatBytes(user.UsedBytes), formatBytes(user.QuotaBytes)),
						"to":           to,
						"subject":      subject,
						"cc":           cc,
						"bodyContent":  htmlBody,
						"usedBytes":    user.UsedBytes,
						"quotaBytes":   user.QuotaBytes,
					})
					return
				}
			}
		}
	}

	// Build the email content
	fromAddr := fmt.Sprintf("%s@%s", currentUser.Username, currentUser.Domain.Name)
	now := time.Now()
	messageID := fmt.Sprintf("<%s@mail_go>", uuid.New().String())

	// Construct the raw email message
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("From: %s\r\n", fromAddr))
	sb.WriteString(fmt.Sprintf("To: %s\r\n", to))
	if cc != "" {
		sb.WriteString(fmt.Sprintf("Cc: %s\r\n", cc))
	}
	sb.WriteString(fmt.Sprintf("Subject: %s\r\n", subject))
	sb.WriteString(fmt.Sprintf("Message-ID: %s\r\n", messageID))
	sb.WriteString(fmt.Sprintf("Date: %s\r\n", now.Format(time.RFC1123Z)))
	sb.WriteString("MIME-Version: 1.0\r\n")

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
	}

	// Send via local SMTP
	err := smtp.SendMail("localhost:25", nil, fromAddr, strings.Split(to, ","), []byte(sb.String()))
	if err != nil {
		// Log the error but still save to sent folder — SMTP may not be running yet
		fmt.Printf("SMTP发送失败（邮件仍保存到发件箱）: %v\n", err)
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

	// Handle attachments
	if multipartErr == nil {
		files := form.File["attachments"]
		for _, file := range files {
			// Read file content
			f, err := file.Open()
			if err != nil {
				continue
			}
			buf, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				continue
			}

			// Save to disk
			relPath, err := h.storage.Save(file.Filename, buf)
			if err != nil {
				continue
			}

			// Determine content type from extension
			contentType := "application/octet-stream"
			ext := strings.ToLower(filepath.Ext(file.Filename))
			if ct, ok := mimeTypes[ext]; ok {
				contentType = ct
			}

			att := &db.Attachment{
				MessageID:   msg.ID,
				FileName:    file.Filename,
				FilePath:    relPath,
				ContentType: contentType,
				FileSize:    file.Size,
			}
			_ = h.stores.Attachments.Create(att)
			// Update user used bytes
			_ = h.stores.Users.UpdateUsedBytes(userID, att.FileSize)
		}
	}

	c.Redirect(http.StatusFound, "/sent")
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
	})
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

	// Redirect back based on the folder
	referer := c.GetHeader("Referer")
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

	referer := c.GetHeader("Referer")
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

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", att.FileName))
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
	})
}

// Settings renders the user settings page.
func (h *MailHandler) Settings(c *gin.Context) {
	currentUser, _ := c.Get("currentUser")
	c.HTML(200, "settings", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "settings",
		"error":        "",
		"success":      "",
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
