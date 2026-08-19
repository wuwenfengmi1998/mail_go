package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGenerateSecretKey(t *testing.T) {
	key, err := generateSecretKey()
	if err != nil {
		t.Fatalf("generateSecretKey() error: %v", err)
	}
	// 32 随机字节 hex 编码 = 64 字符
	if len(key) != secretKeyRandomBytes*2 {
		t.Fatalf("key length = %d, want %d", len(key), secretKeyRandomBytes*2)
	}

	// 两次生成必须不同
	key2, err := generateSecretKey()
	if err != nil {
		t.Fatalf("generateSecretKey() error: %v", err)
	}
	if key == key2 {
		t.Fatal("generated keys must be unique")
	}

	if key == InsecureLegacySecretKey {
		t.Fatal("generated key must never equal the legacy insecure default")
	}
}

func TestLoadConfigFirstBootGeneratesSecretKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_go.toml")

	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom() error: %v", err)
	}
	if cfg.Web.SecretKey == "" {
		t.Fatal("secret key should be generated on first boot")
	}

	// 密钥必须落盘，保证重启后会话不失效
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if !strings.Contains(string(data), "secret_key = \""+cfg.Web.SecretKey+"\"") {
		t.Fatalf("generated secret key should be persisted, file content:\n%s", data)
	}

	// 第二次加载返回相同密钥（会话保持）
	cfg2, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("second loadConfigFrom() error: %v", err)
	}
	if cfg2.Web.SecretKey != cfg.Web.SecretKey {
		t.Fatalf("secret key must be stable across restarts: %q != %q", cfg2.Web.SecretKey, cfg.Web.SecretKey)
	}

	// 配置文件包含敏感信息，权限必须为 0600（Windows 无 POSIX 权限，跳过）
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat config file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Fatalf("config file mode = %o, want 0600", perm)
		}
	}
}

func TestLoadConfigBackfillsSecretKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_go.toml")

	// 模拟旧版本升级：配置文件中没有 secret_key 字段
	old := "[web]\naddr = \":9090\"\n\n[smtp]\ndomain = \"example.com\"\n"
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom() error: %v", err)
	}
	if cfg.Web.SecretKey == "" {
		t.Fatal("missing secret key should be backfilled")
	}
	// 原有字段保持不变
	if cfg.Web.Addr != ":9090" {
		t.Fatalf("existing field overwritten: addr = %q", cfg.Web.Addr)
	}
	if cfg.SMTP.Domain != "example.com" {
		t.Fatalf("existing field overwritten: domain = %q", cfg.SMTP.Domain)
	}
}

func TestLoadConfigReplacesLegacySecretKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_go.toml")

	content := "[web]\naddr = \":8080\"\nsecret_key = \"" + InsecureLegacySecretKey + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("loadConfigFrom() error: %v", err)
	}
	if cfg.Web.SecretKey == InsecureLegacySecretKey {
		t.Fatal("legacy insecure secret key must be replaced")
	}
	if cfg.Web.SecretKey == "" {
		t.Fatal("replacement key must be non-empty")
	}

	// 替换后的密钥必须落盘
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), InsecureLegacySecretKey) {
		t.Fatal("legacy key should be removed from the config file")
	}
	if !strings.Contains(string(data), cfg.Web.SecretKey) {
		t.Fatal("replaced key should be persisted")
	}
}

func TestSecretKeyEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mail_go.toml")

	// 先正常生成一个落盘密钥
	cfg1, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("first loadConfigFrom() error: %v", err)
	}

	// 环境变量覆盖运行时密钥
	envKey := "env-override-secret-key-0123456789abcdef"
	t.Setenv(SecretKeyEnvVar, envKey)

	cfg2, err := loadConfigFrom(path)
	if err != nil {
		t.Fatalf("second loadConfigFrom() error: %v", err)
	}
	if cfg2.Web.SecretKey != envKey {
		t.Fatalf("env var should override the file key: got %q", cfg2.Web.SecretKey)
	}

	// 环境变量的值不能落盘
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), envKey) {
		t.Fatal("env-provided secret must not be persisted to disk")
	}
	// 落盘密钥保持原值
	if !strings.Contains(string(data), cfg1.Web.SecretKey) {
		t.Fatal("file key should remain unchanged when env override is active")
	}
}

func TestValidateSecretKey(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty", "", true},
		{"legacy default", InsecureLegacySecretKey, true},
		{"too short", "short", true},
		{"valid hex key", "0123456789abcdef0123456789abcdef", false},
		{"valid env style", "env-override-secret-key-0123456789abcdef", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSecretKey(tc.key)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for key %q", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for key %q: %v", tc.key, err)
			}
		})
	}
}
