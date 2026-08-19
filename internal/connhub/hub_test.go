package connhub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegisterListCountClose(t *testing.T) {
	h := New()

	c1 := h.Register("smtp", "10.0.0.1", 25, false)
	c2 := h.Register("imap", "10.0.0.2", 993, true)
	c3 := h.Register("pop3", "10.0.0.3", 110, false)

	if n := len(h.List()); n != 3 {
		t.Fatalf("list len = %d, want 3", n)
	}
	counts := h.Counts()
	if counts["smtp"] != 1 || counts["imap"] != 1 || counts["pop3"] != 1 {
		t.Fatalf("counts = %v", counts)
	}

	c2.SetUser("alice@example.com")
	c1.SetTLS(true)
	time.Sleep(time.Millisecond)
	c3.Touch()

	// 用户名 / TLS 状态生效
	var imapUser string
	for _, c := range h.List() {
		if c.Protocol == "imap" {
			imapUser = c.User
		}
		if c.Protocol == "smtp" && !c.TLS {
			t.Fatal("smtp conn should be TLS after SetTLS(true)")
		}
		if c.Protocol == "pop3" && !c.LastActive.After(c.Connected) {
			t.Fatal("pop3 conn LastActive should be after Connected after Touch")
		}
	}
	if imapUser != "alice@example.com" {
		t.Fatalf("imap user = %q", imapUser)
	}

	c1.Close()
	c2.Close()
	if n := len(h.List()); n != 1 {
		t.Fatalf("after close, len = %d, want 1", n)
	}
	if h.Counts()["imap"] != 0 {
		t.Fatalf("imap count after close = %d, want 0", h.Counts()["imap"])
	}
}

func TestNilHubSafe(t *testing.T) {
	var h *Hub
	if c := h.Register("smtp", "1.2.3.4", 25, false); c != nil {
		t.Fatal("nil hub Register must return nil")
	}
	if h.List() != nil || h.Counts() != nil {
		t.Fatal("nil hub List/Counts must be nil")
	}
	var c *Conn
	c.SetUser("x") // 不应 panic
	c.Touch()
	c.SetTLS(true)
	c.Close()
}

func TestConcurrentRegisterClose(t *testing.T) {
	h := New()
	const n = 50
	var wg sync.WaitGroup
	conns := make([]*Conn, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conns[i] = h.Register("smtp", "10.0.0.1", 25, false)
			conns[i].SetUser("u")
			conns[i].Touch()
		}(i)
	}
	wg.Wait()

	if len(h.List()) != n {
		t.Fatalf("len = %d, want %d", len(h.List()), n)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conns[i].Close()
		}(i)
	}
	wg.Wait()
	if len(h.List()) != 0 {
		t.Fatalf("after concurrent close, len = %d, want 0", len(h.List()))
	}
}

func TestListOrderedByID(t *testing.T) {
	h := New()
	h.Register("smtp", "1.1.1.1", 25, false)
	h.Register("imap", "2.2.2.2", 143, false)
	h.Register("pop3", "3.3.3.3", 110, false)
	list := h.List()
	for i := 1; i < len(list); i++ {
		if list[i].ID <= list[i-1].ID {
			t.Fatalf("list not ordered by ID: %+v", list)
		}
	}
}

// TestDisconnect 验证强制断开回调被调用。
func TestDisconnect(t *testing.T) {
	h := New()
	var closed atomic.Int32

	c1 := h.Register("smtp", "10.0.0.1", 25, false)
	c1.SetDisconnect(func() { closed.Add(1) })
	c2 := h.Register("imap", "10.0.0.2", 993, true)
	c2.SetDisconnect(func() { closed.Add(1) })

	if !h.Disconnect(c1.ID) {
		t.Fatal("Disconnect should report success")
	}
	if closed.Load() != 1 {
		t.Fatalf("closed = %d, want 1", closed.Load())
	}
	// 已断开（未注销）仍可查到
	if _, ok := h.Get(c1.ID); !ok {
		t.Fatal("conn should still be registered until Close")
	}
	// 不存在的 ID
	if h.Disconnect(99999) {
		t.Fatal("Disconnect of unknown id must fail")
	}
}

// TestDisconnectByIP 验证封禁时断开该 IP 全部连接。
func TestDisconnectByIP(t *testing.T) {
	h := New()
	var closed atomic.Int32

	// 同一 IP 三个协议连接
	for _, proto := range []string{"smtp", "imap", "pop3"} {
		c := h.Register(proto, "203.0.113.5", 25, false)
		c.SetDisconnect(func() { closed.Add(1) })
	}
	// 另一 IP 不受影响
	other := h.Register("smtp", "203.0.113.6", 25, false)
	other.SetDisconnect(func() { closed.Add(1) })

	n := h.DisconnectByIP("203.0.113.5")
	if n != 3 {
		t.Fatalf("disconnected = %d, want 3", n)
	}
	if closed.Load() != 3 {
		t.Fatalf("closed = %d, want 3", closed.Load())
	}
}

// TestDisconnectNoCallback 验证未注册断开回调的连接安全跳过。
func TestDisconnectNoCallback(t *testing.T) {
	h := New()
	c := h.Register("pop3", "10.0.0.9", 110, false)
	if !h.Disconnect(c.ID) {
		t.Fatal("Disconnect should report success even without callback")
	}
	if n := h.DisconnectByIP("10.0.0.9"); n != 0 {
		t.Fatalf("disconnected = %d, want 0", n)
	}
}
