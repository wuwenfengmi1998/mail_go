package config

// Linux path prefixes
const (
	LinuxEtcDir  = "/etc/mail_go/"
	LinuxBaseDir = "/srv/mail_go/"
)

// Windows path prefixes
const (
	WinEtcDir  = "./win/etc/mail_go/"
	WinBaseDir = "./win/srv/mail_go/"
)

// Default port constants
const (
	DefaultSMTPPort       = 25
	DefaultSMTPTLSPort    = 465
	DefaultSMTPSubmitPort = 587
	DefaultIMAPPort       = 143
	DefaultIMAPTLSPort    = 993
	DefaultPOP3Port       = 110
	DefaultPOP3TLSPort    = 995
	DefaultWebPort        = ":8080"
)

// Default database settings
const (
	DefaultDBDriver = "sqlite"
	DefaultDSNLinux = "/srv/mail_go/mail.db"
	DefaultDSNWin   = "./win/srv/mail_go/mail.db"
)

// Default quota
const (
	DefaultQuotaBytes int64 = 5 * 1024 * 1024 * 1024 // 5GB
)

// DefaultProtocolLogKeepDays 是 SMTP/IMAP/POP3 协议调用日志的默认保留天数。
const DefaultProtocolLogKeepDays = 30

// DefaultTimezone 是 Web 界面显示时间的默认 IANA 时区（北京时间）。
const DefaultTimezone = "Asia/Shanghai"

// Outbound delivery concurrency defaults.
const (
	// DefaultOutboundWorkers 并发投递 worker 数（0/1 为串行）。
	DefaultOutboundWorkers = 4
	// DefaultOutboundBatchSize 每次扫描最多取出的待投递邮件数。
	DefaultOutboundBatchSize = 50
	// DefaultMaxConcurrentPerDomain 同一收件域的最大并发连接数。
	DefaultMaxConcurrentPerDomain = 2
)

// ConfigFileName is the name of the configuration file
const ConfigFileName = "mail_go.toml"
