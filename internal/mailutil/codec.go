// Package mailutil provides utilities for email processing:
// charset conversion and RFC 2047 address formatting.
package mailutil

import (
	"fmt"
	"io"
	"mime"
	"strings"
	"unicode/utf8"

	asgomail "github.com/emersion/go-message/mail"
	"golang.org/x/text/encoding/htmlindex"
)

// DecodeCharset converts bytes from the given charset to a UTF-8 string.
// If charset is empty, "utf-8", or "us-ascii", it returns string(buf) directly.
// If the bytes are already valid UTF-8, no conversion is attempted.
func DecodeCharset(buf []byte, charset string) string {
	cs := strings.ToLower(strings.Trim(charset, `"'`))
	if cs == "" || cs == "utf-8" || cs == "us-ascii" {
		return string(buf)
	}

	// 已是合法 UTF-8，无需转换
	if utf8.Valid(buf) {
		return string(buf)
	}

	enc, err := htmlindex.Get(cs)
	if err != nil {
		// 未知字符集，原样返回
		return string(buf)
	}

	decoded, err := enc.NewDecoder().Bytes(buf)
	if err != nil {
		// 解码失败，原样返回
		return string(buf)
	}

	return string(decoded)
}

// DecodeRFC2047 解码邮件头中的 RFC 2047 encoded-word。
func DecodeRFC2047(value string) string {
	decoder := mime.WordDecoder{
		CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
			data, err := io.ReadAll(input)
			if err != nil {
				return nil, err
			}
			return strings.NewReader(DecodeCharset(data, charset)), nil
		},
	}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// FormatAddressList 解析 RFC 2047 编码的地址列表头并格式化为可读字符串。
// 例如: "=?gb2312?B?zuIgzsS35Q==?= <a@b.com>" → "吴文锋 <a@b.com>"
func FormatAddressList(header *asgomail.Header, key string) string {
	addrs, err := header.AddressList(key)
	if err != nil || len(addrs) == 0 {
		// 降级：返回解码后的原始值
		return DecodeRFC2047(header.Get(key))
	}

	var parts []string
	for _, addr := range addrs {
		if addr.Name != "" {
			parts = append(parts, fmt.Sprintf("%s <%s>", addr.Name, addr.Address))
		} else {
			parts = append(parts, addr.Address)
		}
	}
	return strings.Join(parts, ", ")
}

// ExtractCharset 从 Content-Type 头值中提取 charset 参数。
// 例如: "text/plain; charset=gb2312" → "gb2312"
func ExtractCharset(contentType string) string {
	parts := strings.Split(contentType, ";")
	for _, part := range parts[1:] {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(strings.ToLower(part), "charset=") {
			return strings.Trim(strings.TrimPrefix(part, "charset="), `"'`)
		}
	}
	return ""
}
