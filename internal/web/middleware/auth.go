package middleware

import (
	"mail_go/internal/store"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// AuthMiddleware checks for a valid session and loads the current user
// into the Gin context. If no valid session exists, it redirects to /login.
func AuthMiddleware(stores *store.Stores) gin.HandlerFunc {
	return func(c *gin.Context) {
		session := sessions.Default(c)
		userID := session.Get("userID")
		if userID == nil {
			c.Redirect(302, "/login")
			c.Abort()
			return
		}

		// userID is stored as uint in session, but sessions.Get returns interface{}
		// which may be stored as int or uint depending on the underlying store.
		var id uint
		switch v := userID.(type) {
		case uint:
			id = v
		case int:
			id = uint(v)
		case int64:
			id = uint(v)
		case float64:
			id = uint(v)
		default:
			session.Clear()
			session.Save()
			c.Redirect(302, "/login")
			c.Abort()
			return
		}

		user, err := stores.Users.GetByID(id)
		if err != nil {
			session.Clear()
			session.Save()
			c.Redirect(302, "/login")
			c.Abort()
			return
		}

		// 首次登录/密码被重置的用户必须先修改密码才能使用其他功能
		if user.MustChangePassword && c.Request.URL.Path != "/settings" && c.Request.URL.Path != "/logout" {
			c.Redirect(302, "/settings?force=1")
			c.Abort()
			return
		}

		c.Set("currentUser", user)
		c.Set("userID", id)
		c.Next()
	}
}
