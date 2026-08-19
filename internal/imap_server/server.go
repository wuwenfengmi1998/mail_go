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

	"github.com/emersion/go-imap/v2"
	imapserver "github.com/emersion/go-imap/v2/imapserver"
)

// Pusher 是 IMAP 实时推送接口：SMTP/POP3/Web 在邮件状态变化后调用，
// 更新经 mailboxHub 分发给已选中对应邮箱的其他会话（IDLE 时即时送达）。
type Pusher interface {
	// PushNewMessage 推送新邮件（本地投递成功）。
	PushNewMessage(userEmail string, msg *db.Message)
	// PushFlagsChanged 推送已读/星标等标志变化。
	PushFlagsChanged(userEmail, mailbox string, msg *db.Message)
	// PushExpunged 推送邮件被删除（seqNums 为删除前序号）。
	PushExpunged(userEmail, mailbox string, seqNums []uint32)
}

// IMAPServer 管理 IMAP/IMAPS 监听器、会话注册与跨会话推送。
type IMAPServer struct {
	stores    *store.Stores
	cfg       config.IMAPConfig
	banCfg    config.BanConfig
	tlsLoader *tlsutil.Loader
	hub       *connhub.Hub

	// svc 邮箱服务层（文件夹目录/消息操作），IMAP 会话与 Web 共用。
	svc *MailboxService

	// hubs 按「用户邮箱 + 文件夹」索引的推送中心，会话 SELECT 时加入。
	mu   sync.Mutex
	hubs map[string]*mailboxHub
	// sessions 全部活跃会话（DisconnectByAddr 用）。
	sessions map[*imapSession]struct{}
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
		svc:       NewMailboxService(stores),
		hubs:      make(map[string]*mailboxHub),
		sessions:  make(map[*imapSession]struct{}),
	}
}

// MailboxService 返回邮箱服务层（Web handler 共用）。
func (s *IMAPServer) MailboxService() *MailboxService {
	return s.svc
}

// hubKey 生成推送中心索引键。
func hubKey(userEmail, mailbox string) string {
	return userEmail + "\x00" + mailbox
}

// hubFor 返回已存在的推送中心，不存在返回 nil。
func (s *IMAPServer) hubFor(userEmail, mailbox string) *mailboxHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hubs[hubKey(userEmail, mailbox)]
}

// hubForOrCreate 返回推送中心，不存在则创建。
func (s *IMAPServer) hubForOrCreate(userEmail, mailbox string) *mailboxHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hubKey(userEmail, mailbox)
	h := s.hubs[key]
	if h == nil {
		h = newMailboxHub()
		s.hubs[key] = h
	}
	return h
}

func (s *IMAPServer) unregisterSession(sess *imapSession) {
	s.mu.Lock()
	delete(s.sessions, sess)
	s.mu.Unlock()
}

// NotifyNewMessage 推送新邮件到达通知：EXISTS 计数 + 标记更新。
// 由 SMTP/Web 本地投递成功时调用；无会话选中该邮箱时为 no-op。
// 推送目标邮箱取 msg.Folder（本地投递为 INBOX；Web 移动/恢复时
// 用于通知目标文件夹，如 Trash）。
func (s *IMAPServer) PushNewMessage(userEmail string, msg *db.Message) {
	if s == nil || userEmail == "" || msg == nil || msg.Folder == "" {
		return
	}
	hub := s.hubFor(userEmail, msg.Folder)
	if hub == nil {
		return
	}
	if count, err := s.stores.Mails.CountByUserAndFolder(msg.UserID, msg.Folder); err == nil {
		hub.enqueue(sessionUpdate{exists: ptrU32(uint32(count))}, nil)
	}
}

// PushFlagsChanged 推送邮件标志（已读/星标/删除标记）变化给同用户其他会话。
func (s *IMAPServer) PushFlagsChanged(userEmail, mailbox string, msg *db.Message) {
	if s == nil || userEmail == "" || mailbox == "" || msg == nil {
		return
	}
	hub := s.hubFor(userEmail, mailbox)
	if hub == nil {
		return
	}
	seq := seqOf(s.stores, msg.UserID, mailbox, msg.ID)
	if seq == 0 {
		return
	}
	hub.enqueue(sessionUpdate{fetch: &sessionFetchUpdate{
		seq:   seq,
		uid:   imap.UID(msg.ID),
		flags: flagsOf(msg.IsRead, msg.IsFlagged, msg.IsDeleted),
	}}, nil)
}

// PushExpunged 推送邮件被删除（每条序号一个 EXPUNGE 更新）。
func (s *IMAPServer) PushExpunged(userEmail, mailbox string, seqNums []uint32) {
	if s == nil || userEmail == "" || mailbox == "" || len(seqNums) == 0 {
		return
	}
	hub := s.hubFor(userEmail, mailbox)
	if hub == nil {
		return
	}
	for _, seq := range seqNums {
		hub.enqueue(sessionUpdate{expunge: ptrU32(seq)}, nil)
	}
}

// DisconnectByAddr 强制断开指定远端地址的连接（管理后台「断开并封禁」）。
// 发送 BYE 并关闭底层连接，触发库的收尾流程（session.Close、协议日志回填）。
func (s *IMAPServer) DisconnectByAddr(remoteAddr string) {
	if s == nil || remoteAddr == "" {
		return
	}
	s.mu.Lock()
	var targets []*imapSession
	for sess := range s.sessions {
		if sess.remoteAddr == remoteAddr {
			targets = append(targets, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range targets {
		_ = sess.conn.Bye("Connection closed by administrator")
	}
}

func (s *IMAPServer) tlsConfig() (*tls.Config, error) {
	if s.tlsLoader == nil {
		return nil, fmt.Errorf("IMAP TLS certificate or key not configured")
	}
	// GetCertificate 每次握手按需重载证书，证书更新后无需重启服务
	return &tls.Config{GetCertificate: s.tlsLoader.GetCertificate}, nil
}

// imapCaps 是服务器支持的能力集：UIDPLUS（RFC 4315）提供 UID EXPUNGE、
// COPYUID/APPENDUID 支持；MOVE 由 SessionMove 实现；IDLE/UNSELECT 在
// IMAP4rev1 认证后由库自动广告。
var imapCaps = imap.CapSet{
	imap.CapIMAP4rev1:   {},
	imap.CapUIDPlus:     {},
	imap.CapMove:        {},
	imap.CapLiteralPlus: {},
	imap.CapChildren:    {},
	imap.CapSpecialUse:  {},
}

// newServer 创建指定监听地址的 IMAP 服务（会话工厂捕获监听端口）。
func (s *IMAPServer) newServer(addr string, tlsConfig *tls.Config) *imapserver.Server {
	port := portOf(addr)
	srv := imapserver.New(&imapserver.Options{
		Caps:         imapCaps,
		NewSession:   s.newSessionFactory(port),
		TLSConfig:    tlsConfig,
		InsecureAuth: tlsConfig == nil,
	})
	return srv
}

// newSessionFactory 构造会话工厂：每个连接一个 imapSession，注册到
// 会话表（DisconnectByAddr 用）。
func (s *IMAPServer) newSessionFactory(port int) func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
	return func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
		sess := newImapSession(s, conn, port)
		s.mu.Lock()
		s.sessions[sess] = struct{}{}
		s.mu.Unlock()
		return sess, nil, nil
	}
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

// Start starts the IMAP server on the plain-text port (STARTTLS enabled
// when a certificate is configured).
func (s *IMAPServer) Start() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		log.Printf("IMAP STARTTLS 未启用: %v", err)
		tlsConfig = nil
	}
	srv := s.newServer(s.cfg.Addr, tlsConfig)
	log.Printf("IMAP server listening on %s", s.cfg.Addr)
	return srv.ListenAndServe(s.cfg.Addr)
}

// StartTLS starts the IMAP server on the implicit TLS port.
func (s *IMAPServer) StartTLS() error {
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}
	srv := s.newServer(s.cfg.TLSAddr, tlsConfig)
	log.Printf("IMAPS server listening on %s", s.cfg.TLSAddr)
	return srv.ListenAndServeTLS(s.cfg.TLSAddr)
}
