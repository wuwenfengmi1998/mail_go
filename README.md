# MailGo

> English | [中文](README_cn.md)

A lightweight mail system written in Go, integrating SMTP / IMAP / POP3 protocol servers and a web admin interface.
Web frontend layout: top navigation + left folder sidebar + mail list (three-column design).

## Features

- **Mail protocols**: SMTP (send), IMAP (sync), POP3 (receive), all with TLS support
- **External delivery**: authenticated users can send mail to external addresses (Gmail/Outlook, etc.), with a built-in outbound queue, **concurrent worker pool delivery** (default 4 workers + max 2 concurrent connections per recipient domain), direct MX delivery, STARTTLS, exponential backoff retry, bounce notifications and DKIM signing
- **Multi-language UI (i18n)**: Chinese / English / Japanese, selectable in user settings (or auto-follow the browser language via Accept-Language; English is the fallback), covering all user and admin pages, 12-hour time formats (AM/PM) and error messages
- **Web mail**: folder data-driven (Web and IMAP share the same MailboxService — whatever IMAP LIST returns is what the UI shows), system folders (Inbox / Sent / Drafts / Trash) plus custom folder management, delete moves to Trash (with restore / permanent delete / empty Trash), unread badges and search filters, select-all / batch delete, sender avatars, rich text editing (Quill.js), attachment upload/download
- **Admin console**: domain management, user management, automatic DKIM key generation, DNS configuration hints, full mail browsing, outbound queue management, IP ban management, dashboard statistics, all timestamps displayed as 12-hour format (AM/PM) in the web timezone
- **Protocol call logs**: every SMTP/IMAP/POP3 connection automatically records source IP, username, success/failure, failure reason and operation summary; filterable by protocol/status/IP/user/time for analyzing password brute-force, relay abuse and other attacks (kept 30 days by default, auto-cleaned)
- **IMAP new-mail push**: real-time push after local delivery (SMTP/Web compose) — clients hanging on IDLE receive new-mail notifications instantly (no polling); read/star/delete changes made by other clients are also synced in real time (IMAP STORE/EXPUNGE, POP3 delete, Web mark-read/delete)
- **Live connection monitoring**: admin console shows active SMTP/IMAP/POP3 connections (source IP, username, TLS, duration), auto-refreshing every 5 seconds; "Disconnect and ban" one-click bans all connections of that IP (ban for 180 days, unban anytime)
- **External auth**: OAuth2 (Google / GitHub), LDAP (optional, disabled by default)
- **Security**: BCrypt password hashing, automatic IP ban on login failures (**tiered bans**: the first 3 times reaching the failure threshold only count without banning, from the 4th time on it bans with escalating durations 30 min → 3 h → 3 months → 6 months, cleared on successful login; **no grace for enumeration attacks**: failures with a non-existent username skip the grace period and are banned on first trigger), outbound rate limiting (anti-abuse), relay denied to unauthenticated users (anti-open-relay), admins can unban
- **Multiple databases**: SQLite by default, switchable to MySQL
- **Cross-platform**: Linux production deployment + Windows local debugging

## Screenshots

| Inbox | Mail view |
|-------|-----------|
| ![Inbox](docs/screenshots/inbox.png) | ![Mail view](docs/screenshots/view.png) |

| Compose | Settings | Login |
|---------|----------|-------|
| ![Compose](docs/screenshots/compose.png) | ![Settings](docs/screenshots/settings.png) | ![Login](docs/screenshots/login.png) |

> Screenshots are rendered with demo data; the actual UI may differ from your deployment.

## Quick Start

### Build

```bash
go build -o mailgo .
```

### Start

```bash
./mailgo
```

On first start the config file and database are created automatically, along with the default admin account:

| Item | Value |
|------|-------|
| Email | `admin@example.com` |
| Initial password | Randomly generated (16 chars, digits + upper/lowercase letters), printed once in the startup log; or pre-set via the `MAILGO_ADMIN_PASSWORD` environment variable |

> ⚠️ This account is flagged "must change password on first login" — change it immediately on the Settings page.

### Access

| Page | URL |
|------|-----|
| User mailbox | `http://localhost:8080/` |
| Admin console | `http://localhost:8080/admin` |

---

## Configuration File

Config file path (TOML format):

| System | Path |
|--------|------|
| Linux | `/etc/mail_go/mail_go.toml` |
| Windows | `./win/etc/mail_go/mail_go.toml` |

Auto-generated on first start; missing fields are auto-filled. A restart is required for changes to take effect.

### Full Configuration Reference

```toml
[database]
driver = "sqlite"                          # sqlite | mysql
dsn = "/srv/mail_go/mail.db"               # SQLite: file path; MySQL: DSN string

[storage]
base_dir = "/srv/mail_go"                  # data root directory
attach_dir = "/srv/mail_go/attachments"    # attachment storage directory

[web]
addr = ":8080"                             # listen address; TCP port or Unix socket
secret_key = ""                            # web session signing key; if empty, a random
                                           # key is generated on first start and written
                                           # to this file (back it up: leaking it allows
                                           # forging sessions, losing it invalidates all)
cookie_secure = true                       # session cookie sent over HTTPS only (Secure
                                           # flag); set to false only for local HTTP debug
protocol_log_keep_days = 30                # retention days for SMTP/IMAP/POP3 protocol
                                           # call logs; expired entries are cleaned up by a
                                           # background task; 0 disables cleanup

[smtp]
addr = ":25"                               # SMTP plaintext port
tls_addr = ":465"                          # SMTPS port (requires TLS certs)
domain = "example.com"                     # mail domain
tls_cert = ""                              # TLS certificate path (empty = TLS disabled)
tls_key = ""                               # TLS private key path
max_message_bytes = 67108864               # max 64MB per message

[imap]
addr = ":143"                              # IMAP plaintext port
tls_addr = ":993"                          # IMAPS port
tls_cert = ""
tls_key = ""

[pop3]
addr = ":110"                              # POP3 plaintext port
tls_addr = ":995"                          # POP3S port
tls_cert = ""
tls_key = ""

[auth]
oauth2_enabled = false                     # enable OAuth2 login
oauth2_provider = ""                       # google | github
oauth2_client_id = ""
oauth2_client_secret = ""
oauth2_redirect_url = ""
ldap_enabled = false                       # enable LDAP login
ldap_server = ""                           # e.g. ldap://localhost:389
ldap_bind_dn = ""                          # e.g. cn=admin,dc=example,dc=com
ldap_bind_password = ""
ldap_search_base = ""                      # e.g. ou=users,dc=example,dc=com
ldap_search_filter = ""                    # e.g. (uid=%s)
ldap_use_tls = false

[ban]
max_fail_attempts = 5                      # login failure threshold
ban_duration_min = 30                      # 1st ban duration (minutes); then escalates:
                                           # 2nd 3h → 3rd 3 months → 4th+ 6 months (cap)
                                           # first 3 threshold hits only count, no ban;
                                           # cleared on successful login

[caddy]
data_dir = ""                              # Caddy data directory (the one containing
                                           # certificates/), used for one-click cert import
                                           # in the admin console; auto-detected when empty
                                           # (e.g. /var/lib/caddy/.local/share/caddy)

[outbound]
hostname = ""                              # EHLO hostname; defaults to [smtp] domain
poll_interval = 15                         # outbound queue scan interval (seconds)
workers = 4                                # concurrent delivery workers (parallel sends,
                                           # improves throughput for bulk mail; 0/1 serial)
batch_size = 50                            # max messages picked per scan
max_concurrent_per_domain = 2              # max concurrent connections to one recipient
                                           # domain (or relay), prevents spam-lookalike
                                           # bursts; 0 = unlimited
max_attempts = 12                          # max delivery attempts per message
retry_base_min = 5                         # retry backoff base (minutes), exponential:
                                           # 5/10/20/40...
max_recipients = 50                        # max external recipients per message
max_per_min = 30                           # max outgoing messages per user per minute
max_per_day = 500                          # max outgoing messages per user per day;
                                           # 0 disables external delivery
connect_timeout = 30                       # remote MX connect timeout (seconds)
relay_host = ""                            # smarthost; empty = direct MX delivery
relay_port = 587                           # 465 = implicit TLS, other ports use STARTTLS
relay_user = ""                            # relay auth username (AUTH PLAIN)
relay_password = ""                        # relay auth password
relay_starttls = true                      # use STARTTLS on non-465 ports
relay_tls_insecure = false                 # skip relay TLS certificate verification; certs
                                           # are verified by default (protects relay
                                           # credentials); only set to true for self-signed
                                           # intranet relays when you accept the risk
ip_family = "ipv4"                         # outbound address family: ipv4 (default) | ipv6 | auto
source_ip = ""                             # outbound source IP binding (e.g. static IPv6);
                                           # empty = kernel picks
```

---

## Common Configuration Scenarios

### 1. Switching to MySQL

```toml
[database]
driver = "mysql"
dsn = "mailgo:YourPassword@tcp(127.0.0.1:3306)/mailgo?charset=utf8mb4&parseTime=True&loc=Local"
```

MySQL DSN format: `user:password@tcp(host:port)/dbname?params`

> The database and user must be created in MySQL first:
> ```sql
> CREATE DATABASE mailgo CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
> CREATE USER 'mailgo'@'localhost' IDENTIFIED BY 'YourPassword';
> GRANT ALL PRIVILEGES ON mailgo.* TO 'mailgo'@'localhost';
> FLUSH PRIVILEGES;
> ```

### 2. Web over Unix Socket

```toml
[web]
addr = "/run/mail_go/web.sock"
```

When `addr` starts with `/`, Gin listens on a Unix socket automatically.

### 3. Setting the Session Key (Container / Multi-Instance)

The session signing key can be overridden via the `MAILGO_SECRET_KEY` environment variable (takes precedence over the config file and is never written to disk):

```bash
MAILGO_SECRET_KEY="$(openssl rand -hex 32)" mail_go
```

Requirement: at least 16 bytes; when empty, the value from the config file is used (auto-generated on first start). After changing the key, all logged-in sessions are invalidated immediately and users must log in again.

Nginx reverse proxy configuration:

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

> Make sure the socket directory exists and is writable:
> ```bash
> mkdir -p /run/mail_go
> chown mailgo:mailgo /run/mail_go
> ```

### 4. Enabling TLS Encryption

Enable TLS for SMTP/IMAP/POP3 (all three protocols can share one certificate, or be configured separately):

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

Once TLS is configured, the corresponding encrypted ports (465/993/995) start automatically. Checking the TLS box when adding a domain in the admin console switches the ports automatically.

> You can get free certificates from Let's Encrypt:
> ```bash
> certbot certonly --standalone -d mail.example.com
> # Certificate: /etc/letsencrypt/live/mail.example.com/fullchain.pem
> # Private key: /etc/letsencrypt/live/mail.example.com/privkey.pem
> ```

#### One-Click Certificate Import from Caddy

If this machine already serves HTTPS for the domain via [Caddy](https://caddyserver.com/) (Caddy issues and renews certificates automatically),
you can click **"Fetch certificate from Caddy"** on the **Domain Management → Edit Domain** page in the admin console
to import the certificate and private key from Caddy's storage into the mail server (automatically enabling TLS for that domain), without manually copying PEM files.
Wildcard certificates are supported (e.g. `*.example.com` matches `mail.example.com`).
Certificates support **hot reload**: they take effect immediately after import (or manual upload) without restarting the service — SMTP/IMAP/POP3
re-check and reload changed certificate files on every TLS handshake.

Because Caddy's certificate directory is only readable by the `caddy` user, install.sh installs a root-privileged certificate sync job
(`mailgo-caddy-sync.{path,timer}`) that mirrors Caddy's certificate tree to `/srv/mail_go/tls/caddy`,
so mail_go can always read it after renewals; an ACL grant is also applied as a fallback for direct reads.
This is configured automatically during installation, or can be run manually:

```bash
sudo ./install.sh setup-caddy-cert          # auto-detect Caddy data dir, configure sync + ACL
sudo ./install.sh setup-caddy-cert /path/to/caddy/data   # or specify the data dir manually
```

If Caddy's data directory is not in a common location, specify it explicitly in the config file:

```toml
[caddy]
data_dir = "/var/lib/caddy/.local/share/caddy"
```

### 5. Enabling OAuth2 Login (Google Example)

```toml
[auth]
oauth2_enabled = true
oauth2_provider = "google"
oauth2_client_id = "your-client-id.apps.googleusercontent.com"
oauth2_client_secret = "your-client-secret"
oauth2_redirect_url = "https://mail.example.com/auth/oauth2/callback"
```

> You need to create an OAuth 2.0 client in the [Google Cloud Console](https://console.cloud.google.com/) and set the authorized redirect URI to `https://your-domain/auth/oauth2/callback`.

### 6. Enabling LDAP Authentication

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

### 7. Sending Mail to External Recipients (External Delivery)

Authenticated users (Web mail / SMTP submission) can send mail to external addresses. The system
delivers directly via the recipient domain's MX records, handling STARTTLS and exponential backoff
retries automatically, and DKIM-signs outgoing mail (using the key generated in the admin domain management).

```toml
[smtp]
domain = "example.com"        # must be the real mail domain (EHLO/bounce address)

[outbound]
hostname = "mail.example.com" # recommended to match the mail hostname
poll_interval = 15
max_attempts = 12
max_per_day = 500             # set to 0 to disable external delivery entirely
```

Required DNS / server prerequisites (see `todo.md` and the admin DNS hints page for details):

| Item | Description |
|------|-------------|
| MX | `example.com MX 10 mail.example.com` |
| SPF | `example.com TXT "v=spf1 mx -all"` |
| DKIM | `default._domainkey.example.com TXT "v=DKIM1; k=rsa; p=<public key>"` (auto-generated in the admin console) |
| DMARC | `_dmarc.example.com TXT "v=DMARC1; p=none; rua=mailto:postmaster@example.com"` |
| PTR | Server IP reverse-resolves to the mail hostname (request from your hosting provider) |
| Network | Port 25 outbound is not blocked by the cloud provider |

Security policy: external recipients are only accepted from authenticated users; `MAIL FROM` must match
the logged-in user; per-user per-minute/day outbound limits apply; failed mail bounces back to the
sender's inbox; admins can view delivery status in the **Outbound Queue** and manually retry or cancel.

> **IPv4/IPv6**: Only IPv4 outbound is used by default (`ip_family = "ipv4"`), because many recipients
> (e.g. Gmail) reject IPv6 addresses without PTR, while IPv4 usually has matching forward/reverse PTR.
> To use IPv6: ask your provider to configure PTR for the static address (pointing to `mail.example.com`),
> then set `ip_family = "ipv6"` and bind `source_ip` to that static address
> (to avoid the kernel picking rotating temporary privacy addresses).

### 8. Relaying Outbound Mail Through a Smarthost

When the server IP is on a residential/dynamic IP range, it is often listed in policy lists such as Spamhaus PBL,
and recipients like Microsoft (Outlook/Hotmail) reject the mail outright. In that case, it is recommended to hand
outbound mail to a third-party SMTP relay (Mailgun / SendGrid / Amazon SES / Aliyun Direct Mail, etc.),
configured in `[outbound]` — all external delivery automatically routes through the relay:

```toml
[outbound]
relay_host = "smtp.example-relay.com"
relay_port = 587          # 465 = implicit TLS
relay_user = "your-api-user"
relay_password = "your-api-key"
relay_starttls = true
```

The relay uses AUTH PLAIN authentication; local recipients still use local delivery, unaffected.

### 9. Setting the Initial Admin Password

When the admin account is created on first start, the initial password is randomly generated by default
and printed once in the startup log; to pre-set it (e.g. for deployment scripts), use the `MAILGO_ADMIN_PASSWORD`
environment variable:

```bash
MAILGO_ADMIN_PASSWORD="$(openssl rand -base64 12)" ./mailgo
```

This variable only takes effect when the admin account is first created (ignored if the account already exists);
either way, the password must be changed on first login.

---

## Port Quick Reference

| Protocol | Plaintext port | TLS port | Description |
|----------|----------------|----------|-------------|
| SMTP | 25 | 465 | Mail sending |
| IMAP | 143 | 993 | Mailbox sync |
| POP3 | 110 | 995 | Mail retrieval |
| Web | 8080 | — | Web UI (Unix socket supported) |

---

## Directory Structure

```
mailgo/
├── main.go                          # program entry
├── config/
│   ├── config.go                    # config loading and merging
│   └── defaults.go                  # default constants
├── internal/
│   ├── db/
│   │   ├── db.go                    # database initialization (SQLite/MySQL)
│   │   └── models.go                # GORM model definitions
│   ├── store/
│   │   ├── stores.go                # Store aggregator
│   │   ├── user_store.go            # user data operations
│   │   ├── mail_store.go            # mail data operations
│   │   ├── mailbox_store.go         # folder (IMAP mailbox) data operations
│   │   ├── domain_store.go          # domain data operations
│   │   ├── attachment_store.go      # attachment data operations
│   │   ├── outbound_store.go        # outbound queue data operations
│   │   ├── ban_store.go             # ban data operations
│   │   └── protocol_log_store.go    # protocol call log data operations
│   ├── smtp_server/server.go        # SMTP server
│   ├── outbound/
│   │   ├── mailer.go                # MX lookup and SMTP outbound client
│   │   ├── manager.go               # outbound queue, retry, rate limiting, bounces
│   │   └── sign.go                  # DKIM signing
│   ├── imap_server/
│   │   ├── server.go                # IMAP server (listener/capabilities/cross-session push)
│   │   ├── service.go               # MailboxService (shared by IMAP sessions and Web)
│   │   └── session.go               # IMAP Session implementation (SELECT/FETCH/STORE/EXPUNGE)
│   ├── pop3_server/server.go        # POP3 server
│   ├── connhub/hub.go               # protocol connection registry (live connection monitoring)
│   ├── storage/attachment.go        # attachment file storage
│   ├── dkim/keys.go                 # DKIM key generation
│   ├── auth/
│   │   ├── provider.go              # auth interface
│   │   ├── oauth2.go                # OAuth2 authentication
│   │   └── ldap.go                  # LDAP authentication
│   └── web/
│       ├── server.go                # web routing and template loading
│       ├── handlers/
│       │   ├── auth.go              # login/logout/OAuth2/LDAP
│       │   ├── mail.go              # mailbox operations
│       │   └── admin.go             # admin console
│       ├── middleware/
│       │   ├── auth.go              # session authentication
│       │   ├── admin.go             # admin authorization
│       │   └── ban.go               # IP ban check
│       └── templates/               # HTML templates
│           ├── *.html               # user pages
│           └── admin/*.html         # admin pages
└── .gitignore
```

### Runtime Data Directories (Linux)

| Path | Description |
|------|-------------|
| `/etc/mail_go/` | config file |
| `/srv/mail_go/` | database + attachment storage |

### Runtime Data Directories (Windows Debug)

| Path | Description |
|------|-------------|
| `./win/etc/mail_go/` | config file |
| `./win/srv/mail_go/` | database + attachment storage |

---

## Linux Service Deployment

### systemd Unit File

Create `/etc/systemd/system/mailgo.service`:

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

# Security hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/srv/mail_go /run/mail_go /etc/mail_go

[Install]
WantedBy=multi-user.target
```

### Starting the Service

```bash
# Create the system user
sudo useradd -r -s /sbin/nologin -d /srv/mail_go mailgo

# Set directory ownership
sudo chown -R mailgo:mailgo /srv/mail_go /etc/mail_go

# Enable and start
sudo systemctl daemon-reload
sudo systemctl enable mailgo
sudo systemctl start mailgo

# View logs
sudo journalctl -u mailgo -f
```

---

## DNS Configuration

The mail service requires the following DNS records (the admin console → DNS hints page shows the full configuration):

| Type | Name | Value | Description |
|------|------|-------|-------------|
| A | mail | server IP | mail server address |
| MX | @ | mail.example.com | mail routing |
| TXT | @ | `v=spf1 mx ~all` | SPF anti-spam |
| TXT | default._domainkey | DKIM public key (auto-generated in the admin console) | DKIM signature verification |
| TXT | _dmarc | `v=DMARC1; p=none; rua=mailto:admin@example.com` | DMARC policy |

---

## Tech Stack

| Component | Technology |
|-----------|------------|
| Language | Go 1.25+ |
| Web framework | Gin |
| Template engine | html/template |
| ORM | GORM |
| Database | SQLite (default) / MySQL |
| Config format | TOML |
| SMTP | github.com/emersion/go-smtp |
| IMAP | github.com/emersion/go-imap/v2 |
| POP3 | hand-implemented TCP protocol |
| Password hashing | golang.org/x/crypto/bcrypt |
| Rich text | Quill.js (embedded via go:embed, works offline) |
| OAuth2 | golang.org/x/oauth2 |
| LDAP | github.com/go-ldap/ldap/v3 |
| DKIM | RSA 2048 auto-generated |

---

## Default Account

| Role | Email | Initial password | Default quota |
|------|-------|------------------|---------------|
| Admin | admin@example.com | Randomly generated (printed once in the startup log) or set via `MAILGO_ADMIN_PASSWORD` | 5 GB |

> ⚠️ Password change is mandatory on first login (also triggered when an admin resets the password); change it immediately after production deployment.

## License

MIT
