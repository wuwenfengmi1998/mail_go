package imap_server

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
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
	hub       *connhub.Hub

	beMu sync.Mutex
	bes  []*imapBackend // 各监听器（明文/TLS）的 backend，用于新邮件推送
}

// NewIMAPServer creates a new IMAP server instance. tlsLoader may be nil
// when TLS is not configured.
func NewIMAPServer(cfg config.IMAPConfig, stores *store.Stores, tlsLoader *tlsutil.Loader, banCfg config.BanConfig, hub *connhub.Hub) *IMAPServer {
	return &IMAPServer{
		stores:    stores,
		cfg:       cfg,
		banCfg:    banCfg,
		tlsLoader: tlsLoader,
		hub:       hub,
	}
}

// NotifyNewMessage 向所有 IMAP 监听器推送新邮件通知（go-imap 广播时按
// 用户名+邮箱过滤，只送达已选中 INBOX 的客户端，IDLE 挂起时实时收到
// FETCH 响应）。由 SMTP/Web 本地投递成功时调用；channel 满时非阻塞丢弃。
func (s *IMAPServer) NotifyNewMessage(userEmail string, msg *db.Message) {
	if s == nil || userEmail == "" || msg == nil {
		return
	}
	update := buildNewMessageUpdate(s.stores, userEmail, msg)
	if update == nil {
		return
	}

	s.beMu.Lock()
	bes := append([]*imapBackend(nil), s.bes...)
	s.beMu.Unlock()

	for _, b := range bes {
		select {
		case b.updates <- update:
		default:
			log.Printf("IMAP: 新邮件推送通道已满，丢弃 %s 的更新 (msg=%d)", userEmail, msg.ID)
		}
	}
}

// registerBackend 记录新建的 backend（用于新邮件推送）。
func (s *IMAPServer) registerBackend(be *imapBackend) {
	s.beMu.Lock()
	s.bes = append(s.bes, be)
	s.beMu.Unlock()
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
	be := &imapBackend{
		stores:  s.stores,
		banCfg:  s.banCfg,
		port:    portOf(addr),
		hub:     s.hub,
		updates: make(chan backend.Update, 256),
	}
	s.registerBackend(be)
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
