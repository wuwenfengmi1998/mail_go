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

- [x] 位置：`internal/web/handlers/mail.go`（Delete/MarkRead）
- [x] 修复：新增 `safeRedirectPath`——仅接受以 `/` 开头且非 `//` 的同站相对路径，外部 URL/协议跳转一律回退 `/inbox`。
- 验证：
  - [x] 单测：`https://evil.com/`、`//evil.com`、`javascript:` 等拒绝，相对路径放行（`TestSafeRedirectPath`）。

### 13. Web 发信配额检查 TOCTOU

- [x] 位置：`internal/web/handlers/mail.go`、`internal/store/user_store.go`
- [x] 修复：新增 `UserStore.TryReserveQuota`（单条原子 SQL `UPDATE ... WHERE used_bytes + ? <= quota_bytes`），DoSend 先原子预扣全部附件大小，超配额即拒发；后续读取/保存/落库失败的附件按大小补偿回退。
- 验证：
  - [x] 单测：预扣到配额上限、超额拒绝且不部分扣费、释放后可再扣、非正 delta 拒绝（`TestTryReserveQuota*`）。

### 14. compose 页 safeJS 在 JS 上下文绕过转义（自 XSS）

- [x] 位置：`internal/web/templates/compose.html`、`internal/web/server.go`
- [x] 修复：新增 `jsonify` 模板函数（`json.Marshal`，默认转义 `< > &` 为 `\u003c` 等，无法逃出 `</script>`）；`quill.root.innerHTML` 改用 `jsonify`。**移除** `templateFuncs` 中危险的 `safeHTML`/`safeJS`；view/admin 模板的 `srcdoc` 改回默认属性转义（行为一致，前已实测）。
- 验证：
  - [x] 单测：`</script>` 载荷不产生裸逃逸、控制字符转义、输出为合法字符串字面量（`TestJsonifyEscapesScriptBreakout`）；全模板渲染测试通过。

### 15. Content-Disposition 文件名未编码

- [x] 位置：`internal/web/handlers/mail.go`、`internal/web/handlers/admin.go`
- [x] 修复：随 P1 #4 一并完成——`formatContentDisposition` 使用 `mime.FormatMediaType`（RFC 2231），两处下载端点均已应用，并有 `TestFormatContentDisposition` 覆盖。

### 16. 会话治理

- [x] 位置：`internal/web/handlers/auth.go`、`internal/web/middleware/auth.go`
- [x] 修复：登录成功（Web/LDAP/OAuth2 三处）先 `session.Clear()` 清旧状态再写入；会话记录 `loginAt`，AuthMiddleware 实施**绝对过期 7 天**（超时强制登出）+ **滑动续期**（活跃会话每 12 小时写回刷新）。
- 验证：
  - [x] 单测：8 天前的会话被重定向登录页；1 小时前的会话正常访问（用配置密钥签名构造会话，`TestSessionAbsoluteExpiryForcesRelogin`/`TestSessionWithinExpiryWorks`）。

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
4. ~~#12-#16（P3）~~ 已完成 2026-08-19
5. ~~#17（P4）、#18（P5，方案 A）~~ 已完成 2026-08-20

## P4 低危：第二轮审计发现（2026-08-20，8ea4a62..37b4816）

### 17. 手动封禁 Create 非 upsert，与阶段性封禁体系数据错位

- [x] 位置：`internal/web/handlers/admin.go`（`DisconnectConnection`）、`internal/store/ban_store.go`、`internal/db/models.go`、`internal/db/db.go`
- 现状：`f2493da` 阶段性封禁已改为"每 IP 一条记录 upsert"（`RecordAuthFailure` 内部 GetByIP + Update），但管理员"断开并封禁"仍直接 `Create`。`ip_address` 无唯一索引，当目标 IP 已有失败计数记录时会插入**第二条**记录，造成：
  - `IncrementFail` 用 `First`（默认主键升序）更新**旧行**，`GetByIP` 用 `Order("id DESC")` 返回**新行** -> 自动封禁的档位判定（BanCount）与失败计数（FailCount）读写错行；
  - `UnbanIP` 按 ID 删除一行后另一行仍在，可能出现"解封后仍被旧记录挡住/计数异常"。
- 修复方案：
  - [x] BanStore 新增 `BanIP(ip, reason, duration)`：事务内删除该 IP 全部既有记录（兼容历史脏数据）后插入单条封禁记录，计数清零（与"管理员解封清零"语义一致）；`DisconnectConnection` 改用该方法。
  - [x] `BanEntry.IPAddress` 升级为 `uniqueIndex`；`InitDB` 在 AutoMigrate 前调用 `dedupeBanEntries` 清理历史重复行（保留每 IP 最大 id，SQLite/MySQL 兼容的派生表写法），表不存在时静默。
  - [x] `IncrementFail` 原子化：SQL 侧 `fail_count + 1`，miss 时 `OnConflict DoNothing` 插入兜底并发竞态，回读计数。
- 验证：
  - [x] 单测：已有观察记录的 IP 手动封禁后仅一条、计数清零、封禁生效（`TestBanIPUpsertSingleRow`）。
  - [x] 单测：同 IP 第二条记录被唯一约束拒绝（`TestBanEntryUniqueIndex`）。
  - [x] 并发单测（`-race`）：16 协程并发 IncrementFail 计数精确无重复行（`TestIncrementFailConcurrent`）。
  - [x] db 包单测：旧表重复行清理保留最大 id、表不存在静默（`dedupe_test.go`）。

## P5 备注：产品权衡项（需决策后实施）

### 18. 阶段性封禁"前 3 次触发不封禁"降低爆破门槛

- [x] 位置：`internal/store/auth_guard.go`（`RecordAuthFailure`）、`internal/store/user_store.go`（`LoginExists`）、Web/LDAP/SMTP/IMAP/POP3 五处调用点
- 现状：为防误封手机客户端（配置向导探测、裸用户名重试等），达到失败阈值记为一次触发，前 3 次**只计数不封禁**。副作用：攻击者每次触发前可"免费"尝试 `max_fail_attempts`（默认 5）次，即约 **15 次失败尝试零封禁**；第 4 次起才进入 30min -> 3h -> 3 个月 -> 半年的递增档位。长期防护足够，但自动化爆破的起步门槛降低。
- 已实施（方案 A，2026-08-20）：
  - [x] `RecordAuthFailure` 新增 `knownUser bool` 参数：用户名存在（真实用户输错）保留前 3 次宽限；用户名不存在（枚举型爆破）跳过宽限、首次触发即按第 1 档封禁，封禁原因注明"未知用户名，跳过宽限"。
  - [x] 新增 `UserStore.LoginExists(login)`（完整邮箱或裸用户名），五个失败调用点按场景传入：Web 登录查邮箱存在性；SMTP/IMAP/POP3 用登录名查；LDAP 侧存在性无法判定，保守按已知用户处理（防误封）。
- 验证：
  - [x] A：未知用户名第 1 次触发即封（30 分钟，reason 含"未知用户名"）（`TestRecordAuthFailureUnknownUserSkipsGrace`）。
  - [x] 已知用户名前 3 次触发不封、第 4 次封第 1 档（回归）（`TestRecordAuthFailureKnownUserKeepsGrace`）。
  - [x] `LoginExists` 邮箱/裸用户名/不存在/空输入矩阵（`TestLoginExists`）。
- 决策记录：**方案 A**（按失败性质区分宽限：真实用户防误封，枚举爆破即时封禁）——在不改变正常用户体验的前提下，让针对不存在账号的字典爆破首次达到阈值即被封，兼顾误封防护与爆破门槛。

## 部署侧建议（非代码项）

- Caddy 加固（可选，应用层已加安全头）、8080 端口保持仅本机可达。
- GitHub 仓库中 3 个 50MB+ 的 exe 文件（mailgo.exe / mail_go.exe / mailgo_qa.exe）建议改用 Git LFS 或从历史中删除。
- 线上验证：部署新版后检查登录/收件箱/管理页、协议认证封禁、邮件远程图片加载（CSP 影响）。
