package smtp_server

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/mailutil"
	"mail_go/internal/outbound"
	"mail_go/internal/storage"
	"mail_go/internal/store"

	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type smtpMode int

const (
	smtpModeInbound smtpMode = iota
	smtpModeSubmission
	smtpModeImplicitTLS
)

// SMTPServer wraps go-smtp servers and provides local mail delivery.
type SMTPServer struct {
	stores   *store.Stores
	storage  *storage.AttachmentStorage
	outbound *outbound.Manager
	cfg      config.SMTPConfig
}

// NewSMTPServer creates a new SMTP server instance.
func NewSMTPServer(cfg config.SMTPConfig, stores *store.Stores, attStorage *storage.AttachmentStorage, ob *outbound.Manager) *SMTPServer {
	return &SMTPServer{stores: stores, storage: attStorage, outbound: ob, cfg: cfg}
}

func (s *SMTPServer) tlsConfig() (*tls.Config, error) {
	if s.cfg.TLSCert == "" || s.cfg.TLSKey == "" {
		return nil, fmt.Errorf("SMTP TLS certificate or key not configured")
	}

	cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load SMTP TLS certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

func (s *SMTPServer) newServer(addr string, mode smtpMode, tlsConfig *tls.Config) *smtp.Server {
	be := &smtpBackend{server: s, mode: mode}
	srv := smtp.NewServer(be)
	srv.Addr = addr
	srv.Domain = s.cfg.Domain
	srv.MaxMessageBytes = s.cfg.MaxMessage
	srv.AllowInsecureAuth = tlsConfig == nil
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	srv.TLSConfig = tlsConfig
	return srv
}

// Start starts the inbound SMTP server.
func (s *SMTPServer) Start() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		log.Printf("SMTP STARTTLS 未启用: %v", err)
	}

	log.Printf("SMTP server listening on %s", s.cfg.Addr)
	return s.newServer(s.cfg.Addr, smtpModeInbound, tlsConfig).ListenAndServe()
}

// StartTLS starts the implicit TLS SMTP submission server.
func (s *SMTPServer) StartTLS() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}

	log.Printf("SMTPS server listening on %s", s.cfg.TLSAddr)
	return s.newServer(s.cfg.TLSAddr, smtpModeImplicitTLS, tlsConfig).ListenAndServeTLS()
}

// StartSubmission starts the SMTP submission server with STARTTLS support.
func (s *SMTPServer) StartSubmission() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}

	log.Printf("SMTP submission server listening on %s", s.cfg.SubmissionAddr)
	return s.newServer(s.cfg.SubmissionAddr, smtpModeSubmission, tlsConfig).ListenAndServe()
}

// smtpBackend implements the smtp.Backend interface.
type smtpBackend struct {
	server *SMTPServer
	mode   smtpMode
}

// NewSession creates a new SMTP session for the incoming connection.
func (be *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{
		backend: be,
		mode:    be.mode,
		rcpts:   make([]string, 0),
	}, nil
}

// smtpSession implements the smtp.Session interface for handling a single connection.
type smtpSession struct {
	backend       *smtpBackend
	mode          smtpMode
	from          string
	rcpts         []string
	localRcpts    []string
	externalRcpts []string
	authenticated bool
	userID        uint
	email         string
	user          *db.User
}

// AuthMechanisms returns supported SMTP AUTH mechanisms.
func (s *smtpSession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth authenticates the user with SASL PLAIN credentials.
func (s *smtpSession) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, smtp.ErrAuthUnknownMechanism
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		user, err := s.backend.server.stores.Users.Authenticate(username, password)
		if err != nil {
			return smtp.ErrAuthFailed
		}

		domainName := user.Domain.Name
		if domainName == "" {
			domain, err := s.backend.server.stores.Domains.GetByID(user.DomainID)
			if err == nil {
				domainName = domain.Name
			}
		}
		if domainName == "" {
			return smtp.ErrAuthFailed
		}

		s.authenticated = true
		s.userID = user.ID
		s.user = user
		s.email = user.Username + "@" + domainName
		return nil
	}), nil
}

// Mail records the sender address (MAIL FROM command).
func (s *smtpSession) Mail(from string, opts *smtp.MailOptions) error {
	if s.mode != smtpModeInbound && !s.authenticated {
		return smtp.ErrAuthRequired
	}
	// Authenticated users may only send as themselves, preventing spoofing.
	if s.authenticated && !strings.EqualFold(strings.TrimSpace(from), s.email) {
		return fmt.Errorf("sender address must match authenticated user")
	}

	s.from = from
	s.rcpts = s.rcpts[:0]
	s.localRcpts = s.localRcpts[:0]
	s.externalRcpts = s.externalRcpts[:0]
	return nil
}

// Rcpt validates and records a recipient address (RCPT TO command).
// Local recipients are delivered to the mailbox; external recipients are
// allowed only for authenticated users and go to the outbound queue,
// which prevents open relay.
func (s *smtpSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	to = strings.TrimSpace(to)
	if to == "" {
		return fmt.Errorf("invalid recipient address: %s", to)
	}

	if _, err := s.localUserByEmail(to); err == nil {
		s.rcpts = append(s.rcpts, to)
		s.localRcpts = append(s.localRcpts, to)
		return nil
	}

	// External recipient: only authenticated local users may relay.
	if !s.authenticated {
		return fmt.Errorf("relay access denied: %s", to)
	}

	// Sender verification must have been enforced in Mail() already.
	ob := s.backend.server.outbound
	if ob == nil || !ob.Enabled() {
		return fmt.Errorf("external delivery is disabled: %s", to)
	}

	s.rcpts = append(s.rcpts, to)
	s.externalRcpts = append(s.externalRcpts, to)
	return nil
}

func (s *smtpSession) localUserByEmail(email string) (*db.User, error) {
	return s.backend.server.stores.Users.GetByEmail(strings.TrimSpace(email))
}

// Data handles the message body and stores it for local recipients.
// External recipients (authenticated sessions only) are queued for
// outbound delivery.
func (s *smtpSession) Data(r io.Reader) error {
	if len(s.rcpts) == 0 {
		return fmt.Errorf("no accepted recipients")
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to read message data: %w", err)
	}

	parsed, err := parseSMTPMessage(data)
	if err != nil {
		return err
	}

	// Local recipients: deliver to INBOX.
	for _, rcpt := range s.localRcpts {
		user, err := s.localUserByEmail(rcpt)
		if err != nil {
			log.Printf("SMTP: recipient not found %s, skipping", rcpt)
			continue
		}
		if err := s.saveMessage(user.ID, "INBOX", parsed, data, false); err != nil {
			log.Printf("SMTP: failed to create message for %s: %v", rcpt, err)
			continue
		}
		log.Printf("SMTP: message delivered to %s", rcpt)
	}

	// External recipients: queue for outbound delivery.
	if len(s.externalRcpts) > 0 {
		ob := s.backend.server.outbound
		if ob == nil {
			return fmt.Errorf("outbound delivery is unavailable")
		}
		maxRcpt := ob.MaxRecipients()
		if maxRcpt > 0 && len(s.externalRcpts) > maxRcpt {
			return fmt.Errorf("too many external recipients: %d (max %d)", len(s.externalRcpts), maxRcpt)
		}
		for _, rcpt := range s.externalRcpts {
			if _, err := ob.Enqueue(s.user, s.email, rcpt, data); err != nil {
				return fmt.Errorf("failed to queue external recipient %s: %v", rcpt, err)
			}
			log.Printf("SMTP: external message queued for %s", rcpt)
		}
	}

	if s.authenticated && s.userID != 0 && s.mode != smtpModeInbound {
		if err := s.saveMessage(s.userID, "Sent", parsed, data, true); err != nil {
			log.Printf("SMTP: failed to save sent copy for %s: %v", s.email, err)
		}
	}

	return nil
}

type parsedSMTPMessage struct {
	messageID   string
	fromAddr    string
	toAddr      string
	ccAddr      string
	subject     string
	textBody    string
	htmlBody    string
	date        time.Time
	attachments []*db.Attachment
}

func parseSMTPMessage(data []byte) (*parsedSMTPMessage, error) {
	mr, err := mail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse MIME message: %w", err)
	}

	header := mr.Header
	msg := &parsedSMTPMessage{}
	msg.fromAddr = mailutil.FormatAddressList(&header, "From")
	msg.toAddr = mailutil.FormatAddressList(&header, "To")
	msg.ccAddr = mailutil.FormatAddressList(&header, "Cc")
	msg.subject, _ = header.Subject()
	msg.messageID, _ = header.MessageID()
	msg.date, _ = header.Date()
	if msg.date.IsZero() {
		msg.date = time.Now()
	}

	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("SMTP: error reading MIME part: %v", err)
			break
		}

		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			contentType, params, _ := h.ContentType()
			buf, readErr := io.ReadAll(p.Body)
			if readErr != nil {
				log.Printf("SMTP: error reading inline part: %v", readErr)
				continue
			}
			charset := ""
			if cs, ok := params["charset"]; ok {
				charset = cs
			}
			decoded := mailutil.DecodeCharset(buf, charset)
			if strings.HasPrefix(contentType, "text/plain") {
				msg.textBody = decoded
			} else if strings.HasPrefix(contentType, "text/html") {
				msg.htmlBody = decoded
			}

		case *mail.AttachmentHeader:
			filename, _ := h.Filename()
			if filename == "" {
				filename = "unnamed_attachment"
			}
			contentType, _, _ := h.ContentType()
			buf, readErr := io.ReadAll(p.Body)
			if readErr != nil {
				log.Printf("SMTP: error reading attachment part: %v", readErr)
				continue
			}
			msg.attachments = append(msg.attachments, &db.Attachment{
				FileName:    filename,
				ContentType: contentType,
				FileSize:    int64(len(buf)),
			})
		}
	}

	if msg.textBody == "" && msg.htmlBody == "" {
		msg.textBody = string(data)
	}
	return msg, nil
}

func (s *smtpSession) saveMessage(userID uint, folder string, parsed *parsedSMTPMessage, data []byte, read bool) error {
	msg := &db.Message{
		UserID:    userID,
		MessageID: parsed.messageID,
		Folder:    folder,
		FromAddr:  parsed.fromAddr,
		ToAddr:    parsed.toAddr,
		CcAddr:    parsed.ccAddr,
		Subject:   parsed.subject,
		TextBody:  parsed.textBody,
		HtmlBody:  parsed.htmlBody,
		RawData:   string(data),
		IsRead:    read,
		IsFlagged: false,
		Date:      parsed.date,
	}
	return s.backend.server.stores.Mails.Create(msg)
}

// Reset clears the session state for the next message on the same connection.
func (s *smtpSession) Reset() {
	s.from = ""
	s.rcpts = s.rcpts[:0]
	s.localRcpts = s.localRcpts[:0]
	s.externalRcpts = s.externalRcpts[:0]
}

// Logout is called when the SMTP connection is closed.
func (s *smtpSession) Logout() error {
	return nil
}
