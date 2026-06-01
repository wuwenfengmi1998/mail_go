package handlers

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"mail_go/config"
	"mail_go/internal/auth"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// AuthHandler handles authentication-related routes (login, logout, LDAP, OAuth2).
type AuthHandler struct {
	stores  *store.Stores
	authCfg config.AuthConfig
	banCfg  config.BanConfig
}

// NewAuthHandler creates a new AuthHandler with the given stores, auth config, and ban config.
func NewAuthHandler(stores *store.Stores, authCfg config.AuthConfig, banCfg config.BanConfig) *AuthHandler {
	return &AuthHandler{stores: stores, authCfg: authCfg, banCfg: banCfg}
}

// ShowLogin renders the login page.
func (h *AuthHandler) ShowLogin(c *gin.Context) {
	// If already logged in, redirect to inbox
	session := sessions.Default(c)
	if session.Get("userID") != nil {
		c.Redirect(302, "/inbox")
		return
	}
	c.HTML(200, "login", gin.H{
		"error":          "",
		"oauth2Enabled":  h.authCfg.OAuth2Enabled,
		"ldapEnabled":    h.authCfg.LDAPEnabled,
		"oauth2Provider": h.authCfg.OAuth2Provider,
	})
}

// DoLogin processes the login form submission.
// It authenticates the user with email and password, sets session data
// on success, or re-renders the login page with an error on failure.
func (h *AuthHandler) DoLogin(c *gin.Context) {
	ip := c.ClientIP()

	// Check if IP is banned
	banned, entry := h.stores.Bans.IsBanned(ip)
	if banned {
		c.HTML(http.StatusForbidden, "banned", gin.H{"entry": entry})
		return
	}

	email := c.PostForm("email")
	password := c.PostForm("password")

	if email == "" || password == "" {
		c.HTML(200, "login", gin.H{
			"error":          "请输入邮箱和密码",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	user, err := h.stores.Users.Authenticate(email, password)
	if err != nil {
		failCount, _ := h.stores.Bans.IncrementFail(ip)

		if failCount >= h.banCfg.MaxFailAttempts {
			banDuration := time.Duration(h.banCfg.BanDurationMin) * time.Minute
			banEntry := &db.BanEntry{
				IPAddress: ip,
				Reason:    fmt.Sprintf("登录失败次数过多 (%d次)", failCount),
				FailCount: failCount,
				ExpiresAt: time.Now().Add(banDuration),
			}
			h.stores.Bans.Create(banEntry)

			c.HTML(http.StatusForbidden, "banned", gin.H{"entry": banEntry})
			return
		}

		remaining := h.banCfg.MaxFailAttempts - failCount
		c.HTML(200, "login", gin.H{
			"error":          fmt.Sprintf("用户名或密码错误，还剩 %d 次尝试机会", remaining),
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	// Login successful: reset fail count
	h.stores.Bans.ResetFail(ip)

	// Set session values
	session := sessions.Default(c)
	session.Set("userID", user.ID)
	session.Set("userEmail", user.Username+"@"+user.Domain.Name)
	session.Set("isAdmin", user.IsAdmin)
	if err := session.Save(); err != nil {
		c.HTML(200, "login", gin.H{
			"error":          "会话保存失败，请重试",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	c.Redirect(302, "/inbox")
}

// LDAPLogin handles LDAP authentication form submission.
func (h *AuthHandler) LDAPLogin(c *gin.Context) {
	ip := c.ClientIP()

	// Check if IP is banned
	banned, entry := h.stores.Bans.IsBanned(ip)
	if banned {
		c.HTML(http.StatusForbidden, "banned", gin.H{"entry": entry})
		return
	}

	username := c.PostForm("username")
	password := c.PostForm("password")

	if username == "" || password == "" {
		c.HTML(200, "login", gin.H{
			"error":          "请输入LDAP用户名和密码",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	provider := auth.NewLDAPProvider(h.authCfg)
	email, err := provider.Authenticate(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		log.Printf("LDAP 认证失败: %v", err)

		failCount, _ := h.stores.Bans.IncrementFail(ip)
		if failCount >= h.banCfg.MaxFailAttempts {
			banDuration := time.Duration(h.banCfg.BanDurationMin) * time.Minute
			banEntry := &db.BanEntry{
				IPAddress: ip,
				Reason:    fmt.Sprintf("登录失败次数过多 (%d次)", failCount),
				FailCount: failCount,
				ExpiresAt: time.Now().Add(banDuration),
			}
			h.stores.Bans.Create(banEntry)

			c.HTML(http.StatusForbidden, "banned", gin.H{"entry": banEntry})
			return
		}

		remaining := h.banCfg.MaxFailAttempts - failCount
		c.HTML(200, "login", gin.H{
			"error":          fmt.Sprintf("LDAP 认证失败，还剩 %d 次尝试机会: %v", remaining, err),
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	// Look up or auto-create user by email
	user, err := h.stores.Users.GetByEmail(email)
	if err != nil {
		c.HTML(200, "login", gin.H{
			"error":          fmt.Sprintf("LDAP 用户 %s 在系统中不存在", email),
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	if !user.IsActive {
		c.HTML(200, "login", gin.H{
			"error":          "用户已被禁用",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	// Login successful: reset fail count
	h.stores.Bans.ResetFail(ip)

	// Set session values
	session := sessions.Default(c)
	session.Set("userID", user.ID)
	session.Set("userEmail", user.Username+"@"+user.Domain.Name)
	session.Set("isAdmin", user.IsAdmin)
	if err := session.Save(); err != nil {
		c.HTML(200, "login", gin.H{
			"error":          "会话保存失败，请重试",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	c.Redirect(302, "/inbox")
}

// OAuth2Start redirects to the OAuth2 provider's authorization page.
func (h *AuthHandler) OAuth2Start(c *gin.Context) {
	if !h.authCfg.OAuth2Enabled {
		c.String(http.StatusBadRequest, "OAuth2 未启用")
		return
	}

	provider := auth.NewOAuth2Provider(h.authCfg)
	// Use a simple state for CSRF protection (in production, use a random token)
	state := "mailgo_oauth2_state"
	c.Redirect(http.StatusFound, provider.GetAuthURL(state))
}

// OAuth2Callback handles the OAuth2 provider's callback after user authorization.
func (h *AuthHandler) OAuth2Callback(c *gin.Context) {
	if !h.authCfg.OAuth2Enabled {
		c.String(http.StatusBadRequest, "OAuth2 未启用")
		return
	}

	code := c.Query("code")
	if code == "" {
		c.HTML(200, "login", gin.H{
			"error":          "OAuth2 授权码缺失",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	provider := auth.NewOAuth2Provider(h.authCfg)
	email, err := provider.HandleCallback(code)
	if err != nil {
		log.Printf("OAuth2 回调失败: %v", err)
		c.HTML(200, "login", gin.H{
			"error":          fmt.Sprintf("OAuth2 认证失败: %v", err),
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	// Look up user by email
	user, err := h.stores.Users.GetByEmail(email)
	if err != nil {
		c.HTML(200, "login", gin.H{
			"error":          fmt.Sprintf("OAuth2 用户 %s 在系统中不存在", email),
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	if !user.IsActive {
		c.HTML(200, "login", gin.H{
			"error":          "用户已被禁用",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	// Set session values
	session := sessions.Default(c)
	session.Set("userID", user.ID)
	session.Set("userEmail", user.Username+"@"+user.Domain.Name)
	session.Set("isAdmin", user.IsAdmin)
	if err := session.Save(); err != nil {
		c.HTML(200, "login", gin.H{
			"error":          "会话保存失败，请重试",
			"oauth2Enabled":  h.authCfg.OAuth2Enabled,
			"ldapEnabled":    h.authCfg.LDAPEnabled,
			"oauth2Provider": h.authCfg.OAuth2Provider,
		})
		return
	}

	c.Redirect(302, "/inbox")
}

// DoLogout clears the session and redirects to the login page.
func (h *AuthHandler) DoLogout(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	session.Save()
	c.Redirect(302, "/login")
}
