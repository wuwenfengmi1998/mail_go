package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

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
}

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
			Addr: DefaultWebPort,
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
	return cfg
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
	return nil
}

// LoadConfig loads the configuration from disk.
// If the configuration file does not exist, it creates one with default values.
// If the file exists but has missing fields, they are filled with defaults and the file is updated.
func LoadConfig() (*Config, error) {
	path := configFilePath()
	defaults := defaultConfig()

	// If config file doesn't exist, create it with defaults
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if mkErr := writeConfig(path, defaults); mkErr != nil {
			return nil, mkErr
		}
		return defaults, nil
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

	// Merge defaults for any missing fields
	merged := mergeDefaults(cfg, defaults)

	// Write back if any fields were filled in from defaults
	// (always write back to ensure the file has all fields)
	if writeErr := writeConfig(path, merged); writeErr != nil {
		return nil, writeErr
	}

	return merged, nil
}
