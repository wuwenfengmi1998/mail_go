package middleware

import (
	"net/http"

	"mail_go/internal/i18n"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
)

// BanMiddleware checks if the client IP is currently banned.
// If banned, it renders the "banned" template and aborts the request.
func BanMiddleware(stores *store.Stores) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		banned, entry := stores.Bans.IsBanned(ip)
		if banned {
			// 封禁页无登录用户，按浏览器语言渲染
			lang := i18n.FromAcceptLanguage(c.GetHeader("Accept-Language"))
			c.Set("lang", lang)
			c.HTML(http.StatusForbidden, "banned", gin.H{
				"entry": entry,
				"Lang":  lang,
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
