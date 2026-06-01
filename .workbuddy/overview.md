# MailGo 交付总览

## TL;DR
MailGo 邮件系统开发完成，包含 SMTP/IMAP/POP3 协议服务 + Gin Web 管理界面，2轮QA全部通过。

## 交付概览
- **状态**: ✅ 开发完成，QA验证通过
- **测试通过率**: 12/12 (Round 2)
- **已知问题**: 0

## 文件清单

### 配置
- `config/defaults.go` — 默认路径与端口常量（Linux/Windows双平台）
- `config/config.go` — TOML配置加载+自动补全

### 数据库
- `internal/db/models.go` — GORM模型（User/Domain/Message/Attachment）
- `internal/db/db.go` — 数据库初始化（SQLite/MySQL + AutoMigrate）

### Store层
- `internal/store/stores.go` — Stores聚合器
- `internal/store/user_store.go` — 用户CRUD + UpdatePassword
- `internal/store/mail_store.go` — 邮件CRUD + 文件夹查询
- `internal/store/domain_store.go` — 域名CRUD
- `internal/store/attachment_store.go` — 附件CRUD

### 附件存储
- `internal/storage/attachment.go` — UUID文件名+磁盘读写

### 协议服务
- `internal/smtp_server/server.go` — SMTP服务（go-smtp + TLS）
- `internal/imap_server/server.go` — IMAP服务（go-imap v1）
- `internal/imap_server/backend.go` — IMAP后端实现
- `internal/pop3_server/server.go` — POP3服务（手写TCP协议）

### Web服务
- `internal/web/server.go` — Gin引擎+路由+模板+会话
- `internal/web/middleware/auth.go` — 认证中间件
- `internal/web/middleware/admin.go` — 管理员中间件
- `internal/web/handlers/auth.go` — 登录/登出
- `internal/web/handlers/mail.go` — 收件箱/草稿/发件/撰写/查看/设置
- `internal/web/handlers/admin.go` — 管理后台（域名/用户/DNS提示）

### 模板（自包含+子模板模式）
- `internal/web/templates/base.html` — styles + navbar 子模板
- `internal/web/templates/login.html`
- `internal/web/templates/inbox.html`
- `internal/web/templates/drafts.html`
- `internal/web/templates/sent.html`
- `internal/web/templates/compose.html`
- `internal/web/templates/view.html`
- `internal/web/templates/settings.html`
- `internal/web/templates/admin/dashboard.html`
- `internal/web/templates/admin/domains.html`
- `internal/web/templates/admin/domain_form.html`
- `internal/web/templates/admin/dns_hint.html`
- `internal/web/templates/admin/users.html`
- `internal/web/templates/admin/user_form.html`

### 入口
- `main.go` — 配置加载→DB初始化→Store创建→默认数据→启动四大服务

## 用户下一步建议
1. `go run main.go` 启动服务，访问 http://localhost:8080
2. 默认管理员: admin@example.com / admin
3. 生产环境需修改 `mail_go.toml` 中的 session secret key
4. 配置 TLS 证书后启用 SMTP/IMAP/POP3 的 TLS 端口
5. 配置 DNS MX/SPF/DKIM/DMARC 记录（管理后台有提示页面）
