package pop3_server

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/store"
	"mail_go/internal/tlsutil"
)

// POP3Server implements a simple POP3 mail server over TCP.
type POP3Server struct {
	listener  net.Listener
	stores    *store.Stores
	cfg       config.POP3Config
	banCfg    config.BanConfig
	tlsLoader *tlsutil.Loader
	hub       *connhub.Hub
	wg        sync.WaitGroup
}

// NewPOP3Server creates a new POP3 server instance. tlsLoader may be nil
// when TLS is not configured.
func NewPOP3Server(cfg config.POP3Config, stores *store.Stores, tlsLoader *tlsutil.Loader, banCfg config.BanConfig, hub *connhub.Hub) *POP3Server {
	return &POP3Server{stores: stores, cfg: cfg, banCfg: banCfg, tlsLoader: tlsLoader, hub: hub}
}

func (s *POP3Server) tlsConfig() (*tls.Config, error) {
	if s.tlsLoader == nil {
		return nil, fmt.Errorf("POP3 TLS certificate or key not configured")
	}
	// GetCertificate 每次握手按需重载证书，证书更新后无需重启服务
	return &tls.Config{GetCertificate: s.tlsLoader.GetCertificate}, nil
}

// Start starts the POP3 server on the configured plain-text port.
func (s *POP3Server) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("POP3 listen failed: %w", err)
	}
	port := parseAddrPort(s.cfg.Addr)

	log.Printf("POP3 server listening on %s", s.cfg.Addr)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleConn(conn, port)
			}()
		}
	}()

	return nil
}

// StartTLS starts the POP3 server on the configured TLS port.
func (s *POP3Server) StartTLS() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}

	listener, err := tls.Listen("tcp", s.cfg.TLSAddr, tlsConfig)
	if err != nil {
		return fmt.Errorf("POP3 TLS listen failed: %w", err)
	}
	port := parseAddrPort(s.cfg.TLSAddr)

	log.Printf("POP3 TLS server listening on %s", s.cfg.TLSAddr)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleConn(conn, port)
			}()
		}
	}()

	return nil
}

// parseAddrPort 从监听地址解析端口号，失败返回 0。
func parseAddrPort(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return port
}

// handleConn handles a single POP3 client connection.
func (s *POP3Server) handleConn(conn net.Conn, port int) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Minute))

	clientIP := store.ClientIPFromAddr(conn.RemoteAddr())
	startedAt := time.Now()

	// 已封禁 IP 直接拒绝（防协议层暴力破解）
	if banned, _ := s.stores.Bans.IsBanned(clientIP); banned {
		sendResponse(conn, "-ERR access denied")
		s.writeProtocolLog(port, clientIP, "", false, "IP已被封禁", "连接被拒绝", 0, 0, startedAt)
		return
	}

	// 连接追踪：注册到当前连接中心，连接结束时注销
	activeConn := s.hub.Register("pop3", clientIP, port, false)

	// 会话状态（供协议日志汇总）
	var (
		authUser       *db.User
		authUsername   string
		authFailReason string
		commandCount   = make(map[string]int)
		deletedCount   int
	)

	reader := bufio.NewReader(conn)
	var messages []pop3Message
	var deleted map[int]bool
	tlsActive := false

	defer func() {
		if activeConn != nil {
			activeConn.Close()
		}
	}()

	sendResponse(conn, "+OK MailGo POP3 server ready")

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToUpper(parts[0])
		commandCount[cmd]++
		activeConn.Touch()
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}

		authenticated := authUser != nil && authUser.ID != 0
		if !authenticated && requiresAuth(cmd) {
			sendResponse(conn, "-ERR authentication required")
			continue
		}

		switch cmd {
		case "USER":
			authUsername = arg
			authUser, messages, deleted = s.handleUSER(conn, arg, authUser)
		case "PASS":
			authUser, messages, deleted = s.handlePASS(conn, arg, authUser)
			if authUser == nil || authUser.ID == 0 {
				if authFailReason == "" {
					authFailReason = "用户名或密码错误"
				}
			} else {
				authFailReason = ""
				activeConn.SetUser(authUsername)
			}
		case "STAT":
			s.handleSTAT(conn, messages, deleted)
		case "LIST":
			s.handleLIST(conn, arg, messages, deleted)
		case "RETR":
			s.handleRETR(conn, arg, messages, deleted)
		case "DELE":
			s.handleDELE(conn, arg, messages, deleted)
		case "NOOP":
			sendResponse(conn, "+OK")
		case "RSET":
			deleted = make(map[int]bool)
			sendResponse(conn, "+OK")
		case "QUIT":
			deletedCount = s.expungeDeleted(messages, deleted, authUser)
			sendResponse(conn, "+OK MailGo POP3 server signing off")
			s.writeProtocolLog(port, clientIP, authUsername, authUser != nil && authUser.ID != 0, authFailReason,
				pop3CommandDetail(commandCount, deletedCount), deletedCount,
				time.Since(startedAt).Milliseconds(), time.Now())
			return
		case "CAPA":
			s.handleCAPA(conn, tlsActive)
		case "STLS":
			if authenticated {
				sendResponse(conn, "-ERR STLS not allowed after authentication")
				continue
			}
			if tlsActive {
				sendResponse(conn, "-ERR TLS already active")
				continue
			}
			tlsConfig, err := s.tlsConfig()
			if err != nil {
				sendResponse(conn, "-ERR TLS not available")
				continue
			}
			sendResponse(conn, "+OK Begin TLS negotiation")
			tlsConn := tls.Server(conn, tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				s.writeProtocolLog(port, clientIP, authUsername, false, "TLS 握手失败",
					pop3CommandDetail(commandCount, 0), 0, time.Since(startedAt).Milliseconds(), time.Now())
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
			tlsActive = true
			activeConn.SetTLS(true)
		case "TOP":
			s.handleTOP(conn, arg, messages, deleted)
		case "UIDL":
			s.handleUIDL(conn, arg, messages, deleted)
		default:
			sendResponse(conn, "-ERR unknown command")
		}
	}

	// 连接异常结束（未 QUIT）
	success := authUser != nil && authUser.ID != 0 && authFailReason == ""
	if success && authFailReason == "" && authUsername == "" && len(commandCount) == 0 {
		success = true
	}
	s.writeProtocolLog(port, clientIP, authUsername, success, authFailReason,
		pop3CommandDetail(commandCount, 0), 0, time.Since(startedAt).Milliseconds(), time.Now())
}

// pop3CommandDetail 汇总会话中执行的命令为可读摘要（计数，忽略 NOOP/CAPA）。
func pop3CommandDetail(counts map[string]int, deletedCount int) string {
	var parts []string
	for _, c := range []string{"USER", "PASS", "STAT", "LIST", "RETR", "TOP", "UIDL", "DELE", "RSET", "STLS", "QUIT"} {
		n := counts[c]
		if n == 0 {
			continue
		}
		if n == 1 {
			parts = append(parts, c)
		} else {
			parts = append(parts, fmt.Sprintf("%s×%d", c, n))
		}
	}
	if deletedCount > 0 {
		parts = append(parts, fmt.Sprintf("删除%d", deletedCount))
	}
	if len(parts) == 0 {
		return "连接建立，无命令"
	}
	return strings.Join(parts, " ")
}

// writeProtocolLog 写入一条 POP3 协议调用日志。
func (s *POP3Server) writeProtocolLog(port int, ip, username string, success bool, failReason, detail string, msgCount int, durationMs int64, at time.Time) {
	entry := &db.ProtocolLog{
		Protocol:   db.ProtocolPOP3,
		Port:       port,
		ClientIP:   ip,
		Username:   username,
		Success:    success,
		FailReason: failReason,
		Detail:     detail,
		MsgCount:   msgCount,
		DurationMs: durationMs,
		CreatedAt:  at,
	}
	if err := s.stores.ProtocolLogs.Create(entry); err != nil {
		log.Printf("POP3: 写入协议日志失败: %v", err)
	}
}

func requiresAuth(cmd string) bool {
	switch cmd {
	case "STAT", "LIST", "RETR", "DELE", "RSET", "TOP", "UIDL":
		return true
	default:
		return false
	}
}

func (s *POP3Server) handleCAPA(conn net.Conn, tlsActive bool) {
	sendResponse(conn, "+OK Capability list follows")
	sendResponse(conn, "USER")
	sendResponse(conn, "TOP")
	sendResponse(conn, "UIDL")
	sendResponse(conn, "RESP-CODES")
	if !tlsActive && s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		sendResponse(conn, "STLS")
	}
	sendResponse(conn, ".")
}

// pop3Message holds a message and its computed size for POP3.
type pop3Message struct {
	id      uint
	raw     string
	size    int
	message *db.Message
}

// loadMessages loads all INBOX messages for a user.
func (s *POP3Server) loadMessages(user *db.User) []pop3Message {
	if user == nil {
		return nil
	}

	dbMsgs, err := s.stores.Mails.ListAllByUserAndFolder(user.ID, "INBOX")
	if err != nil {
		return nil
	}

	msgs := make([]pop3Message, 0, len(dbMsgs))
	for i := range dbMsgs {
		raw := string(buildRawMessage(&dbMsgs[i]))
		msgs = append(msgs, pop3Message{
			id:      dbMsgs[i].ID,
			raw:     raw,
			size:    len(normalizePOP3Data(raw)),
			message: &dbMsgs[i],
		})
	}
	return msgs
}

// handleUSER processes the USER command.
func (s *POP3Server) handleUSER(conn net.Conn, username string, currentUser *db.User) (*db.User, []pop3Message, map[int]bool) {
	if username == "" {
		sendResponse(conn, "-ERR missing username")
		return currentUser, nil, nil
	}

	user, err := s.stores.Users.GetByEmail(username)
	if err != nil {
		sendResponse(conn, "+OK")
		return &db.User{Username: username}, nil, nil
	}

	// 保留完整的邮箱地址作为登录标识（PASS 阶段用 Authenticate 校验），
	// user.ID 用于后续加载邮件。
	user.Username = username
	sendResponse(conn, "+OK")
	return user, nil, nil
}

// handlePASS processes the PASS command.
func (s *POP3Server) handlePASS(conn net.Conn, password string, user *db.User) (*db.User, []pop3Message, map[int]bool) {
	if user == nil {
		sendResponse(conn, "-ERR no username given")
		return nil, nil, nil
	}

	clientIP := store.ClientIPFromAddr(conn.RemoteAddr())

	authUser, err := s.stores.Users.Authenticate(user.Username, password)
	if err != nil {
		// 认证失败计数，达到阈值按档位封禁（与 Web 登录共用 ban_entries）
		s.stores.RecordAuthFailure(clientIP, s.banCfg.MaxFailAttempts, s.banCfg.BanDurationMin, "邮件协议认证失败次数过多")
		sendResponse(conn, "-ERR authentication failed")
		return nil, nil, nil
	}

	messages := s.loadMessages(authUser)
	deleted := make(map[int]bool)
	sendResponse(conn, fmt.Sprintf("+OK authenticated, %d messages", len(messages)))
	return authUser, messages, deleted
}

// handleSTAT processes the STAT command.
func (s *POP3Server) handleSTAT(conn net.Conn, messages []pop3Message, deleted map[int]bool) {
	count := 0
	totalSize := 0
	for i, msg := range messages {
		if !deleted[i+1] {
			count++
			totalSize += msg.size
		}
	}
	sendResponse(conn, fmt.Sprintf("+OK %d %d", count, totalSize))
}

// handleLIST processes the LIST command (with optional message number).
func (s *POP3Server) handleLIST(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	if arg == "" {
		sendResponse(conn, "+OK message list follows")
		for i, msg := range messages {
			if !deleted[i+1] {
				sendResponse(conn, fmt.Sprintf("%d %d", i+1, msg.size))
			}
		}
		sendResponse(conn, ".")
		return
	}

	num, err := strconv.Atoi(arg)
	if err != nil || num < 1 || num > len(messages) {
		sendResponse(conn, "-ERR no such message")
		return
	}
	if deleted[num] {
		sendResponse(conn, "-ERR message deleted")
		return
	}
	sendResponse(conn, fmt.Sprintf("+OK %d %d", num, messages[num-1].size))
}

// handleRETR processes the RETR command.
func (s *POP3Server) handleRETR(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	num, err := strconv.Atoi(arg)
	if err != nil || num < 1 || num > len(messages) {
		sendResponse(conn, "-ERR no such message")
		return
	}
	if deleted[num] {
		sendResponse(conn, "-ERR message deleted")
		return
	}

	msg := messages[num-1]
	sendResponse(conn, fmt.Sprintf("+OK %d octets", msg.size))
	writeDotStuffed(conn, msg.raw, nil)
	conn.Write([]byte(".\r\n"))
}

// handleDELE processes the DELE command.
func (s *POP3Server) handleDELE(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	num, err := strconv.Atoi(arg)
	if err != nil || num < 1 || num > len(messages) {
		sendResponse(conn, "-ERR no such message")
		return
	}
	if deleted[num] {
		sendResponse(conn, "-ERR message already deleted")
		return
	}
	deleted[num] = true
	sendResponse(conn, "+OK message marked for deletion")
}

// handleTOP processes the TOP command (headers + first N lines of body).
func (s *POP3Server) handleTOP(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	parts := strings.Fields(arg)
	if len(parts) < 1 {
		sendResponse(conn, "-ERR syntax error")
		return
	}

	num, err := strconv.Atoi(parts[0])
	if err != nil || num < 1 || num > len(messages) {
		sendResponse(conn, "-ERR no such message")
		return
	}
	if deleted[num] {
		sendResponse(conn, "-ERR message deleted")
		return
	}

	nLines := 0
	if len(parts) > 1 {
		nLines, _ = strconv.Atoi(parts[1])
	}

	msg := messages[num-1]
	header, body := splitHeaderBody(msg.raw)
	sendResponse(conn, "+OK top of message follows")
	writeDotStuffed(conn, header, nil)
	conn.Write([]byte("\r\n"))
	writeDotStuffed(conn, body, &nLines)
	conn.Write([]byte(".\r\n"))
}

// handleUIDL processes the UIDL command.
func (s *POP3Server) handleUIDL(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	if arg == "" {
		sendResponse(conn, "+OK UIDL list follows")
		for i, msg := range messages {
			if !deleted[i+1] {
				sendResponse(conn, fmt.Sprintf("%d %d", i+1, msg.id))
			}
		}
		sendResponse(conn, ".")
		return
	}

	num, err := strconv.Atoi(arg)
	if err != nil || num < 1 || num > len(messages) {
		sendResponse(conn, "-ERR no such message")
		return
	}
	if deleted[num] {
		sendResponse(conn, "-ERR message deleted")
		return
	}
	sendResponse(conn, fmt.Sprintf("+OK %d %d", num, messages[num-1].id))
}

// expungeDeleted actually deletes messages that were marked for deletion,
// returning the number of messages deleted.
func (s *POP3Server) expungeDeleted(messages []pop3Message, deleted map[int]bool, user *db.User) int {
	if deleted == nil || user == nil || user.ID == 0 {
		return 0
	}
	count := 0
	for seqNum, msgDeleted := range deleted {
		if msgDeleted && seqNum >= 1 && seqNum <= len(messages) {
			if err := s.stores.Mails.Delete(messages[seqNum-1].id); err != nil {
				log.Printf("POP3: failed to delete message %d: %v", messages[seqNum-1].id, err)
				continue
			}
			count++
		}
	}
	return count
}

// sendResponse writes a POP3 response line to the connection.
func sendResponse(conn net.Conn, line string) {
	conn.Write([]byte(line + "\r\n"))
}

func normalizePOP3Data(raw string) string {
	var b strings.Builder
	writeDotStuffed(&b, raw, nil)
	return b.String()
}

type stringWriter interface {
	Write([]byte) (int, error)
}

func writeDotStuffed(w stringWriter, raw string, maxLines *int) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\r", "\n")
	lines := strings.Split(raw, "\n")
	written := 0
	for i, line := range lines {
		if maxLines != nil && written >= *maxLines {
			break
		}
		if i == len(lines)-1 && line == "" {
			break
		}
		if strings.HasPrefix(line, ".") {
			w.Write([]byte("."))
		}
		w.Write([]byte(line + "\r\n"))
		written++
	}
}

func splitHeaderBody(raw string) (string, string) {
	if idx := strings.Index(raw, "\r\n\r\n"); idx >= 0 {
		return raw[:idx], raw[idx+4:]
	}
	if idx := strings.Index(raw, "\n\n"); idx >= 0 {
		return raw[:idx], raw[idx+2:]
	}
	return raw, ""
}

// buildRawMessage reconstructs a raw RFC822 message from a db.Message.
func buildRawMessage(msg *db.Message) []byte {
	if msg.RawData != "" {
		return []byte(msg.RawData)
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("From: %s\r\n", msg.FromAddr))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", msg.ToAddr))
	if msg.CcAddr != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", msg.CcAddr))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700")))
	if msg.MessageID != "" {
		buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	if msg.HtmlBody != "" && msg.TextBody != "" {
		boundary := fmt.Sprintf("mailgo_%d", msg.ID)
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	} else if msg.TextBody != "" {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
	} else if msg.HtmlBody != "" {
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
	} else {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	}
	return []byte(buf.String())
}
