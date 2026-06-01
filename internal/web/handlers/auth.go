package handlers

import (
	"mail_go/internal/store"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// AuthHandler handles authentication-related routes (login, logout).
type AuthHandler struct {
	stores *store.Stores
}

// NewAuthHandler creates a new AuthHandler with the given stores.
func NewAuthHandler(stores *store.Stores) *AuthHandler {
	return &AuthHandler{stores: stores}
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
		"error": "",
	})
}

// DoLogin processes the login form submission.
// It authenticates the user with email and password, sets session data
// on success, or re-renders the login page with an error on failure.
func (h *AuthHandler) DoLogin(c *gin.Context) {
	email := c.PostForm("email")
	password := c.PostForm("password")

	if email == "" || password == "" {
		c.HTML(200, "login", gin.H{
			"error": "请输入邮箱和密码",
		})
		return
	}

	user, err := h.stores.Users.Authenticate(email, password)
	if err != nil {
		c.HTML(200, "login", gin.H{
			"error": "用户名或密码错误",
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
			"error": "会话保存失败，请重试",
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
