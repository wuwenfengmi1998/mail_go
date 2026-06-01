package middleware

import (
	"net/http"

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
			c.HTML(http.StatusForbidden, "banned", gin.H{
				"entry": entry,
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
