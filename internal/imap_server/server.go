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

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	imapserver "github.com/emersion/go-imap/server"
)

// Pusher 是 IMAP 实时推送接口：SMTP/POP3/Web 在邮件状态变化后调用，
// 由 go-imap 广播给相关客户端（按用户名+邮箱过滤，IDLE 时即时送达）。
type Pusher interface {
	// PushNewMessage 推送新邮件（本地投递成功）。
	PushNewMessage(userEmail string, msg *db.Message)
	// PushFlagsChanged 推送已读/星标等标志变化（MessageUpdate）。
	PushFlagsChanged(userEmail, mailbox string, msg *db.Message)
	// PushExpunged 推送邮件被删除（ExpungeUpdate，seqNums 为删除前序号）。
	PushExpunged(userEmail, mailbox string, seqNums []uint32)
}

// IMAPServer wraps a go-imap Server and provides mailbox access capability.
type IMAPServer struct {
	stores    *store.Stores
	cfg       config.IMAPConfig
	banCfg    config.BanConfig
	tlsLoader *tlsutil.Loader
	hub       *connhub.Hub

	beMu sync.Mutex
	bes  []*imapBackend       // 各监听器（明文/TLS）的 backend，用于新邮件推送
	srvs []*imapserver.Server // 各监听器实例，用于强制断开连接
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
func (s *IMAPServer) PushNewMessage(userEmail string, msg *db.Message) {
	if s == nil || userEmail == "" || msg == nil {
		return
	}
	update := buildNewMessageUpdate(s.stores, userEmail, "INBOX", msg)
	if update == nil {
		return
	}
	// 先发 EXISTS 通知再发 FETCH 更新：RFC 2177（IDLE）要求新邮件到达
	// 时服务器发送 EXISTS，不少客户端（如 Apple Mail）只认 EXISTS 才会
	// 唤醒并主动拉取，仅裸 FETCH 更新会被忽略（表现为"必须手动同步"）。
	if count, err := s.stores.Mails.CountByUserAndFolder(msg.UserID, "INBOX"); err == nil {
		s.pushExists(userEmail, "INBOX", uint32(count))
	}
	s.broadcastUpdate(update, userEmail, msg.ID)
}

// PushFlagsChanged 推送邮件标志（已读/星标等）变化给同用户的其他客户端。
func (s *IMAPServer) PushFlagsChanged(userEmail, mailbox string, msg *db.Message) {
	if s == nil || userEmail == "" || mailbox == "" || msg == nil {
		return
	}
	update := buildFlagsUpdate(s.stores, userEmail, mailbox, msg, false)
	if update == nil {
		return
	}
	s.broadcastUpdate(update, userEmail, msg.ID)
}

// PushExpunged 推送邮件被删除（每条序号一个 ExpungeUpdate）。
func (s *IMAPServer) PushExpunged(userEmail, mailbox string, seqNums []uint32) {
	if s == nil || userEmail == "" || mailbox == "" || len(seqNums) == 0 {
		return
	}
	for _, seq := range seqNums {
		update := &backend.ExpungeUpdate{
			Update: backend.NewUpdate(userEmail, mailbox),
			SeqNum: seq,
		}
		s.broadcastUpdate(update, userEmail, 0)
	}
}

// broadcastUpdate 把一条更新非阻塞地投递到所有监听器的推送通道。
// 每个监听器必须收到独立的 Update 对象（各自独立的 Done channel）：
// 每个监听器的 listenUpdates 都会对 update.Done() 执行 close，共享
// 同一对象会导致对同一 channel 二次 close 而 panic。
func (s *IMAPServer) broadcastUpdate(update backend.Update, userEmail string, msgID uint) {
	s.beMu.Lock()
	bes := append([]*imapBackend(nil), s.bes...)
	s.beMu.Unlock()

	for _, b := range bes {
		select {
		case b.updates <- cloneUpdate(update):
		default:
			log.Printf("IMAP: 推送通道已满，丢弃 %s 的更新 (msg=%d)", userEmail, msgID)
		}
	}
}

// pushExists 向所有监听器中「已登录该用户且已选中该邮箱」的连接直接写入
// 未请求的 "* N EXISTS" 响应（绕过 go-imap 更新通道——其仅支持 FETCH/
// EXPUNGE 类更新，无法表达 EXISTS）。通道满时非阻塞丢弃，与广播一致。
func (s *IMAPServer) pushExists(userEmail, mailbox string, exists uint32) {
	s.beMu.Lock()
	srvs := append([]*imapserver.Server(nil), s.srvs...)
	s.beMu.Unlock()

	for _, srv := range srvs {
		srv.ForEachConn(func(conn imapserver.Conn) {
			ctx := conn.Context()
			if ctx == nil || ctx.User == nil || ctx.Mailbox == nil {
				return
			}
			if ctx.User.Username() != userEmail || ctx.Mailbox.Name() != mailbox {
				return
			}
			select {
			case ctx.Responses <- existsResponse(exists):
			default:
				log.Printf("IMAP: EXISTS 推送通道已满，丢弃 (user=%s mailbox=%s)", userEmail, mailbox)
			}
		})
	}
}

// existsResponse 序列化为 "* N EXISTS\r\n"。
type existsResponse uint32

func (n existsResponse) WriteTo(w *imap.Writer) error {
	_, err := fmt.Fprintf(w, "* %d EXISTS\r\n", uint32(n))
	return err
}

// cloneUpdate 按类型复制一条 backend.Update：载荷（消息/序号）共享，
// 但 Username/Mailbox/Done channel 重置为独立实例。
func cloneUpdate(u backend.Update) backend.Update {
	switch u := u.(type) {
	case *backend.MessageUpdate:
		return &backend.MessageUpdate{
			Update:  backend.NewUpdate(u.Username(), u.Mailbox()),
			Message: u.Message,
		}
	case *backend.ExpungeUpdate:
		return &backend.ExpungeUpdate{
			Update: backend.NewUpdate(u.Username(), u.Mailbox()),
			SeqNum: u.SeqNum,
		}
	default:
		// 防御：未知类型原样传递（当前不存在此类更新）
		return u
	}
}

// registerBackend 记录新建的 backend（用于新邮件推送）。
func (s *IMAPServer) registerBackend(be *imapBackend) {
	s.beMu.Lock()
	s.bes = append(s.bes, be)
	s.beMu.Unlock()
}

// registerServer 记录监听器实例（用于强制断开连接）。
func (s *IMAPServer) registerServer(srv *imapserver.Server) {
	s.beMu.Lock()
	s.srvs = append(s.srvs, srv)
	s.beMu.Unlock()
}

// DisconnectByAddr 强制断开指定远端地址的连接（管理后台「断开并封禁」）。
// 关闭连接会触发 go-imap 的收尾流程（user.Logout、协议日志回填、hub 注销）。
func (s *IMAPServer) DisconnectByAddr(remoteAddr string) {
	if s == nil || remoteAddr == "" {
		return
	}
	s.beMu.Lock()
	srvs := append([]*imapserver.Server(nil), s.srvs...)
	s.beMu.Unlock()

	for _, srv := range srvs {
		srv.ForEachConn(func(conn imapserver.Conn) {
			info := conn.Info()
			if info != nil && info.RemoteAddr != nil && info.RemoteAddr.String() == remoteAddr {
				_ = conn.Close()
			}
		})
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
	be := &imapBackend{
		stores:         s.stores,
		banCfg:         s.banCfg,
		port:           portOf(addr),
		hub:            s.hub,
		updates:        make(chan backend.Update, 256),
		disconnectAddr: s.DisconnectByAddr,
	}
	s.registerBackend(be)
	srv := imapserver.New(be)
	srv.Addr = addr
	srv.TLSConfig = tlsConfig
	srv.AllowInsecureAuth = tlsConfig == nil
	s.registerServer(srv)
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
