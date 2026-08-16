# MailGo

Go 语言编写的轻量级邮件系统，集成 SMTP / IMAP / POP3 协议服务和 Web 管理界面。

## 功能特性

- **邮件协议**：SMTP（发送）、IMAP（同步）、POP3（收取），均支持 TLS 加密
- **外部投递**：认证用户可向外部邮箱（QQ/Gmail/Outlook 等）发送邮件，内置外发队列、MX 直投、STARTTLS、指数退避重试、退信通知与 DKIM 签名
- **Web 邮箱**：收件箱、已发送、草稿箱、富文本编辑（Quill.js）、附件上传/下载
- **管理后台**：域名管理、用户管理、DKIM 密钥自动生成、DNS 配置提示、全量邮件查看、外发队列管理、IP 封禁管理、仪表盘统计
- **外部认证**：OAuth2（Google / GitHub）、LDAP（可选，默认关闭）
- **安全机制**：BCrypt 密码哈希、登录失败自动封禁 IP、外发频率限制（防滥用）、非认证禁止中继（防开放中继）、管理员可解封
- **多数据库**：默认 SQLite，可切换 MySQL
- **跨平台**：Linux 生产部署 + Windows 本地调试

## 快速开始

### 编译

```bash
go build -o mailgo .
```

### 启动

```bash
./mailgo
```

首次启动会自动创建配置文件和数据库，默认管理员账户：

| 项目 | 值 |
|------|-----|
| 邮箱 | `admin@example.com` |
| 密码 | `admin` |

> ⚠️ 请在生产环境中立即修改默认密码。

### 访问

| 页面 | 地址 |
|------|------|
| 用户邮箱 | `http://localhost:8080/` |
| 管理后台 | `http://localhost:8080/admin` |

---

## 配置文件

配置文件路径（TOML 格式）：

| 系统 | 路径 |
|------|------|
| Linux | `/etc/mail_go/mail_go.toml` |
| Windows | `./win/etc/mail_go/mail_go.toml` |

首次启动自动生成，缺失字段自动补全。修改后需重启服务生效。

### 完整配置参考

```toml
[database]
driver = "sqlite"                          # sqlite | mysql
dsn = "/srv/mail_go/mail.db"               # SQLite: 文件路径; MySQL: DSN 连接串

[storage]
base_dir = "/srv/mail_go"                  # 数据根目录
attach_dir = "/srv/mail_go/attachments"     # 附件存储目录

[web]
addr = ":8080"                             # 监听地址，支持 TCP 端口或 Unix socket

[smtp]
addr = ":25"                               # SMTP 明文端口
tls_addr = ":465"                          # SMTPS 端口（需配置 TLS 证书）
domain = "example.com"                     # 邮件域名
tls_cert = ""                             # TLS 证书路径（留空则不启用 TLS）
tls_key = ""                              # TLS 私钥路径
max_message_bytes = 67108864              # 单封邮件最大 64MB

[imap]
addr = ":143"                              # IMAP 明文端口
tls_addr = ":993"                          # IMAPS 端口
tls_cert = ""
tls_key = ""

[pop3]
addr = ":110"                              # POP3 明文端口
tls_addr = ":995"                          # POP3S 端口
tls_cert = ""
tls_key = ""

[auth]
oauth2_enabled = false                     # 是否启用 OAuth2 登录
oauth2_provider = ""                       # google | github
oauth2_client_id = ""
oauth2_client_secret = ""
oauth2_redirect_url = ""
ldap_enabled = false                      # 是否启用 LDAP 登录
ldap_server = ""                          # 例: ldap://localhost:389
ldap_bind_dn = ""                         # 例: cn=admin,dc=example,dc=com
ldap_bind_password = ""
ldap_search_base = ""                     # 例: ou=users,dc=example,dc=com
ldap_search_filter = ""                   # 例: (uid=%s)
ldap_use_tls = false

[ban]
max_fail_attempts = 5                    # 登录失败次数阈值
ban_duration_min = 30                     # 封禁时长（分钟）

[outbound]
hostname = ""                            # EHLO 主机名，留空使用 [smtp] domain
poll_interval = 15                       # 外发队列扫描间隔（秒）
max_attempts = 12                        # 单封邮件最大投递尝试次数
retry_base_min = 5                       # 重试退避基数（分钟），指数增长：5/10/20/40...
max_recipients = 50                      # 单封邮件最大外部收件人数
max_per_min = 30                         # 每用户每分钟最大外发数
max_per_day = 500                        # 每用户每日最大外发数，0 表示禁用外部投递
connect_timeout = 30                     # 连接远程 MX 超时（秒）
relay_host = ""                          # 智能主机（smarthost），留空则直投 MX
relay_port = 587                         # 465 = 隐式 TLS，其他端口按需 STARTTLS
relay_user = ""                          # 中继认证用户名（AUTH PLAIN）
relay_password = ""                      # 中继认证密码
relay_starttls = true                    # 非 465 端口是否使用 STARTTLS
```

---

## 常见配置场景

### 1. 切换到 MySQL

```toml
[database]
driver = "mysql"
dsn = "mailgo:YourPassword@tcp(127.0.0.1:3306)/mailgo?charset=utf8mb4&parseTime=True&loc=Local"
```

MySQL DSN 格式：`用户名:密码@tcp(主机:端口)/数据库名?参数`

> 数据库和用户需提前在 MySQL 中创建：
> ```sql
> CREATE DATABASE mailgo CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
> CREATE USER 'mailgo'@'localhost' IDENTIFIED BY 'YourPassword';
> GRANT ALL PRIVILEGES ON mailgo.* TO 'mailgo'@'localhost';
> FLUSH PRIVILEGES;
> ```

### 2. Web 使用 Unix Socket

```toml
[web]
addr = "/run/mail_go/web.sock"
```

当 `addr` 以 `/` 开头时，Gin 自动以 Unix socket 方式监听。

Nginx 反向代理配置：

```nginx
server {
    listen 80;
    server_name mail.example.com;

    location / {
        proxy_pass http://unix:/run/mail_go/web.sock;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

> 确保 socket 目录存在并有写权限：
> ```bash
> mkdir -p /run/mail_go
> chown mailgo:mailgo /run/mail_go
> ```

### 3. 启用 TLS 加密

为 SMTP/IMAP/POP3 启用 TLS（三个协议共享同一证书，或分别配置）：

```toml
[smtp]
tls_cert = "/etc/mail_go/certs/server.crt"
tls_key = "/etc/mail_go/certs/server.key"

[imap]
tls_cert = "/etc/mail_go/certs/server.crt"
tls_key = "/etc/mail_go/certs/server.key"

[pop3]
tls_cert = "/etc/mail_go/certs/server.crt"
tls_key = "/etc/mail_go/certs/server.key"
```

配置 TLS 后，系统会自动启动对应的加密端口（465/993/995）。管理后台添加域名时勾选 TLS 会自动切换端口号。

> 可用 Let's Encrypt 获取免费证书：
> ```bash
> certbot certonly --standalone -d mail.example.com
> # 证书路径: /etc/letsencrypt/live/mail.example.com/fullchain.pem
> # 私钥路径: /etc/letsencrypt/live/mail.example.com/privkey.pem
> ```

### 4. 启用 OAuth2 登录（Google 示例）

```toml
[auth]
oauth2_enabled = true
oauth2_provider = "google"
oauth2_client_id = "your-client-id.apps.googleusercontent.com"
oauth2_client_secret = "your-client-secret"
oauth2_redirect_url = "https://mail.example.com/auth/oauth2/callback"
```

> 需要在 [Google Cloud Console](https://console.cloud.google.com/) 创建 OAuth 2.0 客户端，授权回调地址填 `https://你的域名/auth/oauth2/callback`。

### 5. 启用 LDAP 认证

```toml
[auth]
ldap_enabled = true
ldap_server = "ldap://ldap.example.com:389"
ldap_bind_dn = "cn=admin,dc=example,dc=com"
ldap_bind_password = "ldap_admin_password"
ldap_search_base = "ou=users,dc=example,dc=com"
ldap_search_filter = "(uid=%s)"
ldap_use_tls = true
```

### 6. 对外发送邮件（外部投递）

认证用户（Web 邮箱 / SMTP 提交）可以向外部邮箱地址发送邮件。系统通过
收件人域名的 MX 记录直投，自动处理 STARTTLS、指数退避重试，并为外发
邮件添加 DKIM 签名（使用后台域名管理中生成的密钥）。

```toml
[smtp]
domain = "example.com"        # 必须设置为真实的邮件域名（EHLO/退信地址）

[outbound]
hostname = "mail.example.com" # 建议与邮件主机名一致
poll_interval = 15
max_attempts = 12
max_per_day = 500             # 设为 0 可完全禁用外部投递
```

需要满足的 DNS / 服务器条件（详见 `todo.md` 与后台 DNS 提示页）：

| 项目 | 说明 |
|------|------|
| MX | `example.com MX 10 mail.example.com` |
| SPF | `example.com TXT "v=spf1 mx -all"` |
| DKIM | `default._domainkey.example.com TXT "v=DKIM1; k=rsa; p=<公钥>"`（后台自动生成） |
| DMARC | `_dmarc.example.com TXT "v=DMARC1; p=none; rua=mailto:postmaster@example.com"` |
| PTR | 服务器 IP 反向解析指向邮件主机名（需向机房申请） |
| 网络 | 服务器 25 端口出站未被云厂商封锁 |

安全策略：外部收件人仅接受已认证用户；`MAIL FROM` 必须与登录用户一致；
每用户每分钟/每日外发数受限；失败邮件会退信到发件人收件箱；
管理员可在后台「外发队列」查看投递状态、手动重试或取消。

### 7. 通过智能主机（smarthost）中继外发

服务器 IP 属于家庭宽带/动态 IP 段时，常被 Spamhaus PBL 等策略列表收录，
Microsoft（Outlook/Hotmail）等收件方会直接拒收。此时建议把外发邮件交给
第三方 SMTP 中继（Mailgun / SendGrid / Amazon SES / 阿里云邮件推送等），
在 `[outbound]` 中配置即可，所有外部投递自动改走中继：

```toml
[outbound]
relay_host = "smtp.example-relay.com"
relay_port = 587          # 465 为隐式 TLS
relay_user = "your-api-user"
relay_password = "your-api-key"
relay_starttls = true
```

中继使用 AUTH PLAIN 认证；本地收件人仍走本地投递，不受影响。

---

## 端口速查

| 协议 | 明文端口 | TLS 端口 | 说明 |
|------|---------|---------|------|
| SMTP | 25 | 465 | 邮件发送 |
| IMAP | 143 | 993 | 邮箱同步 |
| POP3 | 110 | 995 | 邮件收取 |
| Web | 8080 | — | Web 界面（可配 Unix socket） |

---

## 目录结构

```
mailgo/
├── main.go                          # 程序入口
├── config/
│   ├── config.go                    # 配置加载与合并
│   └── defaults.go                  # 默认常量
├── internal/
│   ├── db/
│   │   ├── db.go                    # 数据库初始化（SQLite/MySQL）
│   │   └── models.go               # GORM 模型定义
│   ├── store/
│   │   ├── stores.go                # Store 聚合器
│   │   ├── user_store.go            # 用户数据操作
│   │   ├── mail_store.go            # 邮件数据操作
│   │   ├── domain_store.go          # 域名数据操作
│   │   ├── attachment_store.go      # 附件数据操作
│   │   ├── outbound_store.go        # 外发队列数据操作
│   │   └── ban_store.go             # 封禁数据操作
│   ├── smtp_server/server.go        # SMTP 服务
│   ├── outbound/
│   │   ├── mailer.go               # MX 查询与 SMTP 出站客户端
│   │   ├── manager.go              # 外发队列、重试、限速、退信
│   │   └── sign.go                 # DKIM 签名
│   ├── imap_server/
│   │   ├── server.go                # IMAP 服务
│   │   └── backend.go               # IMAP 后端
│   ├── pop3_server/server.go         # POP3 服务
│   ├── storage/attachment.go         # 附件文件存储
│   ├── dkim/keys.go                  # DKIM 密钥生成
│   ├── auth/
│   │   ├── provider.go              # 认证接口
│   │   ├── oauth2.go                # OAuth2 认证
│   │   └── ldap.go                  # LDAP 认证
│   └── web/
│       ├── server.go                 # Web 路由与模板加载
│       ├── handlers/
│       │   ├── auth.go              # 登录/登出/OAuth2/LDAP
│       │   ├── mail.go              # 邮箱操作
│       │   └── admin.go             # 管理后台
│       ├── middleware/
│       │   ├── auth.go              # 会话认证
│       │   ├── admin.go             # 管理员权限
│       │   └── ban.go               # IP 封禁检查
│       └── templates/               # HTML 模板
│           ├── *.html               # 用户页面
│           └── admin/*.html          # 管理页面
└── .gitignore
```

### 运行时数据目录（Linux）

| 路径 | 说明 |
|------|------|
| `/etc/mail_go/` | 配置文件 |
| `/srv/mail_go/` | 数据库 + 附件存储 |

### 运行时数据目录（Windows 调试）

| 路径 | 说明 |
|------|------|
| `./win/etc/mail_go/` | 配置文件 |
| `./win/srv/mail_go/` | 数据库 + 附件存储 |

---

## Linux 服务部署

### systemd 服务文件

创建 `/etc/systemd/system/mailgo.service`：

```ini
[Unit]
Description=MailGo Mail Server
After=network.target mysql.service

[Service]
Type=simple
User=mailgo
Group=mailgo
WorkingDirectory=/opt/mailgo
ExecStart=/opt/mailgo/mailgo
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

# 安全加固
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/srv/mail_go /run/mail_go /etc/mail_go

[Install]
WantedBy=multi-user.target
```

### 启动服务

```bash
# 创建系统用户
sudo useradd -r -s /sbin/nologin -d /srv/mail_go mailgo

# 设置目录权限
sudo chown -R mailgo:mailgo /srv/mail_go /etc/mail_go

# 启用并启动
sudo systemctl daemon-reload
sudo systemctl enable mailgo
sudo systemctl start mailgo

# 查看日志
sudo journalctl -u mailgo -f
```

---

## DNS 配置

邮件服务需要以下 DNS 记录（管理后台 → DNS 提示 页面可查看完整配置）：

| 记录类型 | 名称 | 值 | 说明 |
|---------|------|-----|------|
| A | mail | 服务器 IP | 邮件服务器地址 |
| MX | @ | mail.example.com | 邮件路由 |
| TXT | @ | `v=spf1 mx ~all` | SPF 反垃圾 |
| TXT | default._domainkey | DKIM 公钥（后台自动生成） | DKIM 签名验证 |
| TXT | _dmarc | `v=DMARC1; p=none; rua=mailto:admin@example.com` | DMARC 策略 |

---

## 技术栈

| 组件 | 技术 |
|------|------|
| 语言 | Go 1.25+ |
| Web 框架 | Gin |
| 模板引擎 | html/template |
| ORM | GORM |
| 数据库 | SQLite（默认）/ MySQL |
| 配置格式 | TOML |
| SMTP | github.com/emersion/go-smtp |
| IMAP | github.com/emersion/go-imap v1 |
| POP3 | 手工实现 TCP 协议 |
| 密码哈希 | golang.org/x/crypto/bcrypt |
| 富文本 | Quill.js (CDN) |
| OAuth2 | golang.org/x/oauth2 |
| LDAP | github.com/go-ldap/ldap/v3 |
| DKIM | RSA 2048 自动生成 |

---

## 默认账户

| 角色 | 邮箱 | 密码 | 默认配额 |
|------|------|------|---------|
| 管理员 | admin@example.com | admin | 5 GB |

> ⚠️ 部署到生产环境后请立即修改管理员密码。

## License

MIT
