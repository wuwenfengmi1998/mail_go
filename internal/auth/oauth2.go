package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"mail_go/config"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"
	"golang.org/x/oauth2/google"
)

// OAuth2Provider 实现 OAuth2 认证
type OAuth2Provider struct {
	cfg    config.AuthConfig
	config *oauth2.Config
}

// NewOAuth2Provider 创建 OAuth2 认证提供者
func NewOAuth2Provider(cfg config.AuthConfig) *OAuth2Provider {
	p := &OAuth2Provider{cfg: cfg}

	var endpoint oauth2.Endpoint
	switch cfg.OAuth2Provider {
	case "google":
		endpoint = google.Endpoint
	case "github":
		endpoint = github.Endpoint
	default:
		endpoint = oauth2.Endpoint{
			AuthURL:  fmt.Sprintf("https://%s/login/oauth/authorize", cfg.OAuth2Provider),
			TokenURL: fmt.Sprintf("https://%s/login/oauth/access_token", cfg.OAuth2Provider),
		}
	}

	p.config = &oauth2.Config{
		ClientID:     cfg.OAuth2ClientID,
		ClientSecret: cfg.OAuth2ClientSecret,
		RedirectURL:  cfg.OAuth2RedirectURL,
		Scopes:       []string{"email", "profile"},
		Endpoint:     endpoint,
	}

	return p
}

// Name 返回提供者名称
func (p *OAuth2Provider) Name() string { return "oauth2" }

// GetAuthURL 返回 OAuth2 授权重定向URL
func (p *OAuth2Provider) GetAuthURL(state string) string {
	return p.config.AuthCodeURL(state)
}

// HandleCallback 处理 OAuth2 回调，返回用户邮箱
func (p *OAuth2Provider) HandleCallback(code string) (string, error) {
	token, err := p.config.Exchange(context.Background(), code)
	if err != nil {
		return "", fmt.Errorf("OAuth2 token 交换失败: %w", err)
	}

	email, err := p.fetchUserEmail(token)
	if err != nil {
		return "", err
	}

	return email, nil
}

// fetchUserEmail 从 OAuth2 提供者获取用户邮箱
func (p *OAuth2Provider) fetchUserEmail(token *oauth2.Token) (string, error) {
	client := p.config.Client(context.Background(), token)

	var userinfoURL string
	switch p.cfg.OAuth2Provider {
	case "google":
		userinfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"
	case "github":
		userinfoURL = "https://api.github.com/user/emails"
	default:
		return "", fmt.Errorf("不支持的 OAuth2 提供者: %s", p.cfg.OAuth2Provider)
	}

	resp, err := client.Get(userinfoURL)
	if err != nil {
		return "", fmt.Errorf("获取用户信息失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取用户信息失败: %w", err)
	}

	if p.cfg.OAuth2Provider == "google" {
		var userInfo struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(body, &userInfo); err != nil {
			return "", fmt.Errorf("解析Google用户信息失败: %w", err)
		}
		if userInfo.Email == "" {
			return "", fmt.Errorf("Google账户未关联邮箱")
		}
		return userInfo.Email, nil
	}

	if p.cfg.OAuth2Provider == "github" {
		var emails []struct {
			Email   string `json:"email"`
			Primary bool   `json:"primary"`
		}
		if err := json.Unmarshal(body, &emails); err != nil {
			return "", fmt.Errorf("解析GitHub用户信息失败: %w", err)
		}
		for _, e := range emails {
			if e.Primary {
				return e.Email, nil
			}
		}
		if len(emails) > 0 {
			return emails[0].Email, nil
		}
		return "", fmt.Errorf("GitHub账户未关联邮箱")
	}

	return "", fmt.Errorf("OAuth2 邮箱获取尚未实现")
}

// Authenticate 实现 Provider 接口（OAuth2 不使用此方法，通过回调流程认证）
func (p *OAuth2Provider) Authenticate(credentials map[string]string) (string, error) {
	return "", fmt.Errorf("OAuth2 不支持直接认证，请使用回调流程")
}

// Ensure interfaces are satisfied
var (
	_ Provider = (*LDAPProvider)(nil)
	_ Provider = (*OAuth2Provider)(nil)
)
