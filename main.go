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
	"sync"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/imap_server"
	"mail_go/internal/outbound"
	"mail_go/internal/pop3_server"
	"mail_go/internal/smtp_server"
	"mail_go/internal/storage"
	"mail_go/internal/store"
	"mail_go/internal/tlsutil"
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
		log.Printf("使用域名 %s 的 TLS 证书；证书更新后自动热加载，无需重启服务", domain.Name)
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

// tlsSource 返回证书路径来源：协议在 toml 中显式配置的证书优先；
// 否则取第一个启用 TLS 且有证书的域名（管理后台一键导入证书后自动
// 切换，无需重启）。结果缓存 10 秒，避免每次握手都查询数据库。
func tlsSource(explicitCert, explicitKey string, stores *store.Stores) tlsutil.Source {
	var (
		mu         sync.Mutex
		lastCheck  time.Time
		cachedCert string
		cachedKey  string
	)
	return func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(lastCheck) < 10*time.Second {
			return cachedCert, cachedKey
		}
		lastCheck = time.Now()
		if explicitCert != "" && explicitKey != "" {
			cachedCert, cachedKey = explicitCert, explicitKey
		} else if d, err := stores.Domains.GetFirstTLSEnabledWithCert(); err == nil {
			cachedCert, cachedKey = d.TlsCertPath, d.TlsKeyPath
		} else {
			cachedCert, cachedKey = "", ""
		}
		return cachedCert, cachedKey
	}
}

// newTLSCertLoader 创建带热加载的 TLS 证书加载器（每次握手自动重载）。
// 初始路径取显式配置或启动时填充的路径；source 允许后续动态切换
// 证书来源。加载失败返回 nil，对应协议将不启用 TLS。
func newTLSCertLoader(explicitCert, explicitKey, initCert, initKey string, stores *store.Stores, proto string) *tlsutil.Loader {
	if initCert == "" || initKey == "" {
		initCert, initKey = explicitCert, explicitKey
	}
	loader, err := tlsutil.NewLoader(initCert, initKey, tlsSource(explicitCert, explicitKey, stores), log.Printf)
	if err != nil {
		log.Printf("%s TLS 证书初始化失败: %v（该协议将不启用 TLS）", proto, err)
		return nil
	}
	return loader
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

	// 记录 toml 中显式配置的证书路径；此后 applyDomainTLSConfig 会用
	// 域名证书填充空值，需要原始值来判断“显式配置优先”。
	explicitSMTPCert, explicitSMTPKey := cfg.SMTP.TLSCert, cfg.SMTP.TLSKey
	explicitIMAPCert, explicitIMAPKey := cfg.IMAP.TLSCert, cfg.IMAP.TLSKey
	explicitPOP3Cert, explicitPOP3Key := cfg.POP3.TLSCert, cfg.POP3.TLSKey

	applyDomainTLSConfig(stores, cfg)
	ensureSelfSignedTLSConfig(cfg)

	// 证书热加载器：每次 TLS 握手自动重载证书文件，证书更新后无需重启
	smtpTLS := newTLSCertLoader(explicitSMTPCert, explicitSMTPKey, cfg.SMTP.TLSCert, cfg.SMTP.TLSKey, stores, "SMTP")
	imapTLS := newTLSCertLoader(explicitIMAPCert, explicitIMAPKey, cfg.IMAP.TLSCert, cfg.IMAP.TLSKey, stores, "IMAP")
	pop3TLS := newTLSCertLoader(explicitPOP3Cert, explicitPOP3Key, cfg.POP3.TLSCert, cfg.POP3.TLSKey, stores, "POP3")

	// 6. Outbound delivery manager (external mail queue + worker)
	outboundMgr := outbound.NewManager(cfg.Outbound, cfg.SMTP.Domain, stores)
	if outboundMgr.Enabled() {
		outboundMgr.Start()
		fmt.Println("外发邮件投递服务已启动")
	} else {
		fmt.Println("外发邮件投递未启用（outbound.max_per_day = 0）")
	}

	// 7. Start SMTP server
	smtpSrv := smtp_server.NewSMTPServer(cfg.SMTP, stores, attStorage, outboundMgr, smtpTLS, cfg.Ban)
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
	imapSrv := imap_server.NewIMAPServer(cfg.IMAP, stores, imapTLS, cfg.Ban)
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
	pop3Srv := pop3_server.NewPOP3Server(cfg.POP3, stores, pop3TLS, cfg.Ban)
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

	// 10. Start Web server
	webServer, err := web.NewWebServer(cfg.Web, stores, attStorage, cfg.Storage, cfg.Auth, cfg.Ban, cfg.Caddy, outboundMgr)
	if err != nil {
		log.Fatalf("Web 服务初始化失败: %v", err)
	}
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

	// 初始密码：优先取环境变量 MAILGO_ADMIN_PASSWORD；
	// 否则生成随机密码并打印一次（只能在本机启动日志中看到）。
	// 无论哪种方式都会标记首次登录必须改密，杜绝默认口令。
	adminPassword := os.Getenv("MAILGO_ADMIN_PASSWORD")
	generated := false
	if adminPassword == "" {
		adminPassword = randomPassword()
		generated = true
	}

	// Hash the admin password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(adminPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("密码哈希失败: %v", err)
		return
	}

	// Create the admin user
	adminUser := &db.User{
		Username:             "admin",
		PasswordHash:         string(hashedPassword),
		DomainID:             domain.ID,
		QuotaBytes:           5 * 1024 * 1024 * 1024, // 5GB
		UsedBytes:            0,
		IsActive:             true,
		IsAdmin:              true,
		MustChangePassword:   true,
	}

	if createErr := stores.Users.Create(adminUser); createErr != nil {
		log.Printf("创建管理员账户失败: %v", createErr)
		return
	}

	if generated {
		fmt.Printf("管理员账户 admin@example.com 创建成功，初始密码: %s\n", adminPassword)
	} else {
		fmt.Println("管理员账户 admin@example.com 创建成功（密码来自 MAILGO_ADMIN_PASSWORD）")
	}
	fmt.Println("安全提示：该账户已被标记为“首次登录必须修改密码”，请登录后立即在 设置 页面修改。")
}

// randomPassword 生成 16 位随机密码（数字+大小写字母），用于初始管理员账户。
func randomPassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见；退化为时间种子以避免空密码
		log.Printf("生成随机密码失败: %v，使用弱随机回退", err)
		n := time.Now().UnixNano()
		for i := range buf {
			buf[i] = charset[(n>>(uint(i)*4))%int64(len(charset))]
		}
		return string(buf)
	}
	for i := range buf {
		buf[i] = charset[int(buf[i])%len(charset)]
	}
	return string(buf)
}
