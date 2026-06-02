package imap_server

import (
	"crypto/tls"
	"fmt"
	"log"

	"mail_go/config"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"
)

// IMAPServer wraps a go-imap Server and provides mailbox access capability.
type IMAPServer struct {
	stores *store.Stores
	cfg    config.IMAPConfig
}

// NewIMAPServer creates a new IMAP server instance.
func NewIMAPServer(cfg config.IMAPConfig, stores *store.Stores) *IMAPServer {
	return &IMAPServer{
		stores: stores,
		cfg:    cfg,
	}
}

func (s *IMAPServer) tlsConfig() (*tls.Config, error) {
	if s.cfg.TLSCert == "" || s.cfg.TLSKey == "" {
		return nil, fmt.Errorf("IMAP TLS certificate or key not configured")
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load IMAP TLS certificate: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// newServer creates a configured imapserver.Server with the given address.
func (s *IMAPServer) newServer(addr string, tlsConfig *tls.Config) *imapserver.Server {
	be := &imapBackend{stores: s.stores}
	srv := imapserver.New(be)
	srv.Addr = addr
	srv.TLSConfig = tlsConfig
	srv.AllowInsecureAuth = tlsConfig == nil
	return srv
}

// Start starts the IMAP server on the plain-text port.
func (s *IMAPServer) Start() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		log.Printf("IMAP STARTTLS 未启用: %v", err)
	}
	srv := s.newServer(s.cfg.Addr, tlsConfig)
	log.Printf("IMAP server listening on %s", s.cfg.Addr)
	return srv.ListenAndServe()
}

// StartTLS starts the IMAP server on the TLS port.
func (s *IMAPServer) StartTLS() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}

	srv := s.newServer(s.cfg.TLSAddr, tlsConfig)

	log.Printf("IMAPS server listening on %s", s.cfg.TLSAddr)
	return srv.ListenAndServeTLS()
}

// ensure imapBackend satisfies backend.Backend at compile time
var _ backend.Backend = (*imapBackend)(nil)
