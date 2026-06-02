package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/imap_server"
	"mail_go/internal/pop3_server"
	"mail_go/internal/smtp_server"
	"mail_go/internal/storage"
	"mail_go/internal/store"
	"mail_go/internal/web"

	"golang.org/x/crypto/bcrypt"
)

func applyDomainTLSConfig(stores *store.Stores, cfg *config.Config) {
	domain, err := stores.Domains.GetFirstTLSEnabledWithCert()
	if err != nil {
		return
	}

	applied := applyTLSCertPaths(cfg, domain.TlsCertPath, domain.TlsKeyPath)
	if applied {
		log.Printf("使用域名 %s 的 TLS 证书；更新证书后需重启服务生效", domain.Name)
	}
}

func applyTLSCertPaths(cfg *config.Config, certPath, keyPath string) bool {
	applied := false
	if cfg.SMTP.TLSCert == "" && cfg.SMTP.TLSKey == "" {
		cfg.SMTP.TLSCert = certPath
		cfg.SMTP.TLSKey = keyPath
		applied = true
	}
	if cfg.IMAP.TLSCert == "" && cfg.IMAP.TLSKey == "" {
		cfg.IMAP.TLSCert = certPath
		cfg.IMAP.TLSKey = keyPath
		applied = true
	}
	if cfg.POP3.TLSCert == "" && cfg.POP3.TLSKey == "" {
		cfg.POP3.TLSCert = certPath
		cfg.POP3.TLSKey = keyPath
		applied = true
	}
	return applied
}

func ensureSelfSignedTLSConfig(cfg *config.Config) {
	if cfg.SMTP.TLSCert != "" && cfg.SMTP.TLSKey != "" && cfg.IMAP.TLSCert != "" && cfg.IMAP.TLSKey != "" && cfg.POP3.TLSCert != "" && cfg.POP3.TLSKey != "" {
		return
	}

	certPath := filepath.Join(cfg.Storage.BaseDir, "tls", "self-signed", "cert.pem")
	keyPath := filepath.Join(cfg.Storage.BaseDir, "tls", "self-signed", "key.pem")
	if err := ensureSelfSignedCert(certPath, keyPath, cfg.SMTP.Domain); err != nil {
		log.Printf("生成自签名 TLS 证书失败: %v", err)
		return
	}

	if applyTLSCertPaths(cfg, certPath, keyPath) {
		log.Printf("未配置 TLS 证书，已使用自签名证书启动 TLS 端口；正式使用请在后台上传受信任证书")
	}
}

func ensureSelfSignedCert(certPath, keyPath, domain string) error {
	if _, certErr := os.Stat(certPath); certErr == nil {
		if _, keyErr := os.Stat(keyPath); keyErr == nil {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		return err
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	notBefore := time.Now()
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return err
	}

	if domain == "" {
		domain = "localhost"
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"MailGo Self-Signed"},
			CommonName:   domain,
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{domain, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return err
	}

	certFile, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		certFile.Close()
		return err
	}
	if err := certFile.Close(); err != nil {
		return err
	}

	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyFile, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}); err != nil {
		keyFile.Close()
		return err
	}
	return keyFile.Close()
}

func main() {
	// 1. Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	fmt.Println("配置加载成功")

	// 2. Initialize database
	database, err := db.InitDB(cfg.Database, cfg.Storage)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	fmt.Println("数据库初始化成功")

	// 3. Create Store layer
	stores := store.NewStores(database)

	// 4. Ensure default admin user exists
	ensureAdminUser(stores, cfg)

	// 5. Initialize attachment storage
	attStorage := storage.NewAttachmentStorage(cfg.Storage.AttachDir)
	applyDomainTLSConfig(stores, cfg)
	ensureSelfSignedTLSConfig(cfg)

	// 6. Start SMTP server
	smtpSrv := smtp_server.NewSMTPServer(cfg.SMTP, stores, attStorage)
	go func() {
		if err := smtpSrv.Start(); err != nil {
			log.Printf("SMTP 服务启动失败: %v", err)
		}
	}()
	// Start SMTPS and submission if TLS is configured
	if cfg.SMTP.TLSCert != "" && cfg.SMTP.TLSKey != "" {
		go func() {
			if err := smtpSrv.StartTLS(); err != nil {
				log.Printf("SMTPS 服务启动失败: %v", err)
			}
		}()
		go func() {
			if err := smtpSrv.StartSubmission(); err != nil {
				log.Printf("SMTP Submission 服务启动失败: %v", err)
			}
		}()
	}

	// 7. Start IMAP server
	imapSrv := imap_server.NewIMAPServer(cfg.IMAP, stores)
	go func() {
		if err := imapSrv.Start(); err != nil {
			log.Printf("IMAP 服务启动失败: %v", err)
		}
	}()
	// Start IMAPS if TLS is configured
	if cfg.IMAP.TLSCert != "" && cfg.IMAP.TLSKey != "" {
		go func() {
			if err := imapSrv.StartTLS(); err != nil {
				log.Printf("IMAPS 服务启动失败: %v", err)
			}
		}()
	}

	// 8. Start POP3 server
	pop3Srv := pop3_server.NewPOP3Server(cfg.POP3, stores)
	go func() {
		if err := pop3Srv.Start(); err != nil {
			log.Printf("POP3 服务启动失败: %v", err)
		}
	}()
	// Start POP3S if TLS is configured
	if cfg.POP3.TLSCert != "" && cfg.POP3.TLSKey != "" {
		go func() {
			if err := pop3Srv.StartTLS(); err != nil {
				log.Printf("POP3S 服务启动失败: %v", err)
			}
		}()
	}

	// 9. Start Web server
	webServer := web.NewWebServer(cfg.Web, stores, attStorage, cfg.Storage, cfg.Auth, cfg.Ban)
	fmt.Printf("Web 服务启动在 %s\n", cfg.Web.Addr)
	go func() {
		if err := webServer.Start(); err != nil {
			log.Fatalf("Web 服务启动失败: %v", err)
		}
	}()

	fmt.Println("MailGo 邮件系统启动完成")
	select {} // Block main goroutine
}

// ensureAdminUser checks if an admin user exists and creates one if not.
// It also ensures the default domain "example.com" exists.
func ensureAdminUser(stores *store.Stores, cfg *config.Config) {
	// Check if admin user exists by trying to authenticate
	_, err := stores.Users.GetByEmail("admin@example.com")
	if err == nil {
		fmt.Println("管理员账户已存在，跳过创建")
		return
	}

	// Ensure the default domain exists
	domain, err := stores.Domains.GetByName("example.com")
	if err != nil {
		// Domain doesn't exist, create it
		domain = &db.Domain{
			Name:       "example.com",
			SmtpPort:   25,
			ImapPort:   143,
			Pop3Port:   110,
			TlsEnabled: false,
		}
		if createErr := stores.Domains.Create(domain); createErr != nil {
			log.Printf("创建默认域名失败: %v", createErr)
			return
		}
		fmt.Println("默认域名 example.com 创建成功")
	}

	// Hash the default admin password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("密码哈希失败: %v", err)
		return
	}

	// Create the admin user
	adminUser := &db.User{
		Username:     "admin",
		PasswordHash: string(hashedPassword),
		DomainID:     domain.ID,
		QuotaBytes:   5 * 1024 * 1024 * 1024, // 5GB
		UsedBytes:    0,
		IsActive:     true,
		IsAdmin:      true,
	}

	if createErr := stores.Users.Create(adminUser); createErr != nil {
		log.Printf("创建管理员账户失败: %v", createErr)
		return
	}

	fmt.Println("管理员账户 admin@example.com 创建成功（密码: admin）")
}
