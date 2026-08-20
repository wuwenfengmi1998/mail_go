// Package i18n provides lightweight UI localization for the MailGo web
// frontend. Supported languages: zh (Chinese), en (English, fallback) and
// ja (Japanese). Users may also pick "auto" so the effective language is
// derived from their browser's Accept-Language header.
//
// Translation keys are the original Chinese UI strings themselves: the zh
// catalog is the identity map, and the en/ja catalogs map those keys to
// their translations. Missing translations fall back to English, then to
// the key itself.
package i18n

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Language preference values stored on the user record.
const (
	// LangAuto 跟随浏览器语言（Accept-Language）。
	LangAuto = "auto"
	// LangEn 英语：兜底语言。
	LangEn = "en"
	// LangZh 中文。
	LangZh = "zh"
	// LangJa 日文。
	LangJa = "ja"
)

// Supported 判断 lang 是否为可显式选择的界面语言（不含 auto）。
func Supported(lang string) bool {
	switch lang {
	case LangEn, LangZh, LangJa:
		return true
	default:
		return false
	}
}

// Normalize 把浏览器语言标签规整为受支持的语言代码：
// zh-CN / zh-TW / zh-Hans → zh；ja-JP → ja；en-US / en-GB → en；
// 其余返回空串（无法识别）。
func Normalize(tag string) string {
	tag = strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexByte(tag, '-'); i >= 0 {
		tag = tag[:i]
	}
	if i := strings.IndexByte(tag, '_'); i >= 0 {
		tag = tag[:i]
	}
	switch tag {
	case LangZh, "zh-hans", "zh-hant":
		return LangZh
	case LangJa:
		return LangJa
	case LangEn:
		return LangEn
	default:
		return ""
	}
}

// FromAcceptLanguage 从 Accept-Language 请求头解析首选语言。
// 按 q 值（缺省 1.0）降序取第一个受支持的语言；都不支持时回退英语。
func FromAcceptLanguage(header string) string {
	type entry struct {
		lang string
		q    float64
	}
	var entries []entry
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag := part
		q := 1.0
		if i := strings.IndexByte(part, ';'); i >= 0 {
			tag = strings.TrimSpace(part[:i])
			for _, param := range strings.Split(part[i+1:], ";") {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(param, "q=") {
					if v, err := strconv.ParseFloat(param[2:], 64); err == nil {
						q = v
					}
				}
			}
		}
		if lang := Normalize(tag); lang != "" {
			entries = append(entries, entry{lang: lang, q: q})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].q > entries[j].q })
	for _, e := range entries {
		if e.q > 0 {
			return e.lang
		}
	}
	return LangEn
}

// Resolve 计算有效界面语言：用户偏好为显式语言时直接采用；
// auto（或空/未知值）时按浏览器 Accept-Language 解析；最终兜底英语。
func Resolve(pref, acceptLanguage string) string {
	if Supported(pref) {
		return pref
	}
	return FromAcceptLanguage(acceptLanguage)
}

// T 返回 key 在 lang 下的译文。zh 直接返回 key；
// ja 优先查日文表，缺译回退英文表；其余语言（含未知）查英文表，
// 仍缺失时原样返回 key（保证界面永不出现空文本）。
func T(lang, key string) string {
	switch Normalize(lang) {
	case LangZh:
		return key
	case LangJa:
		if s, ok := ja[key]; ok {
			return s
		}
		if s, ok := en[key]; ok {
			return s
		}
		return key
	default:
		if s, ok := en[key]; ok {
			return s
		}
		return key
	}
}

// TF 是 T 的格式化版本：对译文执行 fmt.Sprintf。
// 用于带占位符的消息（如「还剩 %d 次尝试机会」）。
func TF(lang, key string, args ...interface{}) string {
	return fmt.Sprintf(T(lang, key), args...)
}
