package outbound

import (
	"fmt"
	"log"
	"net/mail"
	"strings"
	"sync"
	"time"

	"mail_go/config"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/google/uuid"
)

// Manager orchestrates the outbound delivery queue: enqueueing messages,
// background delivery worker, exponential backoff retries, DKIM signing,
// per-user rate limits and failure bounces.
type Manager struct {
	cfg      config.OutboundConfig
	hostname string // EHLO hostname
	mailer   *Mailer
	stores   *store.Stores

	kick  chan struct{}
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	wg    sync.WaitGroup
	mu    sync.Mutex
	lim   map[uint]*userWindow
	batch int
}

// userWindow tracks a user's sending rate within fixed windows.
type userWindow struct {
	minuteStart time.Time
	minuteCount int
	dayStart    time.Time
	dayCount    int
}

// NewManager creates an outbound delivery Manager.
// hostname is the EHLO name presented to remote servers (defaults to "localhost").
func NewManager(cfg config.OutboundConfig, hostname string, stores *store.Stores) *Manager {
	m := &Manager{
		cfg:      cfg,
		hostname: hostname,
		mailer:   NewMailer(hostname, time.Duration(cfg.ConnectTimeout)*time.Second),
		stores:   stores,
		kick:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		lim:      make(map[uint]*userWindow),
		batch:    50,
	}
	m.mailer.IPFamily = cfg.IPFamily
	m.mailer.SourceIP = cfg.SourceIP
	if cfg.SourceIP != "" {
		log.Printf("outbound: binding source address %s (ip_family=%s)", cfg.SourceIP, cfg.IPFamily)
	}

	if cfg.RelayHost != "" {
		m.mailer.Relay = &RelayConfig{
			Host:     cfg.RelayHost,
			Port:     cfg.RelayPort,
			Username: cfg.RelayUser,
			Password: cfg.RelayPassword,
			StartTLS: cfg.RelayStartTLS,
		}
		log.Printf("outbound: using smarthost relay %s:%d", cfg.RelayHost, cfg.RelayPort)
	}
	return m
}

// Start launches the background delivery worker.
func (m *Manager) Start() {
	interval := time.Duration(m.cfg.PollInterval) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		log.Printf("outbound: delivery worker started (interval=%s, max_attempts=%d)", interval, m.cfg.MaxAttempts)
		for {
			select {
			case <-ticker.C:
				m.processDue()
			case <-m.kick:
				m.processDue()
			case <-m.stop:
				close(m.done)
				return
			}
		}
	}()
}

// Stop gracefully stops the delivery worker.
func (m *Manager) Stop() {
	m.once.Do(func() {
		close(m.stop)
	})
	<-m.done
	m.wg.Wait()
}

// kickWorker nudges the worker to scan the queue immediately.
func (m *Manager) kickWorker() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// Enabled reports whether external delivery is configured on.
func (m *Manager) Enabled() bool {
	return m.cfg.MaxPerDay > 0
}

// MaxRecipients returns the maximum number of external recipients allowed
// per message (0 means unlimited).
func (m *Manager) MaxRecipients() int {
	return m.cfg.MaxRecipients
}

// Enqueue validates a sender/recipient pair, DKIM-signs the message once and
// stores it in the outbound queue for background delivery. The recipient must
// NOT be a local address — callers decide local vs external routing.
// Returns a permanent-style error for invalid input or rate-limit violations.
func (m *Manager) Enqueue(senderUser *db.User, from, to string, raw []byte) (*db.OutboundMessage, error) {
	if !m.Enabled() {
		return nil, fmt.Errorf("外部投递未启用")
	}

	to = strings.TrimSpace(to)
	addr, err := mail.ParseAddress(to)
	if err != nil {
		return nil, fmt.Errorf("收件人地址无效: %s", to)
	}
	to = addr.Address

	at := strings.LastIndex(to, "@")
	if at < 0 || at == len(to)-1 {
		return nil, fmt.Errorf("收件人地址无效: %s", to)
	}
	recipientDom := strings.ToLower(to[at+1:])

	// Local addresses must never enter the outbound queue.
	if _, err := m.stores.Users.GetByEmail(to); err == nil {
		return nil, fmt.Errorf("收件人 %s 是本地地址，应走本地投递", to)
	}

	// Rate limiting per sender user.
	if senderUser != nil {
		if err := m.checkRateLimit(senderUser.ID); err != nil {
			return nil, err
		}
	}

	// DKIM-sign once with the sender domain's key.
	signed, err := m.signForSender(from, raw)
	if err != nil {
		log.Printf("outbound: DKIM signing failed for %s: %v", from, err)
		signed = raw
	}

	item := &db.OutboundMessage{
		MessageID:     fmt.Sprintf("<%s@outbound>", uuid.New().String()),
		UserID:        userIDOrZero(senderUser),
		FromAddr:      from,
		ToAddr:        to,
		RecipientDom:  recipientDom,
		RawData:       string(signed),
		Status:        db.OutboundStatusPending,
		Attempts:      0,
		NextAttemptAt: time.Now(),
	}
	if err := m.stores.Outbound.Create(item); err != nil {
		return nil, fmt.Errorf("写入外发队列失败: %w", err)
	}

	log.Printf("outbound: queued %s -> %s (id=%d)", from, to, item.ID)
	m.kickWorker()
	return item, nil
}

func userIDOrZero(u *db.User) uint {
	if u == nil {
		return 0
	}
	return u.ID
}

// signForSender looks up the sender domain's DKIM key and signs the message.
func (m *Manager) signForSender(from string, raw []byte) ([]byte, error) {
	at := strings.LastIndex(from, "@")
	if at < 0 || at == len(from)-1 {
		return raw, fmt.Errorf("无效发件人地址: %s", from)
	}
	domName := strings.ToLower(from[at+1:])

	domain, err := m.stores.Domains.GetByName(domName)
	if err != nil {
		return raw, nil // domain not managed locally; send unsigned
	}
	return SignDKIM(raw, domain.Name, domain.DkimSelector, domain.DkimPrivateKey)
}

// checkRateLimit enforces per-user minute/day sending limits.
func (m *Manager) checkRateLimit(userID uint) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	w := m.lim[userID]
	if w == nil || now.Sub(w.dayStart) >= 24*time.Hour {
		w = &userWindow{minuteStart: now, dayStart: now}
		m.lim[userID] = w
	} else if now.Sub(w.minuteStart) >= time.Minute {
		w.minuteStart = now
		w.minuteCount = 0
	}

	if m.cfg.MaxPerMin > 0 && w.minuteCount >= m.cfg.MaxPerMin {
		log.Printf("outbound: rate limit exceeded (per minute) for user %d", userID)
		return fmt.Errorf("发送频率超限：每分钟最多 %d 封", m.cfg.MaxPerMin)
	}
	if m.cfg.MaxPerDay > 0 && w.dayCount >= m.cfg.MaxPerDay {
		log.Printf("outbound: rate limit exceeded (per day) for user %d", userID)
		return fmt.Errorf("发送频率超限：每日最多 %d 封", m.cfg.MaxPerDay)
	}

	w.minuteCount++
	w.dayCount++
	return nil
}

// processDue attempts delivery of all due queue items.
func (m *Manager) processDue() {
	items, err := m.stores.Outbound.ListDue(time.Now(), m.batch)
	if err != nil {
		log.Printf("outbound: loading due queue failed: %v", err)
		return
	}
	for i := range items {
		m.deliverOne(&items[i])
	}
}

// deliverOne performs a single delivery attempt for a queue item.
func (m *Manager) deliverOne(item *db.OutboundMessage) {
	// Mark as sending to avoid concurrent workers double-delivering.
	item.Status = db.OutboundStatusSending
	if err := m.stores.Outbound.Update(item); err != nil {
		log.Printf("outbound: update item %d to sending failed: %v", item.ID, err)
		return
	}

	resp, err := m.mailer.Deliver(item.FromAddr, item.ToAddr, []byte(item.RawData))

	now := time.Now()
	item.Attempts++

	if err == nil {
		item.Status = db.OutboundStatusSent
		item.LastResponse = resp
		item.LastError = ""
		item.CompletedAt = &now
		if saveErr := m.stores.Outbound.Update(item); saveErr != nil {
			log.Printf("outbound: update item %d to sent failed: %v", item.ID, saveErr)
			return
		}
		log.Printf("outbound: delivered %s -> %s (id=%d, attempts=%d)", item.FromAddr, item.ToAddr, item.ID, item.Attempts)
		return
	}

	var de *DeliveryError
	permanent := false
	if ok := asDeliveryError(err, &de); ok {
		permanent = de.Permanent
	}
	item.LastResponse = ""
	item.LastError = err.Error()

	if permanent || item.Attempts >= m.cfg.MaxAttempts {
		item.Status = db.OutboundStatusFailed
		item.CompletedAt = &now
		if saveErr := m.stores.Outbound.Update(item); saveErr != nil {
			log.Printf("outbound: update item %d to failed failed: %v", item.ID, saveErr)
			return
		}
		log.Printf("outbound: permanent failure %s -> %s (id=%d): %v", item.FromAddr, item.ToAddr, item.ID, err)
		m.bounce(item, err)
		return
	}

	// Temporary failure: exponential backoff retry.
	item.Status = db.OutboundStatusDeferred
	backoff := time.Duration(m.cfg.RetryBaseMin) * time.Minute
	backoff <<= (item.Attempts - 1)
	if backoff > 24*time.Hour {
		backoff = 24 * time.Hour
	}
	item.NextAttemptAt = now.Add(backoff)
	if saveErr := m.stores.Outbound.Update(item); saveErr != nil {
		log.Printf("outbound: update item %d to deferred failed: %v", item.ID, saveErr)
		return
	}
	log.Printf("outbound: temporary failure %s -> %s (id=%d, attempt=%d, retry in %s): %v",
		item.FromAddr, item.ToAddr, item.ID, item.Attempts, backoff, err)
}

// bounce delivers a non-delivery notice to the sender's INBOX.
func (m *Manager) bounce(item *db.OutboundMessage, deliveryErr error) {
	sender, err := m.stores.Users.GetByEmail(item.FromAddr)
	if err != nil {
		log.Printf("outbound: cannot bounce %s: sender is not a local user", item.FromAddr)
		return
	}

	now := time.Now()
	postmaster := "Mail Delivery System <postmaster@" + m.hostname + ">"
	subject := fmt.Sprintf("邮件投递失败: %s", item.ToAddr)

	body := "这是一封系统退信通知。\r\n\r\n" +
		fmt.Sprintf("您的邮件未能投递到以下收件人：\r\n\r\n  收件人：%s\r\n  失败原因：%s\r\n  投递时间：%s\r\n  尝试次数：%d\r\n\r\n",
			item.ToAddr, deliveryErr.Error(), now.Format("2006-01-02 15:04:05"), item.Attempts) +
		"如果收件人地址无误，请稍后重试；连续失败可能表示收件地址不存在或对方服务器拒收。\r\n"

	msg := &db.Message{
		UserID:    sender.ID,
		MessageID: fmt.Sprintf("<bounce-%d@%s>", item.ID, m.hostname),
		Folder:    "INBOX",
		FromAddr:  postmaster,
		ToAddr:    item.FromAddr,
		Subject:   subject,
		TextBody:  body,
		Date:      now,
		IsRead:    false,
	}
	if err := m.stores.Mails.Create(msg); err != nil {
		log.Printf("outbound: bounce message creation failed: %v", err)
		return
	}
	log.Printf("outbound: bounce delivered to %s for failed delivery of %s", item.FromAddr, item.ToAddr)
}

// Retry resets a failed/deferred queue item for immediate redelivery.
func (m *Manager) Retry(id uint) error {
	item, err := m.stores.Outbound.GetByID(id)
	if err != nil {
		return err
	}
	item.Status = db.OutboundStatusPending
	item.Attempts = 0
	item.LastError = ""
	item.LastResponse = ""
	item.CompletedAt = nil
	item.NextAttemptAt = time.Now()
	if err := m.stores.Outbound.Update(item); err != nil {
		return err
	}
	m.kickWorker()
	return nil
}

// Cancel marks a queue item as canceled by the administrator.
func (m *Manager) Cancel(id uint) error {
	item, err := m.stores.Outbound.GetByID(id)
	if err != nil {
		return err
	}
	if item.Status == db.OutboundStatusSent {
		return fmt.Errorf("已送达的邮件无法取消")
	}
	now := time.Now()
	item.Status = db.OutboundStatusCanceled
	item.LastError = "管理员取消"
	item.CompletedAt = &now
	return m.stores.Outbound.Update(item)
}

// asDeliveryError extracts a *DeliveryError from an error chain.
func asDeliveryError(err error, target **DeliveryError) bool {
	for err != nil {
		if de, ok := err.(*DeliveryError); ok {
			*target = de
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// StatusText returns a human-readable Chinese label for a queue status.
func StatusText(status string) string {
	switch status {
	case db.OutboundStatusPending:
		return "待发送"
	case db.OutboundStatusSending:
		return "发送中"
	case db.OutboundStatusSent:
		return "已送达"
	case db.OutboundStatusDeferred:
		return "等待重试"
	case db.OutboundStatusFailed:
		return "失败"
	case db.OutboundStatusCanceled:
		return "已取消"
	default:
		return status
	}
}
