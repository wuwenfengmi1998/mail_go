package auth

// Provider 定义外部认证接口
type Provider interface {
	// Authenticate 验证外部凭据，返回邮箱地址
	Authenticate(credentials map[string]string) (email string, err error)
	// Name 返回提供者名称
	Name() string
}
