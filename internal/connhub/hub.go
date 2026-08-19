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

// Close 从注册中心移除该连接。
func (c *Conn) Close() {
	if c == nil || c.hub == nil {
		return
	}
	c.hub.mu.Lock()
	delete(c.hub.conns, c.ID)
	c.hub.mu.Unlock()
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
