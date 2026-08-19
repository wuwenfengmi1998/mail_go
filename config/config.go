package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

// DatabaseConfig holds database connection settings.
type DatabaseConfig struct {
	Driver string `toml:"driver"`
	DSN    string `toml:"dsn"`
}

// StorageConfig holds file storage paths.
type StorageConfig struct {
	BaseDir   string `toml:"base_dir"`
	AttachDir string `toml:"attach_dir"`
}

// WebConfig holds web server settings.
type WebConfig struct {
	Addr string `toml:"addr"`
	// SecretKey 是 Web 会话 cookie 的签名密钥。留空时首次启动自动生成
	// 随机密钥并持久化到配置文件；也可通过环境变量 MAILGO_SECRET_KEY
	// 覆盖（覆盖值不落盘，适合容器部署）。
	SecretKey string `toml:"secret_key"`
	// CookieSecure 控制会话 cookie 是否仅通过 HTTPS 传输（Secure 标志）。
	// 默认 true；仅当应用直接以 HTTP 提供服务（本地调试、内网明文）时
	// 才应改为 false。
	CookieSecure bool `toml:"cookie_secure"`
	// ProtocolLogKeepDays SMTP/IMAP/POP3 协议调用日志保留天数，
	// 超过该天数的记录会被后台任务自动清理。
	ProtocolLogKeepDays int `toml:"protocol_log_keep_days"`
	// Timezone Web 界面显示时间所用的 IANA 时区（如 "Asia/Shanghai"）。
	// 为空时使用服务器本地时区。邮件日期在库中统一为 UTC 存储，
	// 展示时按此配置转换。
	Timezone string `toml:"timezone"`
}

// SecretKeyEnvVar 是覆盖会话签名密钥的环境变量名。
const SecretKeyEnvVar = "MAILGO_SECRET_KEY"

// InsecureLegacySecretKey 是旧版本硬编码在源码中的会话签名密钥。
// 源码公开意味着该密钥完全不可信，任何出现都必须替换。
const InsecureLegacySecretKey = "mail-go-secret-key-change-in-production"

// MinSecretKeyLen 是允许的最短会话密钥长度（字节）。
const MinSecretKeyLen = 16

// secretKeyRandomBytes 是自动生成密钥的随机字节数（hex 编码后 64 字符）。
const secretKeyRandomBytes = 32

// SMTPConfig holds SMTP server settings.
type SMTPConfig struct {
	Addr           string `toml:"addr"`
	TLSAddr        string `toml:"tls_addr"`
	SubmissionAddr string `toml:"submission_addr"`
	Domain         string `toml:"domain"`
	TLSCert        string `toml:"tls_cert"`
	TLSKey         string `toml:"tls_key"`
	MaxMessage     int64  `toml:"max_message_bytes"`
}

// IMAPConfig holds IMAP server settings.
type IMAPConfig struct {
	Addr    string `toml:"addr"`
	TLSAddr string `toml:"tls_addr"`
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
}

// POP3Config holds POP3 server settings.
type POP3Config struct {
	Addr    string `toml:"addr"`
	TLSAddr string `toml:"tls_addr"`
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
}

// AuthConfig holds external authentication settings (OAuth2, LDAP).
type AuthConfig struct {
	// OAuth2 configuration
	OAuth2Enabled      bool   `toml:"oauth2_enabled"`
	OAuth2Provider     string `toml:"oauth2_provider"` // google, github, gitlab
	OAuth2ClientID     string `toml:"oauth2_client_id"`
	OAuth2ClientSecret string `toml:"oauth2_client_secret"`
	OAuth2RedirectURL  string `toml:"oauth2_redirect_url"`

	// LDAP configuration
	LDAPEnabled      bool   `toml:"ldap_enabled"`
	LDAPServer       string `toml:"ldap_server"`  // e.g. ldap://localhost:389
	LDAPBindDN       string `toml:"ldap_bind_dn"` // e.g. cn=admin,dc=example,dc=com
	LDAPBindPassword string `toml:"ldap_bind_password"`
	LDAPSearchBase   string `toml:"ldap_search_base"`   // e.g. ou=users,dc=example,dc=com
	LDAPSearchFilter string `toml:"ldap_search_filter"` // e.g. (uid=%s)
	LDAPUseTLS       bool   `toml:"ldap_use_tls"`
}

// BanConfig holds IP ban settings for failed login attempts.
type BanConfig struct {
	MaxFailAttempts int `toml:"max_fail_attempts"` // Default: 5
	BanDurationMin  int `toml:"ban_duration_min"`  // Default: 30 (minutes)
}

// CaddyConfig holds settings for importing TLS certificates from a local Caddy.
type CaddyConfig struct {
	// DataDir is the Caddy data directory (the one containing the
	// "certificates/" subdirectory), used by the one-click certificate
	// import in the admin panel. Leave empty to auto-detect common
	// locations such as /var/lib/caddy/.local/share/caddy.
	DataDir string `toml:"data_dir"`
}

// OutboundConfig holds outbound (external) mail delivery settings.
type OutboundConfig struct {
	Hostname       string `toml:"hostname"`        // EHLO 主机名，留空使用 [smtp] domain
	PollInterval   int    `toml:"poll_interval"`   // 队列扫描间隔（秒）
	MaxAttempts    int    `toml:"max_attempts"`    // 单封邮件最大投递尝试次数
	RetryBaseMin   int    `toml:"retry_base_min"`  // 重试退避基数（分钟），指数增长
	MaxRecipients  int    `toml:"max_recipients"`  // 单封邮件最大外部收件人数
	MaxPerMin      int    `toml:"max_per_min"`     // 每用户每分钟最大外发数
	MaxPerDay      int    `toml:"max_per_day"`     // 每用户每日最大外发数，0 表示禁用外部投递
	ConnectTimeout int    `toml:"connect_timeout"` // 连接远程 MX 超时（秒）

	// Workers 并发投递 worker 数：多 goroutine 并行发送队列中的邮件。
	// 0 或 1 表示串行（旧行为）。
	Workers int `toml:"workers"`
	// BatchSize 每次扫描最多取出的待投递邮件数。
	BatchSize int `toml:"batch_size"`
	// MaxConcurrentPerDomain 同一收件域（或中继）的最大并发连接数，
	// 防止对单个 MX 域并发过多而被判定为滥发；0 表示不限制。
	MaxConcurrentPerDomain int `toml:"max_concurrent_per_domain"`

	// Smarthost relay: when relay_host is non-empty, all external mail is
	// delivered through this relay instead of direct MX delivery. Useful when
	// the server IP is listed in PBL/blocklists (residential/dynamic IPs).
	RelayHost     string `toml:"relay_host"`     // 中继服务器地址，留空则直投 MX
	RelayPort     int    `toml:"relay_port"`     // 465 = 隐式 TLS，其他端口先尝试 STARTTLS
	RelayUser     string `toml:"relay_user"`     // 中继认证用户名（AUTH PLAIN）
	RelayPassword string `toml:"relay_password"` // 中继认证密码
	RelayStartTLS bool   `toml:"relay_starttls"` // 非 465 端口是否使用 STARTTLS
	// RelayTLSInsecure 是否跳过中继服务器的 TLS 证书验证。
	// 默认 false（验证证书），避免凭据被中间人截获；仅当使用自签证书的
	// 内网中继且明确知晓风险时才设为 true。
	RelayTLSInsecure bool `toml:"relay_tls_insecure"`

	// IP family and source address binding for outbound connections.
	IPFamily string `toml:"ip_family"` // ipv4（默认，PTR/SPF 最可靠）| ipv6 | auto
	SourceIP string `toml:"source_ip"` // 出站源地址绑定（如静态 IPv6），留空由内核选择
}

// Config is the top-level configuration structure.
type Config struct {
	Database DatabaseConfig `toml:"database"`
	Storage  StorageConfig  `toml:"storage"`
	Web      WebConfig      `toml:"web"`
	SMTP     SMTPConfig     `toml:"smtp"`
	IMAP     IMAPConfig     `toml:"imap"`
	POP3     POP3Config     `toml:"pop3"`
	Auth     AuthConfig     `toml:"auth"`
	Ban      BanConfig      `toml:"ban"`
	Caddy    CaddyConfig    `toml:"caddy"`
	Outbound OutboundConfig `toml:"outbound"`
}

// isWindows returns true if the current OS is Windows.
func isWindows() bool {
	return runtime.GOOS == "windows"
}

// etcDir returns the etc directory based on the current OS.
func etcDir() string {
	if isWindows() {
		return WinEtcDir
	}
	return LinuxEtcDir
}

// baseDir returns the base data directory based on the current OS.
func baseDir() string {
	if isWindows() {
		return WinBaseDir
	}
	return LinuxBaseDir
}

// defaultDSN returns the default database DSN based on the current OS.
func defaultDSN() string {
	if isWindows() {
		return DefaultDSNWin
	}
	return DefaultDSNLinux
}

// defaultConfig returns a fully populated Config with default values.
func defaultConfig() *Config {
	bd := baseDir()
	return &Config{
		Database: DatabaseConfig{
			Driver: DefaultDBDriver,
			DSN:    defaultDSN(),
		},
		Storage: StorageConfig{
			BaseDir:   bd,
			AttachDir: filepath.Join(bd, "attachments"),
		},
		Web: WebConfig{
			Addr:                DefaultWebPort,
			CookieSecure:        true,
			ProtocolLogKeepDays: DefaultProtocolLogKeepDays,
			Timezone:            DefaultTimezone,
		},
		SMTP: SMTPConfig{
			Addr:           fmt.Sprintf(":%d", DefaultSMTPPort),
			TLSAddr:        fmt.Sprintf(":%d", DefaultSMTPTLSPort),
			SubmissionAddr: fmt.Sprintf(":%d", DefaultSMTPSubmitPort),
			Domain:         "localhost",
			MaxMessage:     64 * 1024 * 1024, // 64MB
		},
		IMAP: IMAPConfig{
			Addr:    fmt.Sprintf(":%d", DefaultIMAPPort),
			TLSAddr: fmt.Sprintf(":%d", DefaultIMAPTLSPort),
		},
		POP3: POP3Config{
			Addr:    fmt.Sprintf(":%d", DefaultPOP3Port),
			TLSAddr: fmt.Sprintf(":%d", DefaultPOP3TLSPort),
		},
		Auth: AuthConfig{
			OAuth2Enabled: false,
			LDAPEnabled:   false,
		},
		Ban: BanConfig{
			MaxFailAttempts: 5,
			BanDurationMin:  30,
		},
		// Caddy: 留空则自动探测常见数据目录，无需配置
		Caddy: CaddyConfig{},
		Outbound: OutboundConfig{
			PollInterval:           15, // 15 秒扫描一次队列
			MaxAttempts:            12, // 最多尝试 12 次
			RetryBaseMin:           5,  // 5/10/20/40/... 分钟指数退避
			MaxRecipients:          50, // 单封最多 50 个外部收件人
			MaxPerMin:              30, // 每用户每分钟 30 封
			MaxPerDay:              500,
			ConnectTimeout:         30,  // 连接远程 MX 超时 30 秒
			RelayPort:              587, // smarthost 默认提交端口
			RelayStartTLS:          true,
			IPFamily:               "ipv4",
			Workers:                DefaultOutboundWorkers,
			BatchSize:              DefaultOutboundBatchSize,
			MaxConcurrentPerDomain: DefaultMaxConcurrentPerDomain,
		},
	}
}

// configFilePath returns the full path to the configuration file.
func configFilePath() string {
	return filepath.Join(etcDir(), ConfigFileName)
}

// mergeDefaults overlays default values onto the loaded config for any zero/empty fields.
func mergeDefaults(cfg *Config, defaults *Config) *Config {
	if cfg.Database.Driver == "" {
		cfg.Database.Driver = defaults.Database.Driver
	}
	if cfg.Database.DSN == "" {
		cfg.Database.DSN = defaults.Database.DSN
	}
	if cfg.Storage.BaseDir == "" {
		cfg.Storage.BaseDir = defaults.Storage.BaseDir
	}
	if cfg.Storage.AttachDir == "" {
		cfg.Storage.AttachDir = defaults.Storage.AttachDir
	}
	if cfg.Web.Addr == "" {
		cfg.Web.Addr = defaults.Web.Addr
	}
	if cfg.Web.ProtocolLogKeepDays == 0 {
		cfg.Web.ProtocolLogKeepDays = defaults.Web.ProtocolLogKeepDays
	}
	if cfg.Web.Timezone == "" {
		cfg.Web.Timezone = defaults.Web.Timezone
	}
	if cfg.SMTP.Addr == "" {
		cfg.SMTP.Addr = defaults.SMTP.Addr
	}
	if cfg.SMTP.TLSAddr == "" {
		cfg.SMTP.TLSAddr = defaults.SMTP.TLSAddr
	}
	if cfg.SMTP.SubmissionAddr == "" {
		cfg.SMTP.SubmissionAddr = defaults.SMTP.SubmissionAddr
	}
	if cfg.SMTP.Domain == "" {
		cfg.SMTP.Domain = defaults.SMTP.Domain
	}
	if cfg.SMTP.MaxMessage == 0 {
		cfg.SMTP.MaxMessage = defaults.SMTP.MaxMessage
	}
	if cfg.IMAP.Addr == "" {
		cfg.IMAP.Addr = defaults.IMAP.Addr
	}
	if cfg.IMAP.TLSAddr == "" {
		cfg.IMAP.TLSAddr = defaults.IMAP.TLSAddr
	}
	if cfg.POP3.Addr == "" {
		cfg.POP3.Addr = defaults.POP3.Addr
	}
	if cfg.POP3.TLSAddr == "" {
		cfg.POP3.TLSAddr = defaults.POP3.TLSAddr
	}
	// Auth defaults: no merging needed since booleans default to false
	// and string fields are intentionally empty when disabled
	if cfg.Ban.MaxFailAttempts == 0 {
		cfg.Ban.MaxFailAttempts = defaults.Ban.MaxFailAttempts
	}
	if cfg.Ban.BanDurationMin == 0 {
		cfg.Ban.BanDurationMin = defaults.Ban.BanDurationMin
	}
	if cfg.Outbound.PollInterval == 0 {
		cfg.Outbound.PollInterval = defaults.Outbound.PollInterval
	}
	if cfg.Outbound.MaxAttempts == 0 {
		cfg.Outbound.MaxAttempts = defaults.Outbound.MaxAttempts
	}
	if cfg.Outbound.RetryBaseMin == 0 {
		cfg.Outbound.RetryBaseMin = defaults.Outbound.RetryBaseMin
	}
	if cfg.Outbound.MaxRecipients == 0 {
		cfg.Outbound.MaxRecipients = defaults.Outbound.MaxRecipients
	}
	if cfg.Outbound.MaxPerMin == 0 {
		cfg.Outbound.MaxPerMin = defaults.Outbound.MaxPerMin
	}
	if cfg.Outbound.MaxPerDay == 0 {
		cfg.Outbound.MaxPerDay = defaults.Outbound.MaxPerDay
	}
	if cfg.Outbound.ConnectTimeout == 0 {
		cfg.Outbound.ConnectTimeout = defaults.Outbound.ConnectTimeout
	}
	if cfg.Outbound.Workers == 0 {
		cfg.Outbound.Workers = defaults.Outbound.Workers
	}
	if cfg.Outbound.BatchSize == 0 {
		cfg.Outbound.BatchSize = defaults.Outbound.BatchSize
	}
	if cfg.Outbound.MaxConcurrentPerDomain == 0 {
		cfg.Outbound.MaxConcurrentPerDomain = defaults.Outbound.MaxConcurrentPerDomain
	}
	if cfg.Outbound.RelayPort == 0 {
		cfg.Outbound.RelayPort = defaults.Outbound.RelayPort
	}
	if cfg.Outbound.IPFamily == "" {
		cfg.Outbound.IPFamily = defaults.Outbound.IPFamily
	}
	return cfg
}

// generateSecretKey generates a cryptographically random session key,
// hex-encoded (64 characters for 32 random bytes).
func generateSecretKey() (string, error) {
	buf := make([]byte, secretKeyRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成随机会话密钥失败: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ensureSecretKey guarantees that cfg holds a trustworthy session key:
// an empty value or the known insecure legacy default is replaced with a
// freshly generated random key. The caller is responsible for persisting
// the updated config.
func ensureSecretKey(cfg *Config) error {
	if cfg.Web.SecretKey != "" && cfg.Web.SecretKey != InsecureLegacySecretKey {
		return nil
	}
	key, err := generateSecretKey()
	if err != nil {
		return err
	}
	if cfg.Web.SecretKey == InsecureLegacySecretKey {
		log.Printf("检测到不安全的旧默认会话密钥，已自动更换为随机密钥（所有已登录会话将失效）")
	} else {
		log.Printf("已生成随机会话密钥并写入配置文件（Web 会话签名密钥，请妥善备份）")
	}
	cfg.Web.SecretKey = key
	return nil
}

// applySecretKeyEnv lets the MAILGO_SECRET_KEY environment variable
// override the key loaded from the config file. The override value is
// never persisted to disk.
func applySecretKeyEnv(cfg *Config) *Config {
	if env := os.Getenv(SecretKeyEnvVar); env != "" {
		cfg.Web.SecretKey = env
	}
	return cfg
}

// ValidateSecretKey rejects session signing keys that are missing, too
// short, or the known insecure legacy default hardcoded in old versions.
func ValidateSecretKey(key string) error {
	if key == "" {
		return fmt.Errorf("Web 会话密钥为空，拒绝启动：请检查配置文件 [web].secret_key 或环境变量 %s", SecretKeyEnvVar)
	}
	if key == InsecureLegacySecretKey {
		return fmt.Errorf("Web 会话密钥为已知不安全的旧默认值，拒绝启动：请删除配置文件 [web].secret_key 后重启以自动生成随机密钥")
	}
	if len(key) < MinSecretKeyLen {
		return fmt.Errorf("Web 会话密钥过短（%d 字节，最少 %d）：请检查 %s 或 [web].secret_key",
			len(key), MinSecretKeyLen, SecretKeyEnvVar)
	}
	return nil
}

// writeConfig writes the configuration to the given file path.
// It creates the parent directories if they don't exist.
func writeConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建配置目录失败 %s: %w", dir, err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建配置文件失败 %s: %w", path, err)
	}
	defer f.Close()

	enc := toml.NewEncoder(f)
	if err := enc.Encode(cfg); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	// 配置文件包含会话密钥、中继密码等敏感信息，收紧为仅属主可读写
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("设置配置文件权限失败 %s: %w", path, err)
	}
	return nil
}

// LoadConfig loads the configuration from disk.
// If the configuration file does not exist, it creates one with default values.
// If the file exists but has missing fields, they are filled with defaults and the file is updated.
func LoadConfig() (*Config, error) {
	return loadConfigFrom(configFilePath())
}

// loadConfigFrom implements LoadConfig against an explicit file path so it
// can be unit-tested with temporary directories.
func loadConfigFrom(path string) (*Config, error) {
	defaults := defaultConfig()

	// If config file doesn't exist, create it with defaults
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := ensureSecretKey(defaults); err != nil {
			return nil, err
		}
		if mkErr := writeConfig(path, defaults); mkErr != nil {
			return nil, mkErr
		}
		return applySecretKeyEnv(defaults), nil
	}

	// Read existing config file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}

	cfg := &Config{}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	// relay_starttls 与 web.cookie_secure 默认值为 true for safety;
	// the raw file is checked because TOML decoding cannot distinguish
	// an absent bool from false.
	if !strings.Contains(string(data), "relay_starttls") {
		cfg.Outbound.RelayStartTLS = defaults.Outbound.RelayStartTLS
	}
	if !strings.Contains(string(data), "cookie_secure") {
		cfg.Web.CookieSecure = defaults.Web.CookieSecure
	}

	// 会话密钥缺失或不安全时补发随机密钥（随下面的写回一并落盘）
	if err := ensureSecretKey(cfg); err != nil {
		return nil, err
	}

	// Merge defaults for any missing fields
	merged := mergeDefaults(cfg, defaults)

	// Write back if any fields were filled in from defaults
	// (always write back to ensure the file has all fields)
	if writeErr := writeConfig(path, merged); writeErr != nil {
		return nil, writeErr
	}

	return applySecretKeyEnv(merged), nil
}
