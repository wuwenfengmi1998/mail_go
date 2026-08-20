package middleware

import (
	"time"

	"mail_go/internal/i18n"
	"mail_go/internal/store"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

const (
	// sessionAbsoluteMaxAge 会话绝对过期时间：超过后强制重新登录。
	sessionAbsoluteMaxAge = 7 * 24 * time.Hour
	// sessionSlidingRefresh 滑动续期阈值：距上次刷新超过该时长则更新
	// loginAt 并写回 cookie，保持活跃用户不中断（约 12 小时写回一次）。
	sessionSlidingRefresh = 12 * time.Hour
)

// sessionInt64 兼容不同底层 session store 解码出的整数类型。
func sessionInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case uint:
		return int64(n), true
	case uint64:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}

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

		// 会话绝对过期：登录超过 7 天强制重新登录；
		// 滑动续期：活跃会话每 12 小时刷新一次 loginAt。
		if loginAt, ok := sessionInt64(session.Get("loginAt")); ok {
			elapsed := time.Since(time.Unix(loginAt, 0))
			if elapsed > sessionAbsoluteMaxAge {
				session.Clear()
				session.Save()
				c.Redirect(302, "/login")
				c.Abort()
				return
			}
			if elapsed > sessionSlidingRefresh {
				session.Set("loginAt", time.Now().Unix())
				session.Save()
			}
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

		// 解析界面语言：用户偏好 auto 时按浏览器 Accept-Language 选择，
		// 最终兜底英语。模板数据与错误提示统一使用该值。
		c.Set("lang", i18n.Resolve(user.Language, c.GetHeader("Accept-Language")))

		c.Set("currentUser", user)
		c.Set("userID", id)
		c.Next()
	}
}
