package handlers

import (
	"mail_go/internal/i18n"

	"github.com/gin-gonic/gin"
)

// langOf 返回当前请求的界面语言：优先取中间件解析好的 context 值
// （登录用户偏好 / 浏览器语言），未设置时按浏览器 Accept-Language 兜底。
func langOf(c *gin.Context) string {
	if v, ok := c.Get("lang"); ok {
		if s, ok := v.(string); ok && i18n.Supported(s) {
			return s
		}
	}
	return i18n.FromAcceptLanguage(c.GetHeader("Accept-Language"))
}

// withLang 把当前请求的界面语言注入模板数据（key "lang"），
// 所有 c.HTML 渲染入口都应经过它，保证模板可调用 {{t .Lang "..."}}。
func withLang(c *gin.Context, data gin.H) gin.H {
	if data == nil {
		data = gin.H{}
	}
	data["Lang"] = langOf(c)
	return data
}

// normLang 把表单提交的语言偏好规整为合法值（auto | en | zh | ja），
// 非法值回退 auto（跟随浏览器）。
func normLang(v string) string {
	switch v {
	case i18n.LangAuto, i18n.LangEn, i18n.LangZh, i18n.LangJa:
		return v
	default:
		return i18n.LangAuto
	}
}
