# 安全漏洞修复 TODO

依据 2026-08-19 的安全审计结果（代码静态审计 + mail.lmve.net 线上验证）整理。

按优先级排列：P0 立即修复，P1 尽快修复，P2 排期修复，P3 加固项。

## P0 严重：可被完全接管

### 1. 会话签名密钥硬编码，可伪造任意管理员会话

- [x] 位置：`internal/web/server.go:176`
- 现状：`cookie.NewStore([]byte("mail-go-secret-key-change-in-production"))`，密钥写死在源码中且无配置项可更换。源码公开 = 任何人可签发 `userID=1, isAdmin=true` 的合法 cookie，直接以管理员身份进入线上后台。
- 修复方案：
  - [x] `config.Config` 新增 `[web] secret_key` 字段。
  - [x] `LoadConfig()` 首次启动时用 `crypto/rand` 生成 32 字节随机密钥（hex 编码）写入配置文件；配置文件权限收紧为 0600（兼顾原有的中继密码等敏感字段）。
  - [x] 支持环境变量 `MAILGO_SECRET_KEY` 覆盖（覆盖值不落盘，便于容器部署）。
  - [x] 启动时校验：密钥为空 / 等于旧硬编码默认值 / 短于 16 字节时拒绝启动（`config.ValidateSecretKey` + `NewWebServer` 返回 error）。
- 验证：
  - [x] 重启后旧 cookie 全部失效（登录态被踢下线）。
  - [x] 用旧硬编码密钥手工签发的 cookie 无法通过认证（`TestLegacyHardcodedKeyCannotForgeSession`：伪造 `userID=1, isAdmin=true` 的 cookie 被拒，302 回登录页）。
  - [x] `config.LoadConfig()` 单测：首启生成、二次读取保持不变、旧配置补全、旧默认值替换、env 覆盖不落盘、文件权限 0600（`config/config_test.go`）。
- 已完成（2026-08-19）。注：部署新版后所有用户需重新登录；配置文件权限由 0644 收紧为 0600。

## P1 高危

### 2. OAuth2 state 固定值且回调不校验（登录 CSRF / 授权码注入）

- [x] 位置：`internal/web/handlers/auth.go`（原硬编码 `mailgo_oauth2_state`、`OAuth2Callback` 不校验 state）
- 现状：当前部署未启用 OAuth2，属休眠漏洞，启用前必须修复。
- 修复方案：
  - [x] `OAuth2Start` 用 `crypto/rand` 生成 16 字节随机 state，写入独立的短期 SameSite=Lax cookie（`mail_go_oauth2_state`，10 分钟过期，HttpOnly+Secure）。注意主会话 cookie 是 SameSite=Strict，跨站回调导航不会携带，故不能放主会话。
  - [x] `OAuth2Callback` 读取 `c.Query("state")` 与 cookie 值做 `subtle.ConstantTimeCompare` 比对，缺失/不匹配返回 403。
  - [x] 比对后立即清除 cookie（`MaxAge=-1`），保证一次性使用。
- 验证：
  - [x] 单测：state 缺失/不匹配/无 cookie 均 403；start 设置的 cookie 与 URL state 一致且每次不同；有效 state 通过校验进入后续流程（`oauth2_state_test.go`）。
  - [ ] 手工走完一次 OAuth2 流程（Google/GitHub）确认正常登录。

### 3. Gin 信任所有代理，`ClientIP()` 可伪造（封禁绕过 / 爆破）

- [x] 位置：`internal/web/server.go`（未调用 `SetTrustedProxies`）
- 现状：gin 默认信任 0.0.0.0/0，`X-Forwarded-For` 可任意伪造。线上 8080 端口当前被防火墙挡住，属纵深防御缺失；一旦 8080/socket 可达：伪造不同 IP 即可绕过登录失败封禁无限爆破，也可恶意封禁任意 IP 造成 DoS。
- 修复方案：
  - [x] 统一 `engine.SetTrustedProxies([]string{"127.0.0.1", "::1"})`：外部直连时 XFF 完全不可信（防伪造/防封禁污染）；本机 Caddy/Nginx 转发时 XFF 仍可信（保留真实客户端 IP）。注意 gin 对 Unix socket 监听无条件信任转发头，socket 必须保持仅本机可达。
  - [ ] install.sh 文档注明：8080 端口必须保持仅本机可达（防火墙/绑定 127.0.0.1）。
- 验证：
  - [x] 单测：外部直连 + 伪造 `X-Forwarded-For` 时封禁记录落在真实 IP 上；回环代理 + XFF 时记录 XFF 中的真实客户端 IP（`trustedproxy_test.go`）。
  - [ ] 线上回归：Caddy 反代路径下管理后台封禁列表仍显示真实客户端 IP。

### 4. Web 写信 CRLF 邮件头注入

- [x] 位置：`internal/web/handlers/mail.go`（原 `to`/`cc`/`subject` 直接拼头、附件文件名拼进 `Content-Disposition`/`Content-Type`）
- 现状：信封收件人经 `ParseAddress` 校验无法注入，但注入的头（如 `Reply-To`）会随邮件存储并外发，可被用于钓鱼。
- 修复方案：
  - [x] 新增 `sanitizeHeaderField`：strip `\r`、`\n`、NUL，应用于 From/To/Cc 头。
  - [x] `subject` 经 `sanitizeHeaderField` + RFC 2047（`mime.QEncoding`）编码非 ASCII 内容。
  - [x] 附件名经 `mime.FormatMediaType` 生成 `Content-Disposition`/`Content-Type name` 参数（RFC 2231 编码，中和 CRLF 注入）。
  - [x] 消息构建抽出为 `buildOutgoingMessage` 纯函数（可单测）；`DownloadAttachment`/`AdminDownloadAttachment` 的响应头同步改用 `formatContentDisposition`。
- 验证：
  - [x] 单测：`to`/`cc`/`subject` 携带 CRLF 注入载荷时 RawData 无独立注入头；文件名含 CRLF/引号时头结构完好；非 ASCII 主题正确编码（`mail_injection_test.go`）。
  - [ ] 含特殊字符附件名的邮件实测收发正常。

## P2 中危

### 5. 会话 Cookie 缺 Secure 标志

- [x] 位置：`internal/web/server.go`
- 修复方案：
  - [x] `sessions.Options` 增加 `Secure: cfg.CookieSecure`；新增配置项 `[web].cookie_secure`（默认 true，仅本地 HTTP 调试时改 false；缺失字段按默认 true 处理，参照 relay_starttls 的原始文件检查）。
  - [x] 修正 SameSite 注释（3 = Strict）。
  - [x] 测试：会话 cookie 断言 HttpOnly+Secure+SameSite=Strict。
- 验证：
  - [x] 测试断言 cookie 标志。
  - [ ] 线上登录后检查 `Set-Cookie` 包含 `Secure; HttpOnly; SameSite=Strict`。

### 6. SMTP/IMAP/POP3 认证无速率限制

- [x] 位置：`internal/smtp_server/server.go`、`internal/imap_server/`、`internal/pop3_server/server.go`
- 修复方案：
  - [x] `store.RecordAuthFailure(ip, maxFail, minutes)`：认证失败计数复用 BanStore，达到 `ban.max_fail_attempts` 阈值即封禁 `ban.ban_duration_min` 分钟（与 Web 登录共用封禁记录）。
  - [x] SMTP：`NewSession` 记录 `c.Conn().RemoteAddr()` 提取 IP；Auth 回调失败计数 + 封禁 IP 拒绝认证。
  - [x] IMAP：`Login(connInfo,...)` 从 `connInfo.RemoteAddr` 取 IP；失败计数 + 封禁拒绝。
  - [x] POP3：`handleConn` 开头检查封禁直接拒绝；`handlePASS` 失败计数。
  - [x] 三个服务器构造函数注入 `config.BanConfig`。
- 验证：
  - [x] store 层单测：达到阈值封禁、空 IP 无副作用、与 Web 共用封禁记录（`auth_guard_test.go`）。
  - [ ] 线上用错误密码连续尝试触发封禁后，SMTP/IMAP/POP3 认证被拒。

### 7. 附件存储路径遍历防护无效

- [x] 位置：`internal/storage/attachment.go`
- 修复方案：
  - [x] `FullPath` 改为白名单校验（UUID 文件名正则），非法路径返回错误；兜底校验最终路径仍在 baseDir 内。
  - [x] `Save` 的扩展名白名单化（`safeExt`，丢弃 CR/LF、路径分隔符等）。
- 验证：
  - [x] 单测：`../`、绝对路径、Windows 分隔符、空路径、注入文件名全部拒绝；合法文件名正常读写删（`attachment_test.go`）。

### 8. 默认管理员 admin@example.com/admin

- [x] 位置：`main.go`（`ensureAdminUser`）
- 修复方案：
  - [x] 初始密码改为：环境变量 `MAILGO_ADMIN_PASSWORD` 显式指定，否则生成 16 位随机密码打印一次。
  - [x] User 模型新增 `MustChangePassword`：初始管理员、管理员重置密码的用户在登录后强制跳转设置页改密，改密后清除标记（`UpdatePassword` 顺带清除）。
  - [x] AuthMiddleware 拦截（除 /settings、/logout），settings 页显示提示横幅。
- 验证：
  - [x] 全新数据库启动后 admin/admin 无法登录（密码为随机值）；登录后强制改密流程生效。
  - [ ] 线上验证新装机流程。

### 9. Smarthost 中继 TLS 不验证证书（凭据可被 MITM 截获）

- [x] 位置：`internal/outbound/mailer.go`
- 修复方案：
  - [x] 直投 MX 保持机会式 TLS（`InsecureSkipVerify=true`，业界常规）；relay 路径默认验证证书（`InsecureSkipVerify=false`），IP literal 时以 IP 作为 ServerName 校验 IP SAN。
  - [x] 新增配置 `outbound.relay_tls_insecure`（默认 false），供自签证书内网中继显式放行。
- 验证：
  - [x] 集成测试：自签证书 STARTTLS 中继默认握手失败（certificate 错误）、开启开关后完整 SMTP 流程成功（`mailer_test.go` 两个新测试）。

### 10. 缺安全响应头（点击劫持/降级风险）

- [x] 位置：`internal/web/middleware/security.go`（新中间件，全局注册）
- 修复方案：
  - [x] `Strict-Transport-Security: max-age=31536000; includeSubDomains`
  - [x] `X-Frame-Options: DENY` + CSP `frame-ancestors 'none'`（点击劫持）
  - [x] `X-Content-Type-Options: nosniff`
  - [x] `Referrer-Policy: strict-origin-when-cross-origin`
  - [x] 基础 CSP：`default-src 'self'` + 放宽项（内联脚本/样式必须 unsafe-inline；`img-src https:` 允许邮件远程图片；`connect-src 'self'`/`form-action 'self'` 防数据外泄）。CSP 具体策略在 `security.go` 顶部注释说明。
- 验证：
  - [x] 单测：5 个头均存在，关键值抽查（`security_test.go`）。
  - [ ] 线上回归：登录/收件箱/管理页功能不受 CSP 影响；邮件远程图片正常加载。

### 11. LDAP/OAuth 错误信息泄露与用户枚举

- [x] 位置：`internal/web/handlers/auth.go`
- 修复方案：
  - [x] 错误提示统一为通用文案（"LDAP 认证失败…"、"LDAP 账号未接入本系统…"、"OAuth2 认证失败…"），原始 err 只写日志，不回显页面；不再在提示中回显用户邮箱。
- 验证：
  - [x] 现有 OAuth2 测试仍通过（错误页文案不含内部细节）。
  - [ ] 线上（启用 LDAP/OAuth 后）验证失败页面不含内部地址/DN/原始错误串。

## P3 低危 / 加固

### 12. Referer 开放重定向

- [ ] 位置：`internal/web/handlers/mail.go:567-571、591-595`
- [ ] 修复：仅接受以 `/` 开头且非 `//` 的相对路径 Referer，否则回退 `/inbox`。

### 13. Web 发信配额检查 TOCTOU

- [ ] 位置：`internal/web/handlers/mail.go:209-263`
- [ ] 修复：配额检查与 `UpdateUsedBytes` 改为单条原子 SQL（`WHERE used_bytes + ? <= quota_bytes` 式更新），失败即拒发。

### 14. compose 页 safeJS 在 JS 上下文绕过转义（自 XSS）

- [ ] 位置：`internal/web/templates/compose.html:80`
- [ ] 修复：改为 `quill.root.innerHTML = {{.bodyContent | jsonify}};`（模板函数内用 `json.Marshal` 输出 JS 字符串字面量）。
- [ ] 顺手评估移除 `templateFuncs` 中不再使用的 `safeHTML`，缩小危险面。

### 15. Content-Disposition 文件名未编码

- [ ] 位置：`internal/web/handlers/mail.go:626`、`internal/web/handlers/admin.go:863`
- [ ] 修复：与 #4 一并改用 `mime.FormatMediaType`（RFC 5987 `filename*=`）。

### 16. 会话治理

- [ ] 登录成功后调用 `session.Clear()` 再写入新值（清掉可能的旧状态）。
- [ ] 会话固定时长 24h 无任何续期/空闲过期策略，考虑加滑动过期与绝对过期。

## 已确认安全、无需改动

- bcrypt 密码哈希；GORM 全参数化查询（无 SQL 注入）。
- SMTP 非开放中继、认证用户强制 From=登录身份。
- LDAP 过滤器已 `EscapeFilter`。
- 邮件 HTML 经 sandbox iframe（无 `allow-scripts`）渲染，`srcdoc` 属性转义经实测有效，无存储型 XSS。
- Web 登录错误提示不区分用户是否存在（无枚举）。

## 修复顺序建议

1. ~~#1（P0）~~ 已完成 2026-08-19
2. ~~#2、#3、#4（P1）~~ 已完成 2026-08-19
3. ~~#5-#11（P2）~~ 已完成 2026-08-19
4. 其余 P3 项随版本迭代
