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
	"mail_go/internal/store"
	"mail_go/internal/storage"

	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-smtp"
)

// SMTPServer wraps a go-smtp Server and provides mail receiving capability.
type SMTPServer struct {
	server  *smtp.Server
	stores  *store.Stores
	storage *storage.AttachmentStorage
	cfg     config.SMTPConfig
}

// NewSMTPServer creates a new SMTP server instance.
func NewSMTPServer(cfg config.SMTPConfig, stores *store.Stores, attStorage *storage.AttachmentStorage) *SMTPServer {
	s := &SMTPServer{
		stores:  stores,
		storage: attStorage,
		cfg:     cfg,
	}

	be := &smtpBackend{server: s}
	srv := smtp.NewServer(be)
	srv.Addr = cfg.Addr
	srv.Domain = cfg.Domain
	srv.MaxMessageBytes = cfg.MaxMessage
	srv.AllowInsecureAuth = true
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second

	s.server = srv
	return s
}

// Start starts the SMTP server on the plain-text port.
func (s *SMTPServer) Start() error {
	log.Printf("SMTP server listening on %s", s.cfg.Addr)
	return s.server.ListenAndServe()
}

// StartTLS starts the SMTP server on the TLS port.
func (s *SMTPServer) StartTLS() error {
	if s.cfg.TLSCert == "" || s.cfg.TLSKey == "" {
		return fmt.Errorf("SMTP TLS certificate or key not configured")
	}

	cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
	if err != nil {
		return fmt.Errorf("failed to load SMTP TLS certificate: %w", err)
	}

	// 创建一个新的 SMTP 服务器实例用于 TLS 端口
	be := &smtpBackend{server: s}
	srv := smtp.NewServer(be)
	srv.Addr = s.cfg.TLSAddr
	srv.Domain = s.cfg.Domain
	srv.MaxMessageBytes = s.cfg.MaxMessage
	srv.AllowInsecureAuth = false
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	srv.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{cert},
	}

	log.Printf("SMTPS server listening on %s", s.cfg.TLSAddr)
	return srv.ListenAndServeTLS()
}

// smtpBackend implements the smtp.Backend interface.
type smtpBackend struct {
	server *SMTPServer
}

// NewSession creates a new SMTP session for the incoming connection.
func (be *smtpBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &smtpSession{
		backend:     be,
		rcpts:       make([]string, 0),
		attachments: make([]*db.Attachment, 0),
	}, nil
}

// smtpSession implements the smtp.Session interface for handling a single connection.
type smtpSession struct {
	backend      *smtpBackend
	from         string
	rcpts        []string
	authenticated bool
	username     string
	attachments  []*db.Attachment
}

// AuthPlain authenticates the user with plain-text credentials.
func (s *smtpSession) AuthPlain(username, password string) error {
	user, err := s.backend.server.stores.Users.Authenticate(username, password)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	s.authenticated = true
	s.username = user.Username
	return nil
}

// Mail records the sender address (MAIL FROM command).
func (s *smtpSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	s.rcpts = s.rcpts[:0]
	s.attachments = s.attachments[:0]
	return nil
}

// Rcpt validates and records a recipient address (RCPT TO command).
// It verifies that the recipient domain exists in the system and the user exists.
func (s *smtpSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	parts := strings.SplitN(to, "@", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid recipient address: %s", to)
	}

	userName := parts[0]
	domainName := parts[1]

	// Check if domain is managed by this system
	domain, err := s.backend.server.stores.Domains.GetByName(domainName)
	if err != nil {
		return fmt.Errorf("domain not found: %s", domainName)
	}

	// Check if the user exists in this domain
	_, err = s.backend.server.stores.Users.GetByUsername(userName, domain.ID)
	if err != nil {
		return fmt.Errorf("user not found: %s", to)
	}

	s.rcpts = append(s.rcpts, to)
	return nil
}

// Data handles the message body (DATA command). It parses the MIME message,
// extracts fields and attachments, and stores the message for each recipient.
func (s *smtpSession) Data(r io.Reader) error {
	// Read all message data
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("failed to read message data: %w", err)
	}

	// Parse as MIME message
	mr, err := mail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to parse MIME message: %w", err)
	}

	// Extract headers from the top-level mail header
	header := mr.Header

	fromAddr := header.Get("From")
	toAddr := header.Get("To")
	ccAddr := header.Get("Cc")
	subject, _ := header.Subject()
	messageID, _ := header.MessageID()
	date, _ := header.Date()
	if date.IsZero() {
		date = time.Now()
	}

	var textBody, htmlBody string
	var attachments []*db.Attachment

	// Iterate through all MIME parts
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
			contentType, _, _ := h.ContentType()
			buf, readErr := io.ReadAll(p.Body)
			if readErr != nil {
				log.Printf("SMTP: error reading inline part: %v", readErr)
				continue
			}
			if strings.HasPrefix(contentType, "text/plain") {
				textBody = string(buf)
			} else if strings.HasPrefix(contentType, "text/html") {
				htmlBody = string(buf)
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

			relPath, saveErr := s.backend.server.storage.Save(filename, buf)
			if saveErr != nil {
				log.Printf("SMTP: failed to save attachment %s: %v", filename, saveErr)
				continue
			}

			attachments = append(attachments, &db.Attachment{
				FileName:    filename,
				FilePath:    relPath,
				ContentType: contentType,
				FileSize:    int64(len(buf)),
			})
		}
	}

	// Fallback: if no text body was extracted from MIME parts, use the raw data
	if textBody == "" && htmlBody == "" {
		textBody = string(data)
	}

	// Create a Message record for each verified recipient
	for _, rcpt := range s.rcpts {
		user, err := s.backend.server.stores.Users.GetByEmail(rcpt)
		if err != nil {
			log.Printf("SMTP: recipient not found %s, skipping", rcpt)
			continue
		}

		msg := &db.Message{
			UserID:    user.ID,
			MessageID: messageID,
			Folder:    "INBOX",
			FromAddr:  fromAddr,
			ToAddr:    toAddr,
			CcAddr:    ccAddr,
			Subject:   subject,
			TextBody:  textBody,
			HtmlBody:  htmlBody,
			IsRead:    false,
			IsFlagged: false,
			Date:      date,
		}

		if createErr := s.backend.server.stores.Mails.Create(msg); createErr != nil {
			log.Printf("SMTP: failed to create message for %s: %v", rcpt, createErr)
			continue
		}

		// Create Attachment records linked to the new message
		for _, att := range attachments {
			attCopy := db.Attachment{
				MessageID:   msg.ID,
				FileName:    att.FileName,
				FilePath:    att.FilePath,
				ContentType: att.ContentType,
				FileSize:    att.FileSize,
			}
			if attErr := s.backend.server.stores.Attachments.Create(&attCopy); attErr != nil {
				log.Printf("SMTP: failed to create attachment record: %v", attErr)
			}
		}

		log.Printf("SMTP: message delivered to %s (ID=%d)", rcpt, msg.ID)
	}

	return nil
}

// Reset clears the session state for the next message on the same connection.
func (s *smtpSession) Reset() {
	s.from = ""
	s.rcpts = s.rcpts[:0]
	s.attachments = s.attachments[:0]
}

// Logout is called when the SMTP connection is closed.
func (s *smtpSession) Logout() error {
	return nil
}
