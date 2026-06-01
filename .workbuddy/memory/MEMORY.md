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

## 增强功能（v2）
- DKIM: 创建域名时自动生成RSA 2048密钥对，DNS提示页显示DKIM TXT记录，internal/dkim/keys.go
- 域名编辑: /admin/domains/:id/edit，可改端口/TLS/重新生成DKIM
- 附件配额: 上传时检查用户QuotaBytes，同步更新UsedBytes，SMTP收信也更新
- 富文本: compose页集成Quill.js(CDN)，保存纯文本+HTML到数据库，发送multipart/alternative
- OAuth2/LDAP: 默认关闭，配置文件控制；internal/auth/ 包（provider.go/ldap.go/oauth2.go）
  - 依赖: github.com/go-ldap/ldap/v3, golang.org/x/oauth2
- Domain模型新增: DkimSelector, DkimPrivateKey, DkimPublicKey
- Config新增: Auth AuthConfig（OAuth2+LDAP各项配置）

## 增强功能（v3）
- Banlist系统: 登录失败N次ban IP，次数/时长后台可配(BanConfig)，admin /admin/bans 查看/解ban
  - BanEntry模型、BanStore、BanMiddleware、banned.html、admin/bans.html
  - AuthHandler新增banCfg字段，DoLogin/LDAPLogin集成ban检查
  - 配置: max_fail_attempts(默认5), ban_duration_min(默认30分钟)
- 管理仪表盘增强: 邮件分布表(INBOX/Sent/Drafts/Trash计数+大小)、今日/7日收发统计、ban计数
  - MailStore新增: CountByFolder, CountAll, TotalSizeByFolder, TotalSize, CountByFolderSince
- 全量邮件查看: /admin/mails 支持文件夹筛选(INBOX/Sent/Drafts/Trash)，分页
  - MailStore新增: ListAll, ListAllByFolder
  - admin/mails.html
- Admin sidebar统一: 控制面板/域名管理/用户管理/所有邮件/IP黑名单

## 已知坑
- go-imap v2 是beta，API不稳定，必须用v1
- Go filepath.Glob 不支持 ** 递归匹配，模板分两轮加载
- SMTP ListenAndServeTLS() 不接受参数，TLS需通过 TLSConfig 设置
- User模型密码字段为 PasswordHash，数据库列为 password_hash
