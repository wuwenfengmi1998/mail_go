package auth

import (
	"crypto/tls"
	"fmt"
	"strings"

	"mail_go/config"

	"github.com/go-ldap/ldap/v3"
)

// LDAPProvider 实现 LDAP 认证
type LDAPProvider struct {
	cfg config.AuthConfig
}

// NewLDAPProvider 创建 LDAP 认证提供者
func NewLDAPProvider(cfg config.AuthConfig) *LDAPProvider {
	return &LDAPProvider{cfg: cfg}
}

// Name 返回提供者名称
func (p *LDAPProvider) Name() string { return "ldap" }

// Authenticate 使用 LDAP 验证用户凭据，返回邮箱地址
func (p *LDAPProvider) Authenticate(credentials map[string]string) (string, error) {
	username := credentials["username"]
	password := credentials["password"]
	if username == "" || password == "" {
		return "", fmt.Errorf("用户名和密码不能为空")
	}

	// 使用 go-ldap 库连接
	l, err := ldap.DialURL(p.cfg.LDAPServer)
	if err != nil {
		return "", fmt.Errorf("LDAP 连接失败: %w", err)
	}
	defer l.Close()

	if p.cfg.LDAPUseTLS {
		if err := l.StartTLS(&tls.Config{}); err != nil {
			return "", fmt.Errorf("LDAP TLS 握手失败: %w", err)
		}
	}

	// 用绑定DN搜索用户
	if err := l.Bind(p.cfg.LDAPBindDN, p.cfg.LDAPBindPassword); err != nil {
		return "", fmt.Errorf("LDAP 绑定失败: %w", err)
	}

	// 搜索用户
	filter := fmt.Sprintf(p.cfg.LDAPSearchFilter, ldap.EscapeFilter(username))
	searchReq := ldap.NewSearchRequest(
		p.cfg.LDAPSearchBase,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter,
		[]string{"mail", "uid", "cn"},
		nil,
	)

	sr, err := l.Search(searchReq)
	if err != nil {
		return "", fmt.Errorf("LDAP 搜索失败: %w", err)
	}
	if len(sr.Entries) == 0 {
		return "", fmt.Errorf("LDAP 用户不存在")
	}

	entry := sr.Entries[0]
	userDN := entry.DN
	email := entry.GetAttributeValue("mail")

	// 验证用户密码
	if err := l.Bind(userDN, password); err != nil {
		return "", fmt.Errorf("LDAP 认证失败")
	}

	// 如果没有邮箱属性，尝试从搜索基准拼凑
	if email == "" {
		// 从 LDAPSearchBase 提取域名部分，如 ou=users,dc=example,dc=com -> example.com
		parts := strings.Split(p.cfg.LDAPSearchBase, ",")
		var dcParts []string
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "dc=") {
				dcParts = append(dcParts, strings.TrimPrefix(part, "dc="))
			}
		}
		if len(dcParts) > 0 {
			email = username + "@" + strings.Join(dcParts, ".")
		} else {
			email = username
		}
	}

	return email, nil
}
