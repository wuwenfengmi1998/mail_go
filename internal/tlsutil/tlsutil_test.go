package tlsutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeCertPair 生成一对自签名证书并写入文件，返回叶子证书序列号。
func writeCertPair(t *testing.T, certPath, keyPath string, serial int64) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"localhost"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成证书失败: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatalf("写入证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatalf("写入私钥失败: %v", err)
	}
}

func leafSerial(t *testing.T, cert *x509.Certificate) *big.Int {
	t.Helper()
	return cert.SerialNumber
}

func TestNewLoaderFailsFast(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	os.WriteFile(certPath, []byte("garbage"), 0600)
	os.WriteFile(keyPath, []byte("garbage"), 0600)

	if _, err := NewLoader(certPath, keyPath, nil, nil); err == nil {
		t.Fatal("无效证书应返回错误")
	}
}

func TestReloadOnFileChange(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	writeCertPair(t, certPath, keyPath, 1)
	l, err := NewLoader(certPath, keyPath, nil, nil)
	if err != nil {
		t.Fatalf("NewLoader 失败: %v", err)
	}

	c1, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate 失败: %v", err)
	}
	if c1.Leaf == nil {
		if parsed, err := x509.ParseCertificate(c1.Certificate[0]); err == nil {
			c1.Leaf = parsed
		}
	}
	if leafSerial(t, c1.Leaf).Int64() != 1 {
		t.Fatalf("初始证书序列号应为 1")
	}

	// 替换文件内容（模拟证书更新），无需重启
	time.Sleep(10 * time.Millisecond) // 确保 mtime 变化
	writeCertPair(t, certPath, keyPath, 2)

	c2, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatalf("更新后 GetCertificate 失败: %v", err)
	}
	if parsed, err := x509.ParseCertificate(c2.Certificate[0]); err == nil {
		c2.Leaf = parsed
	}
	if leafSerial(t, c2.Leaf).Int64() != 2 {
		t.Fatal("文件更新后应自动加载新证书（序列号 2）")
	}
}

func TestStaleCertOnInvalidReload(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	writeCertPair(t, certPath, keyPath, 1)
	l, err := NewLoader(certPath, keyPath, nil, nil)
	if err != nil {
		t.Fatalf("NewLoader 失败: %v", err)
	}

	// 写入损坏的证书：重载失败时应继续使用旧证书
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(certPath, []byte("broken"), 0600)

	c, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatalf("重载失败时不应返回错误: %v", err)
	}
	if parsed, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
		c.Leaf = parsed
	}
	if leafSerial(t, c.Leaf).Int64() != 1 {
		t.Fatal("重载失败时应继续使用旧证书")
	}
}

func TestSourcePathSwitch(t *testing.T) {
	dir := t.TempDir()
	certA := filepath.Join(dir, "a", "cert.pem")
	keyA := filepath.Join(dir, "a", "key.pem")
	certB := filepath.Join(dir, "b", "cert.pem")
	keyB := filepath.Join(dir, "b", "key.pem")
	writeCertPair(t, certA, keyA, 1)
	writeCertPair(t, certB, keyB, 2)

	// 初始用 A，source 后续切换到 B（模拟后台导入新域名证书）
	source := func() (string, string) { return certB, keyB }
	l, err := NewLoader(certA, keyA, source, nil)
	if err != nil {
		t.Fatalf("NewLoader 失败: %v", err)
	}

	c, err := l.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate 失败: %v", err)
	}
	if parsed, err := x509.ParseCertificate(c.Certificate[0]); err == nil {
		c.Leaf = parsed
	}
	if leafSerial(t, c.Leaf).Int64() != 2 {
		t.Fatal("source 切换路径后应自动加载新证书")
	}
}

func TestRapidSuccessiveChanges(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	writeCertPair(t, certPath, keyPath, 1)
	l, err := NewLoader(certPath, keyPath, nil, nil)
	if err != nil {
		t.Fatalf("NewLoader 失败: %v", err)
	}

	serialOf := func() int64 {
		t.Helper()
		c, err := l.GetCertificate(nil)
		if err != nil {
			t.Fatalf("GetCertificate 失败: %v", err)
		}
		parsed, err := x509.ParseCertificate(c.Certificate[0])
		if err != nil {
			t.Fatalf("解析证书失败: %v", err)
		}
		return parsed.SerialNumber.Int64()
	}

	// 5 秒内连续两次更新，两次都应立即生效（节流只针对失败重载）
	time.Sleep(10 * time.Millisecond)
	writeCertPair(t, certPath, keyPath, 2)
	if got := serialOf(); got != 2 {
		t.Fatalf("第一次更新后应加载序列号 2，实际 %d", got)
	}

	time.Sleep(10 * time.Millisecond)
	writeCertPair(t, certPath, keyPath, 3)
	if got := serialOf(); got != 3 {
		t.Fatalf("第二次快速更新后应立即加载序列号 3，实际 %d", got)
	}
}

func TestConcurrentGetCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	writeCertPair(t, certPath, keyPath, 1)

	l, err := NewLoader(certPath, keyPath, nil, nil)
	if err != nil {
		t.Fatalf("NewLoader 失败: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 并发读
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := l.GetCertificate(nil); err != nil {
						t.Errorf("并发 GetCertificate 失败: %v", err)
						return
					}
				}
			}
		}()
	}
	// 同时反复替换证书文件（模拟续期）
	for i := int64(2); i < 6; i++ {
		writeCertPair(t, certPath, keyPath, i)
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}
