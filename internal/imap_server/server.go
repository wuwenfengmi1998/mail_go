package imap_server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"

	"mail_go/config"
	"mail_go/internal/store"
	"mail_go/internal/tlsutil"

	"github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"
)

// IMAPServer wraps a go-imap Server and provides mailbox access capability.
type IMAPServer struct {
	stores    *store.Stores
	cfg       config.IMAPConfig
	banCfg    config.BanConfig
	tlsLoader *tlsutil.Loader
}

// NewIMAPServer creates a new IMAP server instance. tlsLoader may be nil
// when TLS is not configured.
func NewIMAPServer(cfg config.IMAPConfig, stores *store.Stores, tlsLoader *tlsutil.Loader, banCfg config.BanConfig) *IMAPServer {
	return &IMAPServer{
		stores:    stores,
		cfg:       cfg,
		banCfg:    banCfg,
		tlsLoader: tlsLoader,
	}
}

func (s *IMAPServer) tlsConfig() (*tls.Config, error) {
	if s.tlsLoader == nil {
		return nil, fmt.Errorf("IMAP TLS certificate or key not configured")
	}
	// GetCertificate 每次握手按需重载证书，证书更新后无需重启服务
	return &tls.Config{GetCertificate: s.tlsLoader.GetCertificate}, nil
}

// newServer creates a configured imapserver.Server with the given address.
func (s *IMAPServer) newServer(addr string, tlsConfig *tls.Config) *imapserver.Server {
	be := &imapBackend{stores: s.stores, banCfg: s.banCfg, port: portOf(addr)}
	srv := imapserver.New(be)
	srv.Addr = addr
	srv.TLSConfig = tlsConfig
	srv.AllowInsecureAuth = tlsConfig == nil
	return srv
}

// portOf 从监听地址解析端口号，失败返回 0。
func portOf(addr string) int {
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
