package handlers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mail_go/internal/db"
)

// makeTestCertPair 生成一对自签名证书（PEM），可选 LF/CRLF 换行。
func makeTestCertPair(t *testing.T, lineEnding string) (certPEM, keyPEM string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"mail.example.com"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}

	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if lineEnding == "crlf" {
		certPEM = strings.ReplaceAll(certPEM, "\n", "\r\n")
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if lineEnding == "crlf" {
		keyPEM = strings.ReplaceAll(keyPEM, "\n", "\r\n")
	}
	return certPEM, keyPEM
}

// writeExistingCert 模拟已有证书文件（LF 换行）并返回 domain。
func writeExistingCert(t *testing.T, tlsDir string, certPEM, keyPEM string) *db.Domain {
	t.Helper()
	dir := filepath.Join(tlsDir, "1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, []byte(certPEM+"\n"), 0644); err != nil {
		t.Fatalf("写入证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte(keyPEM+"\n"), 0600); err != nil {
		t.Fatalf("写入私钥失败: %v", err)
	}
	return &db.Domain{
		ID:          1,
		Name:        "mail.example.com",
		TlsEnabled:  true,
		TlsCertPath: certPath,
		TlsKeyPath:  keyPath,
	}
}

// TestHandleDomainTLSUpdateUnchangedCertCRLF 复现用户报告的 bug：
// 浏览器把 textarea 的 LF 换成 CRLF 提交，私钥留空（保留现有私钥），
// 证书内容未变，此时保存不应报“必须同时填写”。
func TestHandleDomainTLSUpdateUnchangedCertCRLF(t *testing.T) {
	certLF, keyLF := makeTestCertPair(t, "lf")
	h := &AdminHandler{tlsDir: t.TempDir()}
	domain := writeExistingCert(t, h.tlsDir, certLF, keyLF)

	submittedCert := strings.ReplaceAll(certLF, "\n", "\r\n") // 模拟浏览器提交
	if err := h.handleDomainTLSUpdate("zh", domain, submittedCert, ""); err != nil {
		t.Fatalf("证书未修改且私钥留空时应保留现有私钥，实际报错: %v", err)
	}
}

// TestHandleDomainTLSUpdateNewPairCRLF 新证书+私钥（CRLF 提交）应正常保存，
// 且写入文件为 LF 换行、能组成有效密钥对。
func TestHandleDomainTLSUpdateNewPairCRLF(t *testing.T) {
	certCRLF, keyCRLF := makeTestCertPair(t, "crlf")
	h := &AdminHandler{tlsDir: t.TempDir()}
	domain := &db.Domain{ID: 1, Name: "mail.example.com", TlsEnabled: true}

	if err := h.handleDomainTLSUpdate("zh", domain, certCRLF, keyCRLF); err != nil {
		t.Fatalf("CRLF 提交的新证书应保存成功，实际报错: %v", err)
	}

	data, err := os.ReadFile(domain.TlsCertPath)
	if err != nil {
		t.Fatalf("读取保存的证书失败: %v", err)
	}
	if strings.Contains(string(data), "\r") {
		t.Error("保存的证书文件不应包含 CR")
	}
	if _, err := tls.LoadX509KeyPair(domain.TlsCertPath, domain.TlsKeyPath); err != nil {
		t.Fatalf("保存的证书对无效: %v", err)
	}
}

// TestHandleDomainTLSUpdateChangedCertWithoutKey 证书确实被修改但私钥留空，
// 应报“必须同时填写”（防止用不匹配的私钥）。
func TestHandleDomainTLSUpdateChangedCertWithoutKey(t *testing.T) {
	certA, keyA := makeTestCertPair(t, "lf")
	certB, _ := makeTestCertPair(t, "lf")
	h := &AdminHandler{tlsDir: t.TempDir()}
	domain := writeExistingCert(t, h.tlsDir, certA, keyA)

	err := h.handleDomainTLSUpdate("zh", domain, certB, "")
	if err == nil || !strings.Contains(err.Error(), "必须同时填写") {
		t.Fatalf("修改证书但私钥留空应报“必须同时填写”，实际: %v", err)
	}
}
