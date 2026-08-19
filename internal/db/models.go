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
	// MustChangePassword 为 true 时该用户（通常是初始管理员或被重置密码的
	// 用户）在首次登录后必须修改密码。
	MustChangePassword bool      `gorm:"default:false" json:"-"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// TableName specifies the table name for User.
func (User) TableName() string {
	return "users"
}

// Domain represents a mail domain in the system.
type Domain struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	Name           string    `gorm:"size:255;uniqueIndex;not null" json:"name"`
	SmtpPort       int       `gorm:"default:25" json:"smtp_port"`
	ImapPort       int       `gorm:"default:143" json:"imap_port"`
	Pop3Port       int       `gorm:"default:110" json:"pop3_port"`
	TlsCertPath    string    `gorm:"size:512" json:"tls_cert_path"`
	TlsKeyPath     string    `gorm:"size:512" json:"tls_key_path"`
	TlsEnabled     bool      `gorm:"default:false" json:"tls_enabled"`
	DkimSelector   string    `gorm:"size:64;default:default" json:"dkim_selector"`
	DkimPrivateKey string    `gorm:"size:4096" json:"-"`
	DkimPublicKey  string    `gorm:"size:1024" json:"-"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// TableName specifies the table name for Domain.
func (Domain) TableName() string {
	return "domains"
}

// Message represents an email message in the system.
type Message struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index;not null" json:"user_id"`
	User      User      `gorm:"foreignKey:UserID" json:"user"`
	MessageID string    `gorm:"size:255;index" json:"message_id"`
	Folder    string    `gorm:"size:64;default:INBOX;index" json:"folder"`
	FromAddr  string    `gorm:"size:512;not null" json:"from_addr"`
	ToAddr    string    `gorm:"size:2048;not null" json:"to_addr"`
	CcAddr    string    `gorm:"size:2048" json:"cc_addr"`
	Subject   string    `gorm:"size:1024" json:"subject"`
	TextBody  string    `gorm:"type:text" json:"text_body"`
	HtmlBody  string    `gorm:"type:text" json:"html_body"`
	RawData   string    `gorm:"type:mediumtext" json:"raw_data"`
	IsRead    bool      `gorm:"default:false" json:"is_read"`
	IsFlagged bool      `gorm:"default:false" json:"is_flagged"`
	Date      time.Time `json:"date"`
	CreatedAt time.Time `json:"created_at"`
}

// TableName specifies the table name for Message.
func (Message) TableName() string {
	return "messages"
}

// Outbound message delivery statuses.
const (
	OutboundStatusPending  = "pending"  // 等待发送
	OutboundStatusSending  = "sending"  // 发送中
	OutboundStatusSent     = "sent"     // 已送达
	OutboundStatusDeferred = "deferred" // 临时失败，等待重试
	OutboundStatusFailed   = "failed"   // 永久失败/超过重试上限
	OutboundStatusCanceled = "canceled" // 管理员取消
)

// OutboundMessage represents a message queued for delivery to an external domain.
type OutboundMessage struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	MessageID     string     `gorm:"size:255;index" json:"message_id"`
	UserID        uint       `gorm:"index" json:"user_id"` // 发件用户 ID（Web/SMTP 提交用户）
	FromAddr      string     `gorm:"size:512;not null" json:"from_addr"`
	ToAddr        string     `gorm:"size:512;not null" json:"to_addr"`
	RecipientDom  string     `gorm:"size:255;index" json:"recipient_dom"`
	RawData       string     `gorm:"type:mediumtext" json:"-"` // DKIM 签名后的完整邮件
	Status        string     `gorm:"size:32;default:pending;index" json:"status"`
	Attempts      int        `gorm:"default:0" json:"attempts"`
	NextAttemptAt time.Time  `gorm:"index" json:"next_attempt_at"`
	LastResponse  string     `gorm:"size:1024" json:"last_response"`
	LastError     string     `gorm:"size:1024" json:"last_error"`
	CompletedAt   *time.Time `json:"completed_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// TableName specifies the table name for OutboundMessage.
func (OutboundMessage) TableName() string {
	return "outbound_messages"
}

// BanEntry represents an IP address that has been banned due to excessive login failures.
type BanEntry struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	IPAddress string    `gorm:"size:45;index;not null" json:"ip_address"`
	Reason    string    `gorm:"size:255" json:"reason"`
	FailCount int       `gorm:"default:0" json:"fail_count"`
	// BanCount 是该 IP 累计达到失败阈值的次数（含未封禁的前几次）。
	// 阶段封禁依据：前 3 次只计数不封禁，第 4 次起按档位递增时长。
	// 成功登录或管理员解封会删除记录，次数随之清零。
	BanCount  int       `gorm:"default:0" json:"ban_count"`
	ExpiresAt time.Time `gorm:"index" json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TableName specifies the table name for BanEntry.
func (BanEntry) TableName() string { return "ban_entries" }

// Protocol log statuses.
const (
	ProtocolSMTP = "smtp"
	ProtocolIMAP = "imap"
	ProtocolPOP3 = "pop3"
)

// ProtocolLog records one SMTP/IMAP/POP3 connection session: auth result,
// failure reason and source IP, for admin analysis of attacks/abuse.
type ProtocolLog struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	Protocol   string    `gorm:"size:16;index;not null" json:"protocol"` // smtp | imap | pop3
	Port       int       `json:"port"`                                   // 25/465/587/143/993/110/995
	ClientIP   string    `gorm:"size:64;index;not null" json:"client_ip"`
	Username   string    `gorm:"size:255;index" json:"username"`
	Success    bool      `gorm:"index" json:"success"`
	FailReason string    `gorm:"size:512" json:"fail_reason"`
	Detail     string    `gorm:"size:2048" json:"detail"`
	MsgCount   int       `json:"msg_count"`
	DurationMs int64     `json:"duration_ms"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// TableName specifies the table name for ProtocolLog.
func (ProtocolLog) TableName() string {
	return "protocol_logs"
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

// MailboxState 记录每个邮箱（用户+文件夹）的持久化 IMAP 状态。
// UidValidity 在首次访问时随机生成并持久化：数据库重建（消息 ID 空间
// 变化）后该值随之改变，客户端（Thunderbird 等）会据此丢弃本地缓存
// 并全量重新同步。此前硬编码为 1，数据库重建后客户端缓存永不失效，
// 导致只显示/下载少量"缺失"邮件。
type MailboxState struct {
	UserID      uint   `gorm:"primaryKey" json:"user_id"`
	Folder      string `gorm:"primaryKey;size:64" json:"folder"`
	UidValidity uint32 `gorm:"not null" json:"uid_validity"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName specifies the table name for MailboxState.
func (MailboxState) TableName() string {
	return "mailbox_states"
}
