package caddycert

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeCert 生成一份自签名证书（含指定 SAN），返回 PEM 编码的证书与私钥。
func makeCert(t *testing.T, dnsNames []string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// writeSite 在 Caddy 风格目录结构中写入某个域名的证书。
func writeSite(t *testing.T, dataDir, domain string, certPEM, keyPEM []byte) {
	t.Helper()
	dir := filepath.Join(dataDir, "certificates", "acme-v02.api.letsencrypt.org-directory", domain)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain+".crt"), certPEM, 0600); err != nil {
		t.Fatalf("写入证书失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, domain+".key"), keyPEM, 0600); err != nil {
		t.Fatalf("写入私钥失败: %v", err)
	}
}

func TestFetchExactDomain(t *testing.T) {
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dataDir, "mail.example.com", certPEM, keyPEM)

	got, err := Fetch("mail.example.com", []string{dataDir})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if string(got.CertPEM) != string(certPEM) {
		t.Error("返回的证书与写入的不一致")
	}
	if string(got.KeyPEM) != string(keyPEM) {
		t.Error("返回的私钥与写入的不一致")
	}
}

func TestFetchWildcardCoversSubdomain(t *testing.T) {
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"*.example.com", "example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dataDir, "*.example.com", certPEM, keyPEM)

	got, err := Fetch("mail.example.com", []string{dataDir})
	if err != nil {
		t.Fatalf("通配符证书应覆盖子域名，Fetch 失败: %v", err)
	}
	if got.Source == "" {
		t.Error("Source 不应为空")
	}
}

func TestFetchSkipsExpiredCert(t *testing.T) {
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	writeSite(t, dataDir, "mail.example.com", certPEM, keyPEM)

	_, err := Fetch("mail.example.com", []string{dataDir})
	if err == nil {
		t.Fatal("过期证书不应被返回")
	}
	if !strings.Contains(err.Error(), "无效") {
		t.Errorf("错误信息应说明证书无效，实际: %v", err)
	}
}

func TestFetchNotExist(t *testing.T) {
	dataDir := t.TempDir()

	_, err := Fetch("nobody.example.com", []string{dataDir})
	if err == nil {
		t.Fatal("不存在的域名应返回错误")
	}
	if !strings.Contains(err.Error(), "未找到") {
		t.Errorf("错误信息应包含“未找到”，实际: %v", err)
	}
}

func TestFetchUppercaseDomainIsLowercased(t *testing.T) {
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dataDir, "mail.example.com", certPEM, keyPEM)

	if _, err := Fetch("MAIL.Example.COM", []string{dataDir}); err != nil {
		t.Fatalf("域名大小写应被归一化，Fetch 失败: %v", err)
	}
}

func TestFetchCertificatesDirAsDataDir(t *testing.T) {
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dataDir, "mail.example.com", certPEM, keyPEM)

	// 把 certificates 目录本身当作 data_dir 传入
	certsDir := filepath.Join(dataDir, "certificates")
	if _, err := Fetch("mail.example.com", []string{certsDir}); err != nil {
		t.Fatalf("data_dir 直接指向 certificates 目录时应可用: %v", err)
	}
}

func TestFetchPrefersFirstDataDir(t *testing.T) {
	// 模拟“同步镜像目录优先”：两个目录都有该域名证书时，应返回第一个的
	dirA := t.TempDir()
	dirB := t.TempDir()
	certA, keyA := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	certB, keyB := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dirA, "mail.example.com", certA, keyA)
	writeSite(t, dirB, "mail.example.com", certB, keyB)

	got, err := Fetch("mail.example.com", []string{dirA, dirB})
	if err != nil {
		t.Fatalf("Fetch 失败: %v", err)
	}
	if string(got.CertPEM) != string(certA) {
		t.Error("应按优先级返回第一个目录中的证书")
	}
}

func TestFetchPermissionDeniedHint(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root 用户不受文件权限限制，跳过")
	}
	dataDir := t.TempDir()
	certPEM, keyPEM := makeCert(t, []string{"mail.example.com"}, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	writeSite(t, dataDir, "mail.example.com", certPEM, keyPEM)
	if err := os.Chmod(dataDir, 0000); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	defer os.Chmod(dataDir, 0700)

	_, err := Fetch("mail.example.com", []string{dataDir})
	if err == nil {
		t.Fatal("无权限时应返回错误")
	}
	if !strings.Contains(err.Error(), "权限不足") {
		t.Errorf("错误信息应提示权限不足，实际: %v", err)
	}
}
