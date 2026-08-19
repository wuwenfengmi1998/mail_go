package middleware

import (
	"github.com/gin-gonic/gin"
)

// securityCSP 是本应用的基础 CSP。
//
// 说明：
//   - 模板大量使用内联脚本/样式（Quill 初始化、行内事件处理、
//     avatarStyle 内联 CSS），故 script-src / style-src 需要
//     'unsafe-inline'；
//   - 邮件正文在 srcdoc iframe 中渲染，可能引用远程图片（https:），
//     因此 img-src 放行 https，同时仍阻止 data: 以外的自定义协议；
//   - frame-ancestors 'none' 与 X-Frame-Options 共同防护点击劫持；
//   - connect-src 'self' / form-action 'self' 阻止页面数据外泄到
//     外部域名。
const securityCSP = "default-src 'self'; " +
	"script-src 'self' 'unsafe-inline'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: https:; " +
	"connect-src 'self'; object-src 'none'; base-uri 'self'; " +
	"form-action 'self'; frame-ancestors 'none'"

// SecurityHeaders 为所有响应设置基础安全头：HSTS、点击劫持防护、
// MIME 嗅探防护、Referrer 策略与基础 CSP。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", securityCSP)
		c.Next()
	}
}
