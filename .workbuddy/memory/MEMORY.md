# MailGo 项目记忆

## 技术栈
- Go + Gin + html/template + GORM + SQLite(default)/MySQL
- SMTP: github.com/emersion/go-smtp
- IMAP: github.com/emersion/go-imap v1 (NOT v2 beta)
- POP3: 手写TCP协议实现
- 配置: TOML (github.com/BurntSushi/toml)
- 会话: github.com/gin-contrib/sessions (cookie store)
- 密码: golang.org/x/crypto/bcrypt

## 关键架构决策
- 模板采用自包含+子模板模式（Go html/template不支持变量模板名）
- 每个模板是完整HTML文档，通过 {{template "styles" .}} 和 {{template "navbar" .}} 引入公共部分
- Handler调用 c.HTML(200, "define_name", data)，define_name 对应模板名
- SMTP TLS: 创建独立Server实例设置 TLSConfig
- Windows路径回退: ./win/etc/mail_go 和 ./win/srv/mail_go
- 默认管理员: admin@example.com / admin

## 已知坑
- go-imap v2 是beta，API不稳定，必须用v1
- Go filepath.Glob 不支持 ** 递归匹配，模板分两轮加载
- SMTP ListenAndServeTLS() 不接受参数，TLS需通过 TLSConfig 设置
- User模型密码字段为 PasswordHash，数据库列为 password_hash
