package main

import (
	"fmt"
	"log"

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

	// 6. Start SMTP server
	smtpSrv := smtp_server.NewSMTPServer(cfg.SMTP, stores, attStorage)
	go func() {
		if err := smtpSrv.Start(); err != nil {
			log.Printf("SMTP 服务启动失败: %v", err)
		}
	}()
	// Start SMTPS if TLS is configured
	if cfg.SMTP.TLSCert != "" && cfg.SMTP.TLSKey != "" {
		go func() {
			if err := smtpSrv.StartTLS(); err != nil {
				log.Printf("SMTPS 服务启动失败: %v", err)
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
	webServer := web.NewWebServer(cfg.Web, stores, attStorage)
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
