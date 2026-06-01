package db

import (
	"time"
)

// User represents a mail user in the system.
type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:64;not null" json:"username"`
	PasswordHash string    `gorm:"size:255;not null" json:"-"`
	DomainID     uint      `gorm:"index" json:"domain_id"`
	Domain       Domain    `gorm:"foreignKey:DomainID" json:"domain"`
	QuotaBytes   int64     `gorm:"default:5368709120" json:"quota_bytes"`
	UsedBytes    int64     `gorm:"default:0" json:"used_bytes"`
	IsActive     bool      `gorm:"default:true" json:"is_active"`
	IsAdmin      bool      `gorm:"default:false" json:"is_admin"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// TableName specifies the table name for User.
func (User) TableName() string {
	return "users"
}

// Domain represents a mail domain in the system.
type Domain struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:255;uniqueIndex;not null" json:"name"`
	SmtpPort    int       `gorm:"default:25" json:"smtp_port"`
	ImapPort    int       `gorm:"default:143" json:"imap_port"`
	Pop3Port    int       `gorm:"default:110" json:"pop3_port"`
	TlsCertPath string    `gorm:"size:512" json:"tls_cert_path"`
	TlsKeyPath  string    `gorm:"size:512" json:"tls_key_path"`
	TlsEnabled  bool      `gorm:"default:false" json:"tls_enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// TableName specifies the table name for Domain.
func (Domain) TableName() string {
	return "domains"
}

// Message represents an email message in the system.
type Message struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	UserID     uint      `gorm:"index;not null" json:"user_id"`
	User       User      `gorm:"foreignKey:UserID" json:"user"`
	MessageID  string    `gorm:"size:255;index" json:"message_id"`
	Folder     string    `gorm:"size:64;default:INBOX;index" json:"folder"`
	FromAddr   string    `gorm:"size:512;not null" json:"from_addr"`
	ToAddr     string    `gorm:"size:2048;not null" json:"to_addr"`
	CcAddr     string    `gorm:"size:2048" json:"cc_addr"`
	Subject    string    `gorm:"size:1024" json:"subject"`
	TextBody   string    `gorm:"type:text" json:"text_body"`
	HtmlBody   string    `gorm:"type:text" json:"html_body"`
	IsRead     bool      `gorm:"default:false" json:"is_read"`
	IsFlagged  bool      `gorm:"default:false" json:"is_flagged"`
	Date       time.Time `json:"date"`
	CreatedAt  time.Time `json:"created_at"`
}

// TableName specifies the table name for Message.
func (Message) TableName() string {
	return "messages"
}

// Attachment represents a file attached to an email message.
type Attachment struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	MessageID   uint      `gorm:"index;not null" json:"message_id"`
	Message     Message   `gorm:"foreignKey:MessageID" json:"message"`
	FileName    string    `gorm:"size:255;not null" json:"file_name"`
	FilePath    string    `gorm:"size:512;not null" json:"file_path"`
	ContentType string    `gorm:"size:128" json:"content_type"`
	FileSize    int64     `json:"file_size"`
	CreatedAt   time.Time `json:"created_at"`
}

// TableName specifies the table name for Attachment.
func (Attachment) TableName() string {
	return "attachments"
}
