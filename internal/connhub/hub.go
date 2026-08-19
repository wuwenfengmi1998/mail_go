// Package connhub 提供邮件协议（SMTP/IMAP/POP3）当前活动连接的注册中心，
// 供管理后台实时查看连接情况（来源 IP、用户、TLS、时长等）。
package connhub

import (
	"sort"
	"sync"
	"time"
)

// Conn 表示一个活动中的协议连接。字段由 Hub 的锁保护。
type Conn struct {
	ID         uint64 // 自增序号
	Protocol   string // smtp | imap | pop3
	IP         string
	Port       int
	User       string // 认证后填充
	TLS        bool
	Connected  time.Time
	LastActive time.Time

	hub *Hub
	// disconnect 强制断开底层连接的回调（由各协议服务器注册）。
	// 关闭底层 socket 后协议服务器会正常走收尾清理（Logout/注销）。
	disconnect func()
}

// Hub 管理所有活动连接（同一把锁保护注册表与连接字段）。
type Hub struct {
	mu    sync.Mutex
	seq   uint64
	conns map[uint64]*Conn
}

// New 创建连接注册中心。
func New() *Hub {
	return &Hub{conns: make(map[uint64]*Conn)}
}

// Register 注册一个新连接并返回其句柄；调用方在连接结束时调用 Close()。
func (h *Hub) Register(protocol, ip string, port int, tls bool) *Conn {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	now := time.Now()
	c := &Conn{
		ID:         h.seq,
		Protocol:   protocol,
		IP:         ip,
		Port:       port,
		TLS:        tls,
		Connected:  now,
		LastActive: now,
		hub:        h,
	}
	h.conns[c.ID] = c
	return c
}

// SetUser 记录认证成功的用户名（邮箱）并刷新最后活跃时间。
func (c *Conn) SetUser(u string) {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	c.User = u
	c.LastActive = time.Now()
	c.hub.mu.Unlock()
}

// SetTLS 更新连接的 TLS 状态（如 POP3 STLS 升级之后）。
func (c *Conn) SetTLS(on bool) {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	c.TLS = on
	c.hub.mu.Unlock()
}

// Touch 刷新最后活跃时间。
func (c *Conn) Touch() {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	c.LastActive = time.Now()
	c.hub.mu.Unlock()
}

// SetDisconnect 注册强制断开底层连接的回调（管理后台「断开并封禁」用）。
func (c *Conn) SetDisconnect(fn func()) {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	c.disconnect = fn
	c.hub.mu.Unlock()
}

// Close 从注册中心移除该连接。
func (c *Conn) Close() {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	delete(c.hub.conns, c.ID)
	c.hub.mu.Unlock()
}

// Get 按 ID 查找活动连接。
func (h *Hub) Get(id uint64) (*Conn, bool) {
	if h == nil {
		return nil, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.conns[id]
	return c, ok
}

// Disconnect 强制断开指定连接（关闭底层连接并注销）。
func (h *Hub) Disconnect(id uint64) bool {
	c, ok := h.Get(id)
	if !ok {
		return false
	}
	h.mu.Lock()
	fn := c.disconnect
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
	return true
}

// DisconnectByIP 强制断开该 IP 的全部连接，返回断开的连接数。
// 用于封禁 IP 后立即踢掉其所有在线会话。
func (h *Hub) DisconnectByIP(ip string) int {
	if h == nil || ip == "" {
		return 0
	}
	h.mu.Lock()
	var fns []func()
	for _, c := range h.conns {
		if c.IP == ip && c.disconnect != nil {
			fns = append(fns, c.disconnect)
		}
	}
	h.mu.Unlock()

	for _, fn := range fns {
		fn()
	}
	return len(fns)
}

// List 返回当前所有活动连接（拷贝），按连接时间升序。
func (h *Hub) List() []Conn {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	out := make([]Conn, 0, len(h.conns))
	for _, c := range h.conns {
		out = append(out, Conn{
			ID:         c.ID,
			Protocol:   c.Protocol,
			IP:         c.IP,
			Port:       c.Port,
			User:       c.User,
			TLS:        c.TLS,
			Connected:  c.Connected,
			LastActive: c.LastActive,
		})
	}
	h.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Counts 返回按协议分组的当前连接数。
func (h *Hub) Counts() map[string]int {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	counts := make(map[string]int)
	for _, c := range h.conns {
		counts[c.Protocol]++
	}
	return counts
}
