package middleware

import (
	"net/http"

	"mail_go/internal/db"
	"mail_go/internal/i18n"

	"github.com/gin-gonic/gin"
)

// AdminMiddleware checks that the current user has admin privileges.
// Must be used after AuthMiddleware so that "currentUser" is available.
func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		lang := i18n.FromAcceptLanguage(c.GetHeader("Accept-Language"))
		if v, ok := c.Get("lang"); ok {
			if s, ok := v.(string); ok && i18n.Supported(s) {
				lang = s
			}
		}
		userVal, exists := c.Get("currentUser")
		if !exists {
			c.String(http.StatusForbidden, i18n.T(lang, "禁止访问"))
			c.Abort()
			return
		}

		user, ok := userVal.(*db.User)
		if !ok || !user.IsAdmin {
			c.String(http.StatusForbidden, i18n.T(lang, "禁止访问：需要管理员权限"))
			c.Abort()
			return
		}

		c.Next()
	}
}
