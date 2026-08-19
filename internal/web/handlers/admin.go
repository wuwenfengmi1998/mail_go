package handlers

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mail_go/internal/caddycert"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/dkim"
	"mail_go/internal/outbound"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// AdminHandler handles admin-related routes (dashboard, domain/user management).
type AdminHandler struct {
	stores       *store.Stores
	storage      *storage.AttachmentStorage
	tlsDir       string
	caddyDataDir string
	outbound     *outbound.Manager
	// protocolLogKeepDays SMTP/IMAP/POP3 协议日志保留天数（配置文件 [web]）
	protocolLogKeepDays int
	// hub 当前协议连接注册中心（「当前连接」页）
	hub *connhub.Hub
}

// NewAdminHandler creates a new AdminHandler with the given stores, attachment
// storage, TLS directory, Caddy data directory and outbound delivery manager.
func NewAdminHandler(stores *store.Stores, attStorage *storage.AttachmentStorage, tlsDir string, caddyDataDir string, ob *outbound.Manager, protocolLogKeepDays int, hub *connhub.Hub) *AdminHandler {
	return &AdminHandler{stores: stores, storage: attStorage, tlsDir: tlsDir, caddyDataDir: caddyDataDir, outbound: ob, protocolLogKeepDays: protocolLogKeepDays, hub: hub}
}

// manualBanDuration 管理员手动封禁时长（180 天，与自动封禁档位上限制一致）。
const manualBanDuration = 180 * 24 * time.Hour

// DisconnectConnection 强制断开指定连接并封禁其 IP（管理后台「断开并封禁」）。
// 封禁后该 IP 的所有在线连接一并断开。
func (h *AdminHandler) DisconnectConnection(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的连接ID")
		return
	}

	conn, ok := h.hub.Get(id)
	if !ok {
		c.String(http.StatusNotFound, "连接不存在或已断开")
		return
	}

	// 加入黑名单：180 天封禁（管理员可随时解封）
	if err := h.stores.Bans.Create(&db.BanEntry{
		IPAddress: conn.IP,
		Reason:    "管理员手动封禁（连接断开）",
		FailCount: 0,
		BanCount:  0,
		ExpiresAt: time.Now().Add(manualBanDuration),
	}); err != nil {
		c.String(http.StatusInternalServerError, "封禁失败: %v", err)
		return
	}

	// 断开该 IP 的全部连接（含本连接与其他协议连接）
	n := h.hub.DisconnectByIP(conn.IP)
	log.Printf("admin: 已封禁并断开 IP %s 的 %d 个连接", conn.IP, n)
	c.Redirect(http.StatusFound, "/admin/connections")
}

// ListConnections 渲染当前协议连接页面（SMTP/IMAP/POP3 实时连接）。
func (h *AdminHandler) ListConnections(c *gin.Context) {
	conns := h.hub.List()
	counts := h.hub.Counts()

	total := len(conns)
	smtpCount := counts["smtp"]
	imapCount := counts["imap"]
	pop3Count := counts["pop3"]

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_connections", gin.H{
		"currentUser":  currentUser,
		"conns":        conns,
		"total":        total,
		"smtpCount":    smtpCount,
		"imapCount":    imapCount,
		"pop3Count":    pop3Count,
		"now":          time.Now(),
		"activeFolder": "connections",
	})
}

// Dashboard renders the admin dashboard with summary statistics.
func (h *AdminHandler) Dashboard(c *gin.Context) {
	_, domainCount, _ := h.stores.Domains.List(1, 1)
	_, userCount, _ := h.stores.Users.ListAll(1, 1)

	// Mail statistics
	totalMails, _ := h.stores.Mails.CountAll()
	inboxCount, _ := h.stores.Mails.CountByFolder("INBOX")
	sentCount, _ := h.stores.Mails.CountByFolder("Sent")
	draftsCount, _ := h.stores.Mails.CountByFolder("Drafts")
	trashCount, _ := h.stores.Mails.CountByFolder("Trash")

	totalSize, _ := h.stores.Mails.TotalSize()
	inboxSize, _ := h.stores.Mails.TotalSizeByFolder("INBOX")
	sentSize, _ := h.stores.Mails.TotalSizeByFolder("Sent")

	// Today and weekly statistics
	todayStart := time.Now().Truncate(24 * time.Hour)
	weekStart := time.Now().AddDate(0, 0, -7)

	todayReceived, _ := h.stores.Mails.CountByFolderSince("INBOX", todayStart)
	todaySent, _ := h.stores.Mails.CountByFolderSince("Sent", todayStart)
	weekReceived, _ := h.stores.Mails.CountByFolderSince("INBOX", weekStart)
	weekSent, _ := h.stores.Mails.CountByFolderSince("Sent", weekStart)

	// Ban count: number of currently banned IPs
	bans, _, _ := h.stores.Bans.List(1, 1000)
	var banCount int64
	for _, b := range bans {
		if b.ExpiresAt.After(time.Now()) {
			banCount++
		}
	}

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_dashboard", gin.H{
		"currentUser":   currentUser,
		"domainCount":   domainCount,
		"userCount":     userCount,
		"totalMails":    totalMails,
		"inboxCount":    inboxCount,
		"sentCount":     sentCount,
		"draftsCount":   draftsCount,
		"trashCount":    trashCount,
		"totalSize":     totalSize,
		"inboxSize":     inboxSize,
		"sentSize":      sentSize,
		"todayReceived": todayReceived,
		"todaySent":     todaySent,
		"weekReceived":  weekReceived,
		"weekSent":      weekSent,
		"banCount":      banCount,
		"activeFolder":  "admin",
	})
}

// ListDomains renders the domain list page.
func (h *AdminHandler) ListDomains(c *gin.Context) {
	page := getPageParam(c, "page", 1)
	domains, total, err := h.stores.Domains.List(page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载域名列表失败: %v", err)
		return
	}

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "admin_domains", gin.H{
		"currentUser":  currentUser,
		"domains":      domains,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"activeFolder": "domains",
	})
}

// NewDomain renders the new domain form page.
func (h *AdminHandler) NewDomain(c *gin.Context) {
	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_domain_form", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "domains",
		"error":        "",
		"isEdit":       false,
		"domain":       &db.Domain{},
	})
}

// CreateDomain processes the new domain form submission.
func (h *AdminHandler) CreateDomain(c *gin.Context) {
	name := c.PostForm("name")
	smtpPort := formIntOrDefault(c, "smtp_port", 25)
	imapPort := formIntOrDefault(c, "imap_port", 143)
	pop3Port := formIntOrDefault(c, "pop3_port", 110)
	tlsEnabled := c.PostForm("tls_enabled") == "on"

	if name == "" {
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_domain_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "domains",
			"error":        "请输入域名",
			"isEdit":       false,
			"domain": &db.Domain{
				Name:       name,
				SmtpPort:   smtpPort,
				ImapPort:   imapPort,
				Pop3Port:   pop3Port,
				TlsEnabled: tlsEnabled,
			},
		})
		return
	}

	domain := &db.Domain{
		Name:       name,
		SmtpPort:   smtpPort,
		ImapPort:   imapPort,
		Pop3Port:   pop3Port,
		TlsEnabled: tlsEnabled,
	}

	// 自动生成 DKIM 密钥对
	privKey, pubKey, err := dkim.GenerateKeyPair()
	if err != nil {
		log.Printf("DKIM密钥生成失败: %v", err)
	} else {
		domain.DkimSelector = "default"
		domain.DkimPrivateKey = privKey
		domain.DkimPublicKey = pubKey
	}

	if err := h.stores.Domains.Create(domain); err != nil {
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_domain_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "domains",
			"error":        fmt.Sprintf("创建域名失败: %v", err),
			"isEdit":       false,
			"domain":       domain,
		})
		return
	}

	c.Redirect(http.StatusFound, "/admin/domains")
}

// EditDomain 渲染编辑域名表单
func (h *AdminHandler) EditDomain(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的域名ID")
		return
	}

	domain, err := h.stores.Domains.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "域名不存在")
		return
	}

	currentUser, _ := c.Get("currentUser")

	caddyMsg, caddyMsgType := "", ""
	if c.Query("caddy_err") != "" {
		caddyMsg = c.Query("caddy_err")
		caddyMsgType = "error"
	} else if c.Query("caddy_ok") == "1" {
		caddyMsg = "✅ 已从 Caddy 获取证书并保存到域名 TLS 目录，同时已启用该域名的 TLS；证书已热加载，无需重启服务。"
		caddyMsgType = "success"
	}

	c.HTML(200, "admin_domain_form", gin.H{
		"currentUser":       currentUser,
		"activeFolder":      "domains",
		"error":             "",
		"isEdit":            true,
		"domain":            domain,
		"tlsPublicCert":     readTLSCert(domain.TlsCertPath),
		"tlsCertConfigured": domain.TlsCertPath != "" && domain.TlsKeyPath != "",
		"caddyMsg":          caddyMsg,
		"caddyMsgType":      caddyMsgType,
	})
}

// UpdateDomain 处理编辑域名表单提交
func (h *AdminHandler) UpdateDomain(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的域名ID")
		return
	}

	domain, err := h.stores.Domains.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "域名不存在")
		return
	}

	domain.SmtpPort = formIntOrDefault(c, "smtp_port", domain.SmtpPort)
	domain.ImapPort = formIntOrDefault(c, "imap_port", domain.ImapPort)
	domain.Pop3Port = formIntOrDefault(c, "pop3_port", domain.Pop3Port)
	domain.TlsEnabled = c.PostForm("tls_enabled") == "on"

	tlsPrivateKey := strings.TrimSpace(c.PostForm("tls_private_key"))
	tlsPublicCert := strings.TrimSpace(c.PostForm("tls_public_cert"))
	if err := h.handleDomainTLSUpdate(domain, tlsPublicCert, tlsPrivateKey); err != nil {
		h.renderDomainFormError(c, domain, err.Error(), tlsPublicCert)
		return
	}

	// 重新生成DKIM
	if c.PostForm("regenerate_dkim") == "on" {
		privKey, pubKey, err := dkim.GenerateKeyPair()
		if err == nil {
			domain.DkimPrivateKey = privKey
			domain.DkimPublicKey = pubKey
			domain.DkimSelector = "default"
		}
	}

	if err := h.stores.Domains.Update(domain); err != nil {
		h.renderDomainFormError(c, domain, fmt.Sprintf("更新域名失败: %v", err), tlsPublicCert)
		return
	}

	c.Redirect(http.StatusFound, "/admin/domains")
}

// FetchCaddyCert 尝试从本机 Caddy 的证书存储中获取该域名的证书与私钥，
// 保存到 MailGo 的域名 TLS 目录并更新数据库记录。结果通过查询参数回显到
// 编辑页面（caddy_ok=1 成功 / caddy_err=<消息> 失败）。
func (h *AdminHandler) FetchCaddyCert(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的域名ID")
		return
	}

	domain, err := h.stores.Domains.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "域名不存在")
		return
	}

	editURL := fmt.Sprintf("/admin/domains/%d/edit", domain.ID)
	fail := func(msg string) {
		log.Printf("从 Caddy 获取证书失败 domain=%s: %s", domain.Name, msg)
		c.Redirect(http.StatusFound, editURL+"?caddy_err="+url.QueryEscape(msg))
	}

	cert, err := caddycert.Fetch(domain.Name, h.caddyCertRoots())
	if err != nil {
		fail(fmt.Sprintf("从 Caddy 获取证书失败: %v", err))
		return
	}

	// 保存到域名 TLS 目录（与手动上传证书的位置一致）
	domainTLSDir := filepath.Join(h.tlsDir, strconv.FormatUint(uint64(domain.ID), 10))
	if err := os.MkdirAll(domainTLSDir, 0700); err != nil {
		fail(fmt.Sprintf("创建 TLS 证书目录失败: %v", err))
		return
	}

	certPath := filepath.Join(domainTLSDir, "cert.pem")
	keyPath := filepath.Join(domainTLSDir, "key.pem")
	if err := os.WriteFile(certPath, cert.CertPEM, 0644); err != nil {
		fail(fmt.Sprintf("保存 TLS 公钥证书失败: %v", err))
		return
	}
	if err := os.WriteFile(keyPath, cert.KeyPEM, 0600); err != nil {
		fail(fmt.Sprintf("保存 TLS 私钥失败: %v", err))
		return
	}

	domain.TlsCertPath = certPath
	domain.TlsKeyPath = keyPath
	domain.TlsEnabled = true
	if err := h.stores.Domains.Update(domain); err != nil {
		fail(fmt.Sprintf("更新域名记录失败: %v", err))
		return
	}

	log.Printf("已从 Caddy 导入域名 %s 的证书 (%s)，热加载生效", domain.Name, cert.Source)
	c.Redirect(http.StatusFound, editURL+"?caddy_ok=1")
}

// caddyCertRoots 返回按优先级排列的证书来源目录：
//  1. MailGo 的同步镜像目录 <storage>/tls/caddy —— 由 install.sh 安装的
//     systemd path 同步任务以 root 权限从 Caddy 证书存储镜像而来，
//     mail_go 始终可读，证书续期后自动更新；
//  2. 配置文件 caddy.data_dir 指定的目录（可选）；
//
// 其余默认位置由 caddycert.Fetch 自行探测。
func (h *AdminHandler) caddyCertRoots() []string {
	return []string{
		filepath.Join(filepath.Dir(h.tlsDir), "caddy"),
		h.caddyDataDir,
	}
}

func readTLSCert(path string) string {
	if path == "" {
		return ""
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// normalizePEM 统一 PEM 文本的换行为 LF：浏览器提交 textarea 时会把
// 换行规范为 CRLF，而证书文件里通常是 LF，直接比较会误判“证书已修改”
// （表现为：私钥留空保留现有私钥时仍报“必须同时填写”）。
func normalizePEM(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

func (h *AdminHandler) handleDomainTLSUpdate(domain *db.Domain, publicCert, privateKey string) error {
	// 归一化换行，保证与磁盘文件一致，避免表单往返时被误判为已修改
	publicCert = normalizePEM(strings.TrimSpace(publicCert))
	privateKey = normalizePEM(strings.TrimSpace(privateKey))

	if !domain.TlsEnabled {
		return nil
	}

	hasExistingCert := domain.TlsCertPath != "" && domain.TlsKeyPath != ""
	if publicCert == "" && privateKey == "" {
		if hasExistingCert {
			return nil
		}
		return fmt.Errorf("启用 TLS 时必须填写 TLS 私钥和公钥证书")
	}
	if hasExistingCert && privateKey == "" && normalizePEM(strings.TrimSpace(readTLSCert(domain.TlsCertPath))) == publicCert {
		return nil
	}
	if publicCert == "" || privateKey == "" {
		return fmt.Errorf("TLS 私钥和公钥证书必须同时填写")
	}

	if _, err := tls.X509KeyPair([]byte(publicCert), []byte(privateKey)); err != nil {
		return fmt.Errorf("TLS 证书或私钥无效: %v", err)
	}

	domainTLSDir := filepath.Join(h.tlsDir, strconv.FormatUint(uint64(domain.ID), 10))
	if err := os.MkdirAll(domainTLSDir, 0700); err != nil {
		return fmt.Errorf("创建 TLS 证书目录失败: %v", err)
	}

	certPath := filepath.Join(domainTLSDir, "cert.pem")
	keyPath := filepath.Join(domainTLSDir, "key.pem")
	if err := os.WriteFile(certPath, []byte(publicCert+"\n"), 0644); err != nil {
		return fmt.Errorf("保存 TLS 公钥证书失败: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte(privateKey+"\n"), 0600); err != nil {
		return fmt.Errorf("保存 TLS 私钥失败: %v", err)
	}

	domain.TlsCertPath = certPath
	domain.TlsKeyPath = keyPath
	return nil
}

func (h *AdminHandler) renderDomainFormError(c *gin.Context, domain *db.Domain, message, tlsPublicCert string) {
	currentUser, _ := c.Get("currentUser")
	if tlsPublicCert == "" {
		tlsPublicCert = readTLSCert(domain.TlsCertPath)
	}

	c.HTML(http.StatusBadRequest, "admin_domain_form", gin.H{
		"currentUser":       currentUser,
		"activeFolder":      "domains",
		"error":             message,
		"isEdit":            true,
		"domain":            domain,
		"tlsPublicCert":     tlsPublicCert,
		"tlsCertConfigured": domain.TlsCertPath != "" && domain.TlsKeyPath != "",
	})
}

// DeleteDomain removes a domain by ID.
func (h *AdminHandler) DeleteDomain(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的域名ID")
		return
	}

	if err := h.stores.Domains.Delete(uint(id)); err != nil {
		c.String(http.StatusInternalServerError, "删除域名失败: %v", err)
		return
	}

	c.Redirect(http.StatusFound, "/admin/domains")
}

// DNSHint renders the DNS configuration hints for a specific domain.
func (h *AdminHandler) DNSHint(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的域名ID")
		return
	}

	domain, err := h.stores.Domains.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "域名不存在")
		return
	}

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_dns_hint", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "domains",
		"domain":       domain,
		"dkimRecord":   dkim.GetDKIMDNSRecord(domain.DkimPublicKey),
	})
}

// ListUsers renders the user list page.
func (h *AdminHandler) ListUsers(c *gin.Context) {
	page := getPageParam(c, "page", 1)

	users, total, err := h.stores.Users.ListAll(page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载用户列表失败: %v", err)
		return
	}

	// Get all domains for display
	domains, _, _ := h.stores.Domains.List(1, 1000)

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "admin_users", gin.H{
		"currentUser":  currentUser,
		"users":        users,
		"domains":      domains,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"activeFolder": "users",
	})
}

// NewUser renders the new user form page.
func (h *AdminHandler) NewUser(c *gin.Context) {
	domains, _, _ := h.stores.Domains.List(1, 1000)

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_user_form", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "users",
		"error":        "",
		"isEdit":       false,
		"domains":      domains,
		"user":         &db.User{},
	})
}

// CreateUser processes the new user form submission.
func (h *AdminHandler) CreateUser(c *gin.Context) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	domainID := formUintOrDefault(c, "domain_id", 0)
	quotaGB := formIntOrDefault(c, "quota_gb", 5)
	isAdmin := c.PostForm("is_admin") == "on"

	if username == "" || password == "" || domainID == 0 {
		domains, _, _ := h.stores.Domains.List(1, 1000)
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_user_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "users",
			"error":        "请填写所有必填字段",
			"isEdit":       false,
			"domains":      domains,
			"user": &db.User{
				Username: username,
				DomainID: domainID,
				IsAdmin:  isAdmin,
			},
		})
		return
	}

	// Hash password
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		domains, _, _ := h.stores.Domains.List(1, 1000)
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusInternalServerError, "admin_user_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "users",
			"error":        "密码加密失败",
			"isEdit":       false,
			"domains":      domains,
			"user": &db.User{
				Username: username,
				DomainID: domainID,
				IsAdmin:  isAdmin,
			},
		})
		return
	}

	quotaBytes := int64(quotaGB) * 1024 * 1024 * 1024

	user := &db.User{
		Username:     username,
		PasswordHash: string(hashedPassword),
		DomainID:     domainID,
		QuotaBytes:   quotaBytes,
		UsedBytes:    0,
		IsActive:     true,
		IsAdmin:      isAdmin,
	}

	if err := h.stores.Users.Create(user); err != nil {
		domains, _, _ := h.stores.Domains.List(1, 1000)
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_user_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "users",
			"error":        fmt.Sprintf("创建用户失败: %v", err),
			"isEdit":       false,
			"domains":      domains,
			"user":         user,
		})
		return
	}

	c.Redirect(http.StatusFound, "/admin/users")
}

// DeleteUser removes a user by ID.
func (h *AdminHandler) DeleteUser(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的用户ID")
		return
	}

	currentUser, _ := c.Get("currentUser")
	if currentUser.(*db.User).ID == uint(id) {
		c.String(http.StatusBadRequest, "不能删除自己的账户")
		return
	}

	if err := h.stores.Users.Delete(uint(id)); err != nil {
		c.String(http.StatusInternalServerError, "删除用户失败: %v", err)
		return
	}

	c.Redirect(http.StatusFound, "/admin/users")
}

// EditUser renders the edit user form page.
func (h *AdminHandler) EditUser(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的用户ID")
		return
	}

	user, err := h.stores.Users.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "用户不存在")
		return
	}

	domains, _, _ := h.stores.Domains.List(1, 1000)
	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_user_form", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "users",
		"error":        "",
		"isEdit":       true,
		"domains":      domains,
		"user":         user,
	})
}

// UpdateUser processes the edit user form submission.
func (h *AdminHandler) UpdateUser(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的用户ID")
		return
	}

	user, err := h.stores.Users.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "用户不存在")
		return
	}

	username := c.PostForm("username")
	domainID := formUintOrDefault(c, "domain_id", user.DomainID)
	quotaGB := formIntOrDefault(c, "quota_gb", int(user.QuotaBytes/(1024*1024*1024)))
	isActive := c.PostForm("is_active") == "on"
	isAdmin := c.PostForm("is_admin") == "on"
	password := c.PostForm("password")

	if username != "" {
		user.Username = username
	}
	user.DomainID = domainID
	user.QuotaBytes = int64(quotaGB) * 1024 * 1024 * 1024
	user.IsActive = isActive
	user.IsAdmin = isAdmin

	// Update password only if a new one is provided
	if password != "" {
		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			domains, _, _ := h.stores.Domains.List(1, 1000)
			currentUser, _ := c.Get("currentUser")
			c.HTML(http.StatusInternalServerError, "admin_user_form", gin.H{
				"currentUser":  currentUser,
				"activeFolder": "users",
				"error":        "密码加密失败",
				"isEdit":       true,
				"domains":      domains,
				"user":         user,
			})
			return
		}
		user.PasswordHash = string(hashedPassword)
		// 管理员重置的密码必须由用户本人修改后才能正常使用
		user.MustChangePassword = true
	}

	if err := h.stores.Users.Update(user); err != nil {
		domains, _, _ := h.stores.Domains.List(1, 1000)
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_user_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "users",
			"error":        fmt.Sprintf("更新用户失败: %v", err),
			"isEdit":       true,
			"domains":      domains,
			"user":         user,
		})
		return
	}

	c.Redirect(http.StatusFound, "/admin/users")
}

// ListBans renders the IP ban list page.
func (h *AdminHandler) ListBans(c *gin.Context) {
	page := getPageParam(c, "page", 1)

	bans, total, err := h.stores.Bans.List(page, 20)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载黑名单失败: %v", err)
		return
	}

	// 标记当前是否仍处于封禁中
	now := time.Now()
	rows := make([]banRow, 0, len(bans))
	for _, b := range bans {
		rows = append(rows, banRow{BanEntry: b, Active: b.ExpiresAt.After(now)})
	}

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "admin_bans", gin.H{
		"currentUser":  currentUser,
		"rows":         rows,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"activeFolder": "bans",
	})
}

// banRow 是黑名单列表行：附带了当前是否封禁中的标记。
type banRow struct {
	db.BanEntry
	Active bool
}

// UnbanIP removes a ban entry by ID.
func (h *AdminHandler) UnbanIP(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的记录ID")
		return
	}

	if err := h.stores.Bans.Delete(uint(id)); err != nil {
		c.String(http.StatusInternalServerError, "解封失败: %v", err)
		return
	}

	c.Redirect(http.StatusFound, "/admin/bans")
}

// ListProtocolLogs 渲染协议调用日志页（SMTP/IMAP/POP3 调用记录，支持筛选）。
func (h *AdminHandler) ListProtocolLogs(c *gin.Context) {
	// 页面访问时顺带清理过期日志，避免日志表无限增长
	h.stores.ProtocolLogs.CleanupBefore(time.Now().AddDate(0, 0, -h.protocolLogKeepDays))

	page := getPageParam(c, "page", 1)
	pageSize := 50

	var success *bool
	switch c.Query("success") {
	case "success":
		v := true
		success = &v
	case "fail":
		v := false
		success = &v
	}

	from := parseDateQuery(c.Query("from"))
	to := parseDateQuery(c.Query("to"))
	// 日期选择到天，含当天
	if !to.IsZero() {
		to = to.AddDate(0, 0, 1)
	}

	filter := store.ProtocolLogFilter{
		Protocol: c.Query("protocol"),
		Success:  success,
		IP:       strings.TrimSpace(c.Query("ip")),
		Username: strings.TrimSpace(c.Query("username")),
		From:     from,
		To:       to,
	}

	logs, total, err := h.stores.ProtocolLogs.List(page, pageSize, filter)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载协议日志失败: %v", err)
		return
	}

	// 统计卡片：今日 + 全部成功/失败数（按协议），int64 → int 供模板 add 使用
	dayStart := time.Now().Truncate(24 * time.Hour)
	todayStats, _ := h.stores.ProtocolLogs.CountStats(dayStart)
	allStats, _ := h.stores.ProtocolLogs.CountStats(time.Time{})
	normStats := func(m map[string]map[string]int64) map[string]map[string]int {
		out := make(map[string]map[string]int, len(m))
		for proto, counts := range m {
			out[proto] = map[string]int{"success": int(counts["success"]), "fail": int(counts["fail"])}
		}
		return out
	}

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / pageSize
	if int(total)%pageSize > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	// 分页/筛选链接保留当前筛选条件（URL 编码防止特殊字符破坏链接）
	query := map[string]string{
		"protocol": url.QueryEscape(filter.Protocol),
		"success":  url.QueryEscape(c.Query("success")),
		"ip":       url.QueryEscape(filter.IP),
		"username": url.QueryEscape(filter.Username),
		"from":     url.QueryEscape(c.Query("from")),
		"to":       url.QueryEscape(c.Query("to")),
	}

	c.HTML(200, "admin_protocol_logs", gin.H{
		"currentUser":  currentUser,
		"logs":         logs,
		"total":        total,
		"page":         page,
		"pageSize":     pageSize,
		"totalPages":   totalPages,
		"filter":       query,
		"todayStats":   normStats(todayStats),
		"allStats":     normStats(allStats),
		"keepDays":     h.protocolLogKeepDays,
		"activeFolder": "protocol-logs",
	})
}

// CleanupProtocolLogs 手动清理超出保留天数的协议日志。
func (h *AdminHandler) CleanupProtocolLogs(c *gin.Context) {
	_, _ = h.stores.ProtocolLogs.CleanupBefore(time.Now().AddDate(0, 0, -h.protocolLogKeepDays))
	c.Redirect(http.StatusFound, "/admin/protocol-logs")
}

// parseDateQuery 解析 YYYY-MM-DD 日期，失败返回零值。
func parseDateQuery(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ListMails renders the admin mail list page showing all messages across all users.
func (h *AdminHandler) ListMails(c *gin.Context) {
	page := getPageParam(c, "page", 1)
	folder := c.Query("folder")

	var messages []db.Message
	var total int64
	var err error

	if folder != "" {
		messages, total, err = h.stores.Mails.ListAllByFolder(folder, page, 20)
	} else {
		messages, total, err = h.stores.Mails.ListAll(page, 20)
	}

	if err != nil {
		c.String(http.StatusInternalServerError, "加载邮件列表失败: %v", err)
		return
	}

	currentUser, _ := c.Get("currentUser")

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	c.HTML(200, "admin_mails", gin.H{
		"currentUser":  currentUser,
		"messages":     messages,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"folder":       folder,
		"activeFolder": "mails",
	})
}

// AdminViewMail renders the detail view of a specific mail for admin.
func (h *AdminHandler) AdminViewMail(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的邮件ID")
		return
	}

	msg, err := h.stores.Mails.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "邮件不存在")
		return
	}

	// 加载附件
	attachments, _ := h.stores.Attachments.ListByMessage(uint(id))

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_mail_view", gin.H{
		"currentUser":  currentUser,
		"message":      msg,
		"attachments":  attachments,
		"activeFolder": "mails",
	})
}

// AdminDownloadAttachment serves an attachment file for admin (bypasses user ownership check).
func (h *AdminHandler) AdminDownloadAttachment(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的附件ID")
		return
	}

	att, err := h.stores.Attachments.GetByID(uint(id))
	if err != nil {
		c.String(http.StatusNotFound, "附件不存在")
		return
	}

	data, err := h.storage.Read(att.FilePath)
	if err != nil {
		c.String(http.StatusInternalServerError, "读取附件失败")
		return
	}

	c.Header("Content-Disposition", formatContentDisposition(att.FileName))
	c.Data(http.StatusOK, att.ContentType, data)
}

// ListOutbound renders the outbound delivery queue page.
func (h *AdminHandler) ListOutbound(c *gin.Context) {
	page := getPageParam(c, "page", 1)
	status := c.Query("status")

	items, total, err := h.stores.Outbound.List(page, 20, status)
	if err != nil {
		c.String(http.StatusInternalServerError, "加载外发队列失败: %v", err)
		return
	}

	// Queue statistics for the summary cards.
	statCounts := make(map[string]int64)
	for _, s := range []string{
		db.OutboundStatusPending,
		db.OutboundStatusDeferred,
		db.OutboundStatusSent,
		db.OutboundStatusFailed,
	} {
		n, _ := h.stores.Outbound.CountByStatus(s)
		statCounts[s] = n
	}

	totalPages := int(total) / 20
	if int(total)%20 > 0 {
		totalPages++
	}
	if totalPages < 1 {
		totalPages = 0
	}

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_outbound", gin.H{
		"currentUser":  currentUser,
		"items":        items,
		"total":        total,
		"page":         page,
		"pageSize":     20,
		"totalPages":   totalPages,
		"status":       status,
		"statCounts":   statCounts,
		"statusText":   outbound.StatusText,
		"activeFolder": "outbound",
	})
}

// RetryOutbound resets an outbound queue item for immediate redelivery.
func (h *AdminHandler) RetryOutbound(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的队列ID")
		return
	}

	if h.outbound == nil {
		c.String(http.StatusInternalServerError, "外发服务不可用")
		return
	}
	if err := h.outbound.Retry(uint(id)); err != nil {
		c.String(http.StatusInternalServerError, "重试失败: %v", err)
		return
	}

	c.Redirect(http.StatusFound, "/admin/outbound")
}

// CancelOutbound cancels a queued outbound message.
func (h *AdminHandler) CancelOutbound(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.String(http.StatusBadRequest, "无效的队列ID")
		return
	}

	if h.outbound == nil {
		c.String(http.StatusInternalServerError, "外发服务不可用")
		return
	}
	if err := h.outbound.Cancel(uint(id)); err != nil {
		c.String(http.StatusInternalServerError, "取消失败: %v", err)
		return
	}

	c.Redirect(http.StatusFound, "/admin/outbound")
}

// formIntOrDefault extracts an integer from a form field, returning the default if missing/invalid.

// formIntOrDefault extracts an integer from a form field, returning the default if missing/invalid.
func formIntOrDefault(c *gin.Context, key string, defaultVal int) int {
	val := c.PostForm(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return n
}

// formUintOrDefault extracts a uint from a form field, returning the default if missing/invalid.
func formUintOrDefault(c *gin.Context, key string, defaultVal uint) uint {
	val := c.PostForm(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil {
		return defaultVal
	}
	return uint(n)
}
