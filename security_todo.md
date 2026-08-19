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

- [ ] 位置：`internal/web/server.go:177-182`
- 修复方案：
  - [ ] `sessions.Options` 增加 `Secure: true`。
  - [ ] 同时修正注释：当前 `SameSite: 3` 实为 Strict 而非注释所写的 Lax。
  - [ ] （可选）新增配置项允许本地 HTTP 调试时关闭 Secure。
- 验证：线上登录后检查 `Set-Cookie` 包含 `Secure; HttpOnly; SameSite=Strict`。

### 6. SMTP/IMAP/POP3 认证无速率限制

- [ ] 位置：`internal/smtp_server/server.go`、`internal/imap_server/`、`internal/pop3_server/server.go`
- 修复方案：
  - [ ] 认证失败计数复用 `BanStore`（按 `RemoteAddr` 提取 IP 记录 fail/ban）。
  - [ ] go-smtp 可通过 `AuthHandler` 包一层计数；IMAP/POP3 在各自登录入口计数。
  - [ ] 达到 `ban.max_fail_attempts` 后直接拒绝连接（SMTP 返回 421，IMAP/POP3 断开）。
- 验证：连续输错 N 次密码后，后续 AUTH 尝试被拒绝且管理后台封禁列表出现对应记录。

### 7. 附件存储路径遍历防护无效

- [ ] 位置：`internal/storage/attachment.go:62-68`
- 现状：`filepath.Clean("../../x")` 仍以 `..` 开头，`TrimPrefix` 只剥离一层 `../`；`../../../etc/passwd` 清洗后仍可逃逸。路径来自 DB，需配合 SQL 写权限才可利用，属纵深防御缺陷。
- 修复方案：
  - [ ] 改为白名单校验：`cleanRel` 必须匹配 `^[a-f0-9-]{36}(\.[A-Za-z0-9.]+)?$`（uuid 命名格式），否则返回错误。
  - [ ] 兜底再校验 `strings.HasPrefix(fullPath, s.baseDir + string(os.PathSeparator))`。
- 验证：单测覆盖 `../`、`..\`（Windows）、绝对路径、符号链接名等用例，均应拒绝。

### 8. 默认管理员 admin@example.com/admin

- [ ] 位置：`main.go:308-358`（`ensureAdminUser`）
- 现状：线上实测默认凭据**未生效**（管理员已修改），但新装机仍存在默认口令窗口。
- 修复方案：
  - [ ] 首次启动生成 16 位随机密码，打印一次并要求首次登录强制修改（User 模型加 `MustChangePassword bool`）。
  - [ ] 或支持环境变量 `MAILGO_ADMIN_PASSWORD` 由部署者显式指定。
- 验证：全新数据库启动后，用 admin/admin 无法登录。

### 9. Smarthost 中继 TLS 不验证证书（凭据可被 MITM 截获）

- [ ] 位置：`internal/outbound/mailer.go:357-366`（`InsecureSkipVerify: true`）
- 修复方案：
  - [ ] 区分两条路径：直投 MX 保持机会式 TLS（不验证，业界常规）；relay 配置了用户名密码时默认验证证书（`ServerName` + 可选 `relay_tls_ca` pin 根证书），提供 `relay_tls_insecure` 开关逃生。
- 验证：对自签证书 relay 测试：默认握手失败，开启开关后成功。

### 10. 缺安全响应头（点击劫持/降级风险）

- [ ] 位置：Caddy 层或 `internal/web/server.go` 全局中间件
- 修复方案（推荐 Caddy 统一加）：
  - [ ] `Strict-Transport-Security: max-age=31536000; includeSubDomains`
  - [ ] `X-Frame-Options: DENY`（或 CSP `frame-ancestors 'none'`）
  - [ ] `X-Content-Type-Options: nosniff`
  - [ ] `Referrer-Policy: strict-origin-when-cross-origin`
  - [ ] 基础 CSP（注意 Gmail/管理页内联脚本较多，先从 `default-src 'self'` + 按需放宽起步）
- 验证：`curl -sD - https://mail.lmve.net/login` 检查各头存在。

### 11. LDAP/OAuth 错误信息泄露与用户枚举

- [ ] 位置：`internal/web/handlers/auth.go:170,182,269`
- 修复方案：
  - [ ] 错误提示统一为“认证失败”，内部细节只写日志，原始 `err` 不回显页面。
  - [ ] “用户 %s 在系统中不存在”改为与密码错误相同的提示。
- 验证：LDAP/OAuth 登录失败时页面不含内部地址、DN、原始错误串。

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
3. #5、#10（Cookie/安全头，部署层加固，改动小）
4. #6、#7、#9（协议与存储层）
5. 其余 P3 项随版本迭代
