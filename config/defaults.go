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

// ConfigFileName is the name of the configuration file
const ConfigFileName = "mail_go.toml"
