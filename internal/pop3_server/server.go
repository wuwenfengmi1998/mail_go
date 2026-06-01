package pop3_server

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/store"
)

// POP3Server implements a simple POP3 mail server over TCP.
type POP3Server struct {
	listener net.Listener
	stores   *store.Stores
	cfg      config.POP3Config
	wg       sync.WaitGroup
}

// NewPOP3Server creates a new POP3 server instance.
func NewPOP3Server(cfg config.POP3Config, stores *store.Stores) *POP3Server {
	return &POP3Server{
		stores: stores,
		cfg:    cfg,
	}
}

// Start starts the POP3 server on the configured plain-text port.
func (s *POP3Server) Start() error {
	var err error
	s.listener, err = net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("POP3 listen failed: %w", err)
	}

	log.Printf("POP3 server listening on %s", s.cfg.Addr)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := s.listener.Accept()
			if err != nil {
				// Listener closed
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.handleConn(conn)
			}()
		}
	}()

	return nil
}

// StartTLS starts the POP3 server on the configured TLS port.
func (s *POP3Server) StartTLS() error {
	if s.cfg.TLSCert == "" || s.cfg.TLSKey == "" {
		return fmt.Errorf("POP3 TLS certificate or key not configured")
	}
	return fmt.Errorf("POP3 TLS not yet implemented")
}

// handleConn handles a single POP3 client connection.
func (s *POP3Server) handleConn(conn net.Conn) {
	defer conn.Close()

	// Set read/write deadlines
	conn.SetDeadline(time.Now().Add(10 * time.Minute))

	reader := bufio.NewReader(conn)

	// Session state
	var user *db.User
	var messages []pop3Message
	var deleted map[int]bool

	// Send greeting
	sendResponse(conn, "+OK MailGo POP3 server ready")

	// Main command loop
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return // connection closed or error
		}

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse command
		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToUpper(parts[0])
		arg := ""
		if len(parts) > 1 {
			arg = strings.TrimSpace(parts[1])
		}

		switch cmd {
		case "USER":
			user, messages, deleted = s.handleUSER(conn, arg, user)
		case "PASS":
			user, messages, deleted = s.handlePASS(conn, arg, user)
		case "STAT":
			s.handleSTAT(conn, messages, deleted)
		case "LIST":
			s.handleLIST(conn, arg, messages, deleted)
		case "RETR":
			s.handleRETR(conn, arg, messages, deleted)
		case "DELE":
			s.handleDELE(conn, arg, deleted)
		case "NOOP":
			sendResponse(conn, "+OK")
		case "RSET":
			deleted = make(map[int]bool)
			sendResponse(conn, "+OK")
		case "QUIT":
			// In UPDATE state, actually delete marked messages
			s.expungeDeleted(messages, deleted, user)
			sendResponse(conn, "+OK MailGo POP3 server signing off")
			return
		case "CAPA":
			sendResponse(conn, "+OK Capability list follows")
			sendResponse(conn, "USER")
			sendResponse(conn, ".")
		case "TOP":
			s.handleTOP(conn, arg, messages, deleted)
		case "UIDL":
			s.handleUIDL(conn, arg, messages, deleted)
		default:
			sendResponse(conn, "-ERR unknown command")
		}
	}
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
			size:    len(raw),
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

	// Look up the user by email
	user, err := s.stores.Users.GetByEmail(username)
	if err != nil {
		// Don't reveal whether user exists yet (standard POP3 behavior)
		sendResponse(conn, "+OK")
		// Store the attempted username for PASS to use
		return &db.User{Username: username}, nil, nil
	}

	sendResponse(conn, "+OK")
	return user, nil, nil
}

// handlePASS processes the PASS command.
func (s *POP3Server) handlePASS(conn net.Conn, password string, user *db.User) (*db.User, []pop3Message, map[int]bool) {
	if user == nil {
		sendResponse(conn, "-ERR no username given")
		return nil, nil, nil
	}

	// Authenticate
	authUser, err := s.stores.Users.Authenticate(user.Username, password)
	if err != nil {
		// If username was an email, try using it directly
		if strings.Contains(user.Username, "@") {
			authUser, err = s.stores.Users.Authenticate(user.Username, password)
		}
		if err != nil {
			sendResponse(conn, "-ERR authentication failed")
			return nil, nil, nil
		}
	}

	// Load messages
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
		// List all messages
		sendResponse(conn, "+OK message list follows")
		for i, msg := range messages {
			if !deleted[i+1] {
				sendResponse(conn, fmt.Sprintf("%d %d", i+1, msg.size))
			}
		}
		sendResponse(conn, ".")
	} else {
		// List specific message
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

	// Send the raw message, dot-stuffing any lines that start with "."
	scanner := bufio.NewScanner(strings.NewReader(msg.raw))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ".") {
			conn.Write([]byte("."))
		}
		conn.Write([]byte(line + "\r\n"))
	}
	conn.Write([]byte(".\r\n"))
}

// handleDELE processes the DELE command.
func (s *POP3Server) handleDELE(conn net.Conn, arg string, deleted map[int]bool) {
	num, err := strconv.Atoi(arg)
	if err != nil || num < 1 {
		sendResponse(conn, "-ERR invalid message number")
		return
	}
	if deleted == nil {
		deleted = make(map[int]bool)
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
	// Format: TOP msgNum [n]
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
	if deleted != nil && deleted[num] {
		sendResponse(conn, "-ERR message deleted")
		return
	}

	nLines := 0
	if len(parts) > 1 {
		nLines, _ = strconv.Atoi(parts[1])
	}

	msg := messages[num-1]
	sendResponse(conn, "+OK top of message follows")

	// Find blank line separating headers from body
	headerEnd := strings.Index(msg.raw, "\r\n\r\n")
	if headerEnd == -1 {
		headerEnd = strings.Index(msg.raw, "\n\n")
		if headerEnd == -1 {
			// No body, just send everything
			sendResponse(conn, msg.raw)
			sendResponse(conn, ".")
			return
		}
	}

	// Send headers
	header := msg.raw[:headerEnd]
	scanner := bufio.NewScanner(strings.NewReader(header))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, ".") {
			conn.Write([]byte("."))
		}
		conn.Write([]byte(line + "\r\n"))
	}
	conn.Write([]byte("\r\n"))

	// Send up to N lines of body
	if nLines > 0 {
		body := msg.raw[headerEnd:]
		scanner := bufio.NewScanner(strings.NewReader(body))
		lineCount := 0
		for scanner.Scan() && lineCount < nLines {
			line := scanner.Text()
			if strings.HasPrefix(line, ".") {
				conn.Write([]byte("."))
			}
			conn.Write([]byte(line + "\r\n"))
			lineCount++
		}
	}

	conn.Write([]byte(".\r\n"))
}

// handleUIDL processes the UIDL command.
func (s *POP3Server) handleUIDL(conn net.Conn, arg string, messages []pop3Message, deleted map[int]bool) {
	if arg == "" {
		// List all UIDs
		sendResponse(conn, "+OK UIDL list follows")
		for i, msg := range messages {
			if deleted == nil || !deleted[i+1] {
				sendResponse(conn, fmt.Sprintf("%d %d", i+1, msg.id))
			}
		}
		sendResponse(conn, ".")
	} else {
		// Specific message UID
		num, err := strconv.Atoi(arg)
		if err != nil || num < 1 || num > len(messages) {
			sendResponse(conn, "-ERR no such message")
			return
		}
		if deleted != nil && deleted[num] {
			sendResponse(conn, "-ERR message deleted")
			return
		}
		sendResponse(conn, fmt.Sprintf("+OK %d %d", num, messages[num-1].id))
	}
}

// expungeDeleted actually deletes messages that were marked for deletion.
func (s *POP3Server) expungeDeleted(messages []pop3Message, deleted map[int]bool, user *db.User) {
	if deleted == nil || user == nil {
		return
	}
	for seqNum, msgDeleted := range deleted {
		if msgDeleted && seqNum >= 1 && seqNum <= len(messages) {
			if err := s.stores.Mails.Delete(messages[seqNum-1].id); err != nil {
				log.Printf("POP3: failed to delete message %d: %v", messages[seqNum-1].id, err)
			}
		}
	}
}

// sendResponse writes a POP3 response line to the connection.
func sendResponse(conn net.Conn, line string) {
	conn.Write([]byte(line + "\r\n"))
}

// buildRawMessage reconstructs a raw RFC822 message from a db.Message.
// This is a local copy to avoid importing from the imap_server package.
func buildRawMessage(msg *db.Message) []byte {
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
