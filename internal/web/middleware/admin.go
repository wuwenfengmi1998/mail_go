package middleware

import (
	"net/http"

	"mail_go/internal/db"

	"github.com/gin-gonic/gin"
)

// AdminMiddleware checks that the current user has admin privileges.
// Must be used after AuthMiddleware so that "currentUser" is available.
func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		userVal, exists := c.Get("currentUser")
		if !exists {
			c.String(http.StatusForbidden, "禁止访问")
			c.Abort()
			return
		}

		user, ok := userVal.(*db.User)
		if !ok || !user.IsAdmin {
			c.String(http.StatusForbidden, "禁止访问：需要管理员权限")
			c.Abort()
			return
		}

		c.Next()
	}
}
