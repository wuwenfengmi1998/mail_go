package handlers

import (
	"fmt"
	"net/http"
	"strconv"

	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// AdminHandler handles admin-related routes (dashboard, domain/user management).
type AdminHandler struct {
	stores *store.Stores
}

// NewAdminHandler creates a new AdminHandler with the given stores.
func NewAdminHandler(stores *store.Stores) *AdminHandler {
	return &AdminHandler{stores: stores}
}

// Dashboard renders the admin dashboard with summary statistics.
func (h *AdminHandler) Dashboard(c *gin.Context) {
	_, domainCount, _ := h.stores.Domains.List(1, 1)
	_, userCount, _ := h.stores.Users.ListAll(1, 1)

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_dashboard", gin.H{
		"currentUser":  currentUser,
		"domainCount":  domainCount,
		"userCount":    userCount,
		"activeFolder": "admin",
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
		"activeFolder": "admin",
	})
}

// NewDomain renders the new domain form page.
func (h *AdminHandler) NewDomain(c *gin.Context) {
	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_domain_form", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "admin",
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
			"activeFolder": "admin",
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

	if err := h.stores.Domains.Create(domain); err != nil {
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_domain_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "admin",
			"error":        fmt.Sprintf("创建域名失败: %v", err),
			"isEdit":       false,
			"domain":       domain,
		})
		return
	}

	c.Redirect(http.StatusFound, "/admin/domains")
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
		"activeFolder": "admin",
		"domain":       domain,
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
		"activeFolder": "admin",
	})
}

// NewUser renders the new user form page.
func (h *AdminHandler) NewUser(c *gin.Context) {
	domains, _, _ := h.stores.Domains.List(1, 1000)

	currentUser, _ := c.Get("currentUser")

	c.HTML(200, "admin_user_form", gin.H{
		"currentUser":  currentUser,
		"activeFolder": "admin",
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
			"activeFolder": "admin",
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
			"activeFolder": "admin",
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
			"activeFolder": "admin",
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
		"activeFolder": "admin",
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
				"activeFolder": "admin",
				"error":        "密码加密失败",
				"isEdit":       true,
				"domains":      domains,
				"user":         user,
			})
			return
		}
		user.PasswordHash = string(hashedPassword)
	}

	if err := h.stores.Users.Update(user); err != nil {
		domains, _, _ := h.stores.Domains.List(1, 1000)
		currentUser, _ := c.Get("currentUser")
		c.HTML(http.StatusBadRequest, "admin_user_form", gin.H{
			"currentUser":  currentUser,
			"activeFolder": "admin",
			"error":        fmt.Sprintf("更新用户失败: %v", err),
			"isEdit":       true,
			"domains":      domains,
			"user":         user,
		})
		return
	}

	c.Redirect(http.StatusFound, "/admin/users")
}

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
