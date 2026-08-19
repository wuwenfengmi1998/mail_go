package imap_server

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"time"

	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/mailutil"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	asgomail "github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"
)

// ---------- 跨会话推送 ----------

// sessionUpdate 是一条待下发给客户端的邮箱状态更新。
type sessionUpdate struct {
	expunge *uint32 // EXPUNGE 序号
	exists  *uint32 // EXISTS 计数
	fetch   *sessionFetchUpdate
}

type sessionFetchUpdate struct {
	seq   uint32
	uid   imap.UID
	flags []imap.Flag
}

// mailboxHub 跟踪一个（用户+文件夹）邮箱的所有已选中会话，把其他会话
// 产生的变化（EXISTS/EXPUNGE/FLAGS）推送给其余会话（排除来源会话）。
// 自行实现而非复用 imapserver.MailboxTracker：库版本对 EXPUNGE/EXISTS
// 无法排除来源会话，会导致本会话 EXPUNGE 时给自己排队重复响应。
type mailboxHub struct {
	mu       sync.Mutex
	sessions map[*imapSession]struct{}
}

func newMailboxHub() *mailboxHub {
	return &mailboxHub{sessions: make(map[*imapSession]struct{})}
}

func (h *mailboxHub) add(s *imapSession) {
	h.mu.Lock()
	h.sessions[s] = struct{}{}
	h.mu.Unlock()
}

func (h *mailboxHub) remove(s *imapSession) {
	h.mu.Lock()
	delete(h.sessions, s)
	h.mu.Unlock()
}

// enqueue 把更新分发给除 source 外的所有会话。
func (h *mailboxHub) enqueue(u sessionUpdate, source *imapSession) {
	h.mu.Lock()
	targets := make([]*imapSession, 0, len(h.sessions))
	for s := range h.sessions {
		if s != source {
			targets = append(targets, s)
		}
	}
	h.mu.Unlock()
	for _, s := range targets {
		s.pushUpdate(u)
	}
}

// ---------- imapSession ----------

// imapSession 实现 imapserver.Session 与 imapserver.SessionMove。
// 每个连接一个会话实例；所有命令串行执行，跨会话更新经 mailboxHub 推送。
type imapSession struct {
	srv  *IMAPServer
	conn *imapserver.Conn

	port       int    // 监听端口（协议日志）
	remoteAddr string // 远端地址字符串（封禁/断开用）

	mu        sync.Mutex
	userID    uint
	email     string
	logID     uint
	startedAt time.Time
	clientIP  string
	hubConn   *connhub.Conn

	// selected state
	selected string
	readOnly bool
	hub      *mailboxHub

	// 待下发的推送队列（Idle/Poll 时写出）
	queue  []sessionUpdate
	notify chan struct{}
}

func newImapSession(srv *IMAPServer, conn *imapserver.Conn, port int) *imapSession {
	s := &imapSession{
		srv:       srv,
		conn:      conn,
		port:      port,
		startedAt: time.Now(),
		notify:    make(chan struct{}, 1),
	}
	if nc := conn.NetConn(); nc != nil && nc.RemoteAddr() != nil {
		s.remoteAddr = nc.RemoteAddr().String()
	}
	return s
}

// pushUpdate 入队一条推送并唤醒 Idle（非阻塞，队列共享锁保护）。
func (s *imapSession) pushUpdate(u sessionUpdate) {
	s.mu.Lock()
	s.queue = append(s.queue, u)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// takeUpdates 取出待下发更新；allowExpunge=false 时遇到 EXPUNGE 停止
// （与 RFC 要求一致：FETCH/STORE/SEARCH 期间不能下发 EXPUNGE）。
func (s *imapSession) takeUpdates(allowExpunge bool) []sessionUpdate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if allowExpunge {
		updates := s.queue
		s.queue = nil
		return updates
	}
	stop := -1
	for i, u := range s.queue {
		if u.expunge != nil {
			stop = i
			break
		}
	}
	if stop < 0 {
		updates := s.queue
		s.queue = nil
		return updates
	}
	updates := s.queue[:stop]
	s.queue = s.queue[stop:]
	return updates
}

// writeUpdates 把更新按类型写入连接。
func writeUpdates(w *imapserver.UpdateWriter, updates []sessionUpdate) error {
	for _, u := range updates {
		switch {
		case u.expunge != nil:
			if err := w.WriteExpunge(*u.expunge); err != nil {
				return err
			}
		case u.exists != nil:
			if err := w.WriteNumMessages(*u.exists); err != nil {
				return err
			}
		case u.fetch != nil:
			if err := w.WriteMessageFlags(u.fetch.seq, u.fetch.uid, u.fetch.flags); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- Session 基础 ----------

// Close 在连接结束时调用：注销推送、关闭连接中心记录、回填协议日志。
func (s *imapSession) Close() error {
	s.mu.Lock()
	hub := s.hub
	s.hub = nil
	hubConn := s.hubConn
	s.hubConn = nil
	logID := s.logID
	startedAt := s.startedAt
	s.mu.Unlock()

	if hub != nil {
		hub.remove(s)
	}
	if hubConn != nil {
		hubConn.Close()
	}
	if logID != 0 {
		s.srv.stores.ProtocolLogs.UpdateDuration(logID, time.Since(startedAt).Milliseconds())
	}
	s.srv.unregisterSession(s)
	return nil
}

// Login 认证：封禁检查 → 认证 → 失败计数 → 协议日志 → 连接中心注册。
// 与 v1 imapBackend.Login 完全一致的策略（含裸用户名支持、成功后清零失败计数）。
func (s *imapSession) Login(username, password string) error {
	clientIP := store.ClientIPFromAddr(s.conn.NetConn().RemoteAddr())
	now := time.Now()

	if banned, _ := s.srv.stores.Bans.IsBanned(clientIP); banned {
		s.recordLogin(clientIP, username, false, "IP已被封禁", "认证被拒绝（IP 已封禁）", now)
		return imapserver.ErrAuthFailed
	}

	user, err := s.srv.stores.Users.AuthenticateLogin(username, password)
	if err != nil {
		s.srv.stores.RecordAuthFailure(clientIP, s.srv.banCfg.MaxFailAttempts, s.srv.banCfg.BanDurationMin, "邮件协议认证失败次数过多")
		s.recordLogin(clientIP, username, false, "用户名或密码错误", "LOGIN 失败", now)
		return imapserver.ErrAuthFailed
	}

	// 登录成功清零失败计数（与 Web 登录一致），避免协议客户端失败计数
	// 只增不减（配置探测、APP 裸用户名重试等）累计触发误封。
	s.srv.stores.Bans.ResetFail(clientIP)

	email := user.Username + "@"
	if domain, err := s.srv.stores.Domains.GetByID(user.DomainID); err == nil {
		email = user.Username + "@" + domain.Name
	}

	logID := s.recordLogin(clientIP, username, true, "", "LOGIN 成功", now)

	// STARTTLS 后连接对象会被库替换为 tls.Conn，此处按当前实际加密状态标记。
	tlsOn := false
	if _, ok := s.conn.NetConn().(*tls.Conn); ok {
		tlsOn = true
	}
	conn := s.srv.hub.Register("imap", clientIP, s.port, tlsOn)
	if conn != nil {
		conn.SetUser(email)
		if s.remoteAddr != "" {
			addr := s.remoteAddr
			conn.SetDisconnect(func() { s.srv.DisconnectByAddr(addr) })
		}
	}

	s.mu.Lock()
	s.userID = user.ID
	s.email = email
	s.logID = logID
	s.clientIP = clientIP
	s.hubConn = conn
	s.mu.Unlock()
	return nil
}

// recordLogin 写入一条 IMAP 协议日志，返回记录 ID（失败为 0）。
func (s *imapSession) recordLogin(ip, username string, success bool, failReason, detail string, at time.Time) uint {
	entry := &db.ProtocolLog{
		Protocol:   db.ProtocolIMAP,
		Port:       s.port,
		ClientIP:   ip,
		Username:   username,
		Success:    success,
		FailReason: failReason,
		Detail:     detail,
		CreatedAt:  at,
	}
	if err := s.srv.stores.ProtocolLogs.Create(entry); err != nil {
		log.Printf("IMAP: 写入协议日志失败: %v", err)
		return 0
	}
	return entry.ID
}

// systemMailboxes 是系统支持的全部文件夹。
var systemMailboxes = []struct {
	name  string
	attrs []imap.MailboxAttr
}{
	{"INBOX", nil},
	{"Sent", []imap.MailboxAttr{imap.MailboxAttrSent}},
	{"Drafts", []imap.MailboxAttr{imap.MailboxAttrDrafts}},
	{"Trash", []imap.MailboxAttr{imap.MailboxAttrTrash}},
}

// matchPattern 按 RFC 3501 LIST wildcard 语义匹配（* 任意、% 不跨分隔符）。
// INBOX 大小写不敏感，其余文件夹按系统定义大小写精确匹配。
func matchPattern(name, pattern string) bool {
	var sb strings.Builder
	sb.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			sb.WriteString(".*")
		case '%':
			sb.WriteString("[^/]*")
		default:
			sb.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	sb.WriteString("$")
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false
	}
	if strings.EqualFold(name, "INBOX") {
		return re.MatchString(strings.ToUpper(name))
	}
	return re.MatchString(name)
}

// matchAnyPattern 判断名字是否命中任意一个 pattern。
func matchAnyPattern(name string, patterns []string) bool {
	for _, p := range patterns {
		if matchPattern(name, p) {
			return true
		}
	}
	return false
}

// List 返回系统文件夹列表（LSUB 同样返回全部——订阅状态恒为已订阅）。
func (s *imapSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *imap.ListOptions) error {
	for _, mb := range systemMailboxes {
		if !matchAnyPattern(mb.name, patterns) {
			continue
		}
		data := &imap.ListData{
			Attrs:   mb.attrs,
			Delim:   '/',
			Mailbox: mb.name,
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}

// Create 不支持。
func (s *imapSession) Create(mailbox string, options *imap.CreateOptions) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "mailbox creation not supported"}
}

// Delete 不支持。
func (s *imapSession) Delete(mailbox string) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "mailbox deletion not supported"}
}

// Rename 不支持。
func (s *imapSession) Rename(mailbox, newName string, options *imap.RenameOptions) error {
	return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "mailbox rename not supported"}
}

// Subscribe 恒为已订阅（no-op）。
func (s *imapSession) Subscribe(mailbox string) error {
	return nil
}

// Unsubscribe no-op。
func (s *imapSession) Unsubscribe(mailbox string) error {
	return nil
}

// ---------- 选中状态 ----------

// Select 选中邮箱：登记 mailboxHub 会话，返回状态数据。
func (s *imapSession) Select(mailbox string, options *imap.SelectOptions) (*imap.SelectData, error) {
	name, ok := canonicalMailboxName(mailbox)
	if !ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "No such mailbox"}
	}

	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	var unseen uint32
	for i := range msgs {
		if !msgs[i].IsRead {
			unseen++
		}
	}
	maxID, err := s.srv.stores.Mails.MaxIDByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	uidValidity, err := s.srv.stores.MailboxState.UidValidity(userID, name)
	if err != nil {
		log.Printf("IMAP: 获取 UIDVALIDITY 失败 folder=%s: %v", name, err)
		uidValidity = 1
	}

	// 换绑推送（若此前已选中，先注销旧邮箱）
	s.mu.Lock()
	oldHub := s.hub
	s.hub = nil
	s.mu.Unlock()
	if oldHub != nil {
		oldHub.remove(s)
	}
	hub := s.srv.hubForOrCreate(s.currentEmail(), name)
	hub.add(s)

	s.mu.Lock()
	s.selected = name
	s.readOnly = options.ReadOnly
	s.hub = hub
	s.mu.Unlock()

	flags := []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagSeen, imap.FlagDraft}
	return &imap.SelectData{
		Flags:          flags,
		PermanentFlags: append(flags, imap.FlagWildcard),
		NumMessages:    uint32(len(msgs)),
		NumRecent:      0,
		UIDNext:        imap.UID(maxID + 1),
		UIDValidity:    uidValidity,
	}, nil
}

// Unselect 取消选中：注销 mailboxHub 会话。
func (s *imapSession) Unselect() error {
	s.mu.Lock()
	hub := s.hub
	s.hub = nil
	s.selected = ""
	s.readOnly = false
	s.queue = nil
	s.mu.Unlock()
	if hub != nil {
		hub.remove(s)
	}
	return nil
}

// Status 返回邮箱状态（计数/未读/UIDNEXT/UIDVALIDITY/已删除标记数/大小）。
func (s *imapSession) Status(mailbox string, options *imap.StatusOptions) (*imap.StatusData, error) {
	name, ok := canonicalMailboxName(mailbox)
	if !ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "No such mailbox"}
	}
	userID := s.currentUserID()

	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, name)
	if err != nil {
		return nil, err
	}
	data := &imap.StatusData{Mailbox: name}

	if options.NumMessages || options.NumUnseen || options.NumDeleted || options.Size {
		var unseen, deleted uint32
		var size int64
		for i := range msgs {
			if !msgs[i].IsRead {
				unseen++
			}
			if msgs[i].IsDeleted {
				deleted++
			}
			size += int64(len(messageRawData(&msgs[i])))
		}
		if options.NumMessages {
			n := uint32(len(msgs))
			data.NumMessages = &n
		}
		if options.NumUnseen {
			data.NumUnseen = &unseen
		}
		if options.NumDeleted {
			data.NumDeleted = &deleted
		}
		if options.Size {
			data.Size = &size
		}
	}
	if options.NumRecent {
		zero := uint32(0)
		data.NumRecent = &zero
	}
	if options.UIDNext {
		maxID, err := s.srv.stores.Mails.MaxIDByUserAndFolder(userID, name)
		if err != nil {
			return nil, err
		}
		data.UIDNext = imap.UID(maxID + 1)
	}
	if options.UIDValidity {
		uidValidity, err := s.srv.stores.MailboxState.UidValidity(userID, name)
		if err != nil {
			return nil, err
		}
		data.UIDValidity = uidValidity
	}
	return data, nil
}

// ---------- 消息操作 ----------

// numSetContains 判断消息是否命中序号/UID 集合（含 "*" 动态上界）。
func numSetContains(numSet imap.NumSet, seq uint32, uid imap.UID, maxSeq, maxUID uint32) bool {
	switch set := numSet.(type) {
	case imap.SeqSet:
		for _, r := range set {
			stop := r.Stop
			if stop == 0 {
				stop = maxSeq
			}
			if seq >= r.Start && seq <= stop {
				return true
			}
		}
	case imap.UIDSet:
		for _, r := range set {
			stop := r.Stop
			if stop == 0 {
				stop = imap.UID(maxUID)
			}
			if uid >= r.Start && uid <= stop {
				return true
			}
		}
	}
	return false
}

// Fetch 下发匹配消息的 FETCH 响应。
func (s *imapSession) Fetch(w *imapserver.FetchWriter, numSet imap.NumSet, options *imap.FetchOptions) error {
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return err
	}
	maxSeq := uint32(len(msgs))
	var maxUID uint32
	for i := range msgs {
		if uint32(msgs[i].ID) > maxUID {
			maxUID = uint32(msgs[i].ID)
		}
	}

	for i := range msgs {
		msg := &msgs[i]
		seq := uint32(i + 1)
		uid := imap.UID(msg.ID)
		if !numSetContains(numSet, seq, uid, maxSeq, maxUID) {
			continue
		}

		raw := messageRawData(msg)
		fw := w.CreateMessage(seq)
		if options.UID {
			fw.WriteUID(uid)
		}
		if options.Flags {
			fw.WriteFlags(flagsOf(msg.IsRead, msg.IsFlagged, msg.IsDeleted))
		}
		if options.RFC822Size {
			fw.WriteRFC822Size(int64(len(raw)))
		}
		if options.InternalDate {
			fw.WriteInternalDate(msg.Date)
		}
		if options.Envelope {
			var env *imap.Envelope
			if hdr, _, err := headerAndBody(raw); err == nil {
				env = imapserver.ExtractEnvelope(hdr)
			}
			if env == nil {
				env = envelopeFromDB(msg)
			}
			fw.WriteEnvelope(env)
		}
		if options.BodyStructure != nil {
			bs := imapserver.ExtractBodyStructure(bytes.NewReader(raw))
			if bs == nil {
				// 防御：畸形 MIME 解析失败时降级为 text/plain 单段
				bs = fallbackBodyStructure(raw)
			}
			fw.WriteBodyStructure(bs)
		}
		for _, section := range options.BodySection {
			b := imapserver.ExtractBodySection(bytes.NewReader(raw), section)
			lit := fw.WriteBodySection(section, int64(len(b)))
			lit.Write(b)
			lit.Close()
		}
		if err := fw.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Search 按条件返回匹配消息的序号/UID 集合。
func (s *imapSession) Search(kind imapserver.NumKind, criteria *imap.SearchCriteria, options *imap.SearchOptions) (*imap.SearchData, error) {
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return nil, err
	}

	data := &imap.SearchData{}
	var seqSet imap.SeqSet
	var uidSet imap.UIDSet
	for i := range msgs {
		msg := &msgs[i]
		if !matchesCriteria(msg, criteria) {
			continue
		}
		if kind == imapserver.NumKindSeq {
			seqSet.AddNum(uint32(i + 1))
		} else {
			uidSet.AddNum(imap.UID(msg.ID))
		}
	}
	if kind == imapserver.NumKindSeq {
		data.All = seqSet
	} else {
		data.All = uidSet
	}
	return data, nil
}

// matchesCriteria 判断消息是否满足搜索条件（\Deleted 读数据库持久化标记）。
func matchesCriteria(msg *db.Message, criteria *imap.SearchCriteria) bool {
	for _, flag := range criteria.Flag {
		switch flag {
		case imap.FlagSeen:
			if !msg.IsRead {
				return false
			}
		case imap.FlagFlagged:
			if !msg.IsFlagged {
				return false
			}
		case imap.FlagDeleted:
			if !msg.IsDeleted {
				return false
			}
		case imap.FlagAnswered, imap.FlagDraft:
			return false
		}
	}
	for _, flag := range criteria.NotFlag {
		switch flag {
		case imap.FlagSeen:
			if msg.IsRead {
				return false
			}
		case imap.FlagFlagged:
			if msg.IsFlagged {
				return false
			}
		case imap.FlagDeleted:
			if msg.IsDeleted {
				return false
			}
		}
	}

	if !criteria.Since.IsZero() && msg.Date.Before(criteria.Since) {
		return false
	}
	if !criteria.Before.IsZero() && !msg.Date.Before(criteria.Before) {
		return false
	}

	for _, f := range criteria.Header {
		switch strings.ToLower(f.Key) {
		case "subject":
			if !strings.Contains(strings.ToLower(msg.Subject), strings.ToLower(f.Value)) {
				return false
			}
		case "from":
			if !strings.Contains(strings.ToLower(msg.FromAddr), strings.ToLower(f.Value)) {
				return false
			}
		case "to":
			if !strings.Contains(strings.ToLower(msg.ToAddr), strings.ToLower(f.Value)) {
				return false
			}
		case "cc":
			if !strings.Contains(strings.ToLower(msg.CcAddr), strings.ToLower(f.Value)) {
				return false
			}
		}
	}

	rawSize := int64(len(messageRawData(msg)))
	if criteria.Larger > 0 && rawSize <= criteria.Larger {
		return false
	}
	if criteria.Smaller > 0 && rawSize >= criteria.Smaller {
		return false
	}

	for _, text := range criteria.Body {
		bodyText := strings.ToLower(msg.TextBody + " " + msg.HtmlBody)
		if !strings.Contains(bodyText, strings.ToLower(text)) {
			return false
		}
	}
	for _, text := range criteria.Text {
		allText := strings.ToLower(msg.Subject + " " + msg.FromAddr + " " + msg.ToAddr + " " + msg.TextBody + " " + msg.HtmlBody)
		if !strings.Contains(allText, strings.ToLower(text)) {
			return false
		}
	}

	for _, notCrit := range criteria.Not {
		if matchesCriteria(msg, &notCrit) {
			return false
		}
	}
	for _, orPair := range criteria.Or {
		if !matchesCriteria(msg, &orPair[0]) && !matchesCriteria(msg, &orPair[1]) {
			return false
		}
	}

	return true
}

// Store 修改消息标志：批量持久化（含 \Deleted，落库不丢失）并推送其他会话。
func (s *imapSession) Store(w *imapserver.FetchWriter, numSet imap.NumSet, flags *imap.StoreFlags, options *imap.StoreOptions) error {
	if s.isReadOnly() {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return err
	}
	maxSeq := uint32(len(msgs))
	var maxUID uint32
	for i := range msgs {
		if uint32(msgs[i].ID) > maxUID {
			maxUID = uint32(msgs[i].ID)
		}
	}

	flagSet := make(map[imap.Flag]bool, len(flags.Flags))
	for _, flag := range flags.Flags {
		flagSet[flag] = true
	}

	type change struct {
		msg        *db.Message
		seq        uint32
		newRead    bool
		readSet    bool
		newFlagged bool
		flaggedSet bool
		newDeleted bool
		deletedSet bool
	}
	var changes []change

	for i := range msgs {
		msg := &msgs[i]
		seq := uint32(i + 1)
		uid := imap.UID(msg.ID)
		if !numSetContains(numSet, seq, uid, maxSeq, maxUID) {
			continue
		}
		c := change{msg: msg, seq: seq}
		apply := func(flag imap.Flag, enabled bool) {
			switch flag {
			case imap.FlagSeen:
				c.newRead, c.readSet = enabled, true
			case imap.FlagFlagged:
				c.newFlagged, c.flaggedSet = enabled, true
			case imap.FlagDeleted:
				c.newDeleted, c.deletedSet = enabled, true
			}
		}
		switch flags.Op {
		case imap.StoreFlagsSet:
			apply(imap.FlagSeen, flagSet[imap.FlagSeen])
			apply(imap.FlagFlagged, flagSet[imap.FlagFlagged])
			apply(imap.FlagDeleted, flagSet[imap.FlagDeleted])
		case imap.StoreFlagsAdd:
			for flag := range flagSet {
				apply(flag, true)
			}
		case imap.StoreFlagsDel:
			for flag := range flagSet {
				apply(flag, false)
			}
		}
		changes = append(changes, c)
	}

	// 批量持久化（已读/星标/删除各合并为单条 UPDATE ... IN）
	var readTrue, readFalse, flagTrue, flagFalse, delTrue, delFalse []uint
	for _, c := range changes {
		if c.readSet && c.newRead != c.msg.IsRead {
			if c.newRead {
				readTrue = append(readTrue, c.msg.ID)
			} else {
				readFalse = append(readFalse, c.msg.ID)
			}
		}
		if c.flaggedSet && c.newFlagged != c.msg.IsFlagged {
			if c.newFlagged {
				flagTrue = append(flagTrue, c.msg.ID)
			} else {
				flagFalse = append(flagFalse, c.msg.ID)
			}
		}
		if c.deletedSet && c.newDeleted != c.msg.IsDeleted {
			if c.newDeleted {
				delTrue = append(delTrue, c.msg.ID)
			} else {
				delFalse = append(delFalse, c.msg.ID)
			}
		}
	}
	// 记录首个持久化错误：SQLite 忙/锁等瞬时失败必须让客户端感知
	// （返回 NO 触发重试），否则标志会静默丢失。
	var firstErr error
	mark := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	mark(s.srv.stores.Mails.SetReadStates(readTrue, true))
	mark(s.srv.stores.Mails.SetReadStates(readFalse, false))
	mark(s.srv.stores.Mails.SetFlaggedStates(flagTrue, true))
	mark(s.srv.stores.Mails.SetFlaggedStates(flagFalse, false))
	mark(s.srv.stores.Mails.SetDeletedStates(delTrue, true))
	mark(s.srv.stores.Mails.SetDeletedStates(delFalse, false))

	// 标志变化 → 回写本连接（非 SILENT）并推送同用户其他会话
	hub := s.hubForSelected()
	for _, c := range changes {
		read := c.msg.IsRead
		if c.readSet {
			read = c.newRead
		}
		flagged := c.msg.IsFlagged
		if c.flaggedSet {
			flagged = c.newFlagged
		}
		deleted := c.msg.IsDeleted
		if c.deletedSet {
			deleted = c.newDeleted
		}
		fl := flagsOf(read, flagged, deleted)
		if !flags.Silent {
			fw := w.CreateMessage(c.seq)
			fw.WriteUID(imap.UID(c.msg.ID))
			fw.WriteFlags(fl)
			if err := fw.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if hub != nil {
			hub.enqueue(sessionUpdate{fetch: &sessionFetchUpdate{seq: c.seq, uid: imap.UID(c.msg.ID), flags: fl}}, s)
		}
	}
	return firstErr
}

// Copy 把匹配消息复制到目标文件夹。
func (s *imapSession) Copy(numSet imap.NumSet, dest string) (*imap.CopyData, error) {
	destName, ok := canonicalMailboxName(dest)
	if !ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "No such mailbox"}
	}
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return nil, err
	}
	maxSeq := uint32(len(msgs))
	var maxUID uint32
	for i := range msgs {
		if uint32(msgs[i].ID) > maxUID {
			maxUID = uint32(msgs[i].ID)
		}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	for i := range msgs {
		dbMsg := &msgs[i]
		seq := uint32(i + 1)
		uid := imap.UID(dbMsg.ID)
		if !numSetContains(numSet, seq, uid, maxSeq, maxUID) {
			continue
		}
		copyMsg := &db.Message{
			UserID:    userID,
			MessageID: dbMsg.MessageID,
			Folder:    destName,
			FromAddr:  dbMsg.FromAddr,
			ToAddr:    dbMsg.ToAddr,
			CcAddr:    dbMsg.CcAddr,
			Subject:   dbMsg.Subject,
			TextBody:  dbMsg.TextBody,
			HtmlBody:  dbMsg.HtmlBody,
			RawData:   dbMsg.RawData,
			IsRead:    dbMsg.IsRead,
			IsFlagged: dbMsg.IsFlagged,
			Date:      dbMsg.Date,
		}
		if err := s.srv.stores.Mails.Create(copyMsg); err != nil {
			log.Printf("IMAP: failed to copy message %d to %s: %v", dbMsg.ID, destName, err)
			continue
		}
		sourceUIDs.AddNum(uid)
		destUIDs.AddNum(imap.UID(copyMsg.ID))
	}

	// 目标文件夹会话收到 EXISTS 通知
	if hub := s.srv.hubFor(s.currentEmail(), destName); hub != nil {
		if count, err := s.srv.stores.Mails.CountByUserAndFolder(userID, destName); err == nil {
			hub.enqueue(sessionUpdate{exists: ptrU32(uint32(count))}, nil)
		}
	}

	uidValidity, err := s.srv.stores.MailboxState.UidValidity(userID, destName)
	if err != nil {
		uidValidity = 0
	}
	return &imap.CopyData{
		UIDValidity: uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}, nil
}

// Move 把匹配消息移动到目标文件夹（COPYUID + EXPUNGE 响应）。
func (s *imapSession) Move(w *imapserver.MoveWriter, numSet imap.NumSet, dest string) error {
	if s.isReadOnly() {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}
	destName, ok := canonicalMailboxName(dest)
	if !ok {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "No such mailbox"}
	}
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return err
	}
	maxSeq := uint32(len(msgs))
	var maxUID uint32
	for i := range msgs {
		if uint32(msgs[i].ID) > maxUID {
			maxUID = uint32(msgs[i].ID)
		}
	}

	var sourceUIDs, destUIDs imap.UIDSet
	type moved struct {
		seq uint32
		uid imap.UID
	}
	var movedMsgs []moved
	for i := range msgs {
		dbMsg := &msgs[i]
		seq := uint32(i + 1)
		uid := imap.UID(dbMsg.ID)
		if !numSetContains(numSet, seq, uid, maxSeq, maxUID) {
			continue
		}
		if err := s.srv.stores.Mails.MoveToFolder(dbMsg.ID, destName); err != nil {
			log.Printf("IMAP: failed to move message %d to %s: %v", dbMsg.ID, destName, err)
			continue
		}
		sourceUIDs.AddNum(uid)
		destUIDs.AddNum(uid)
		movedMsgs = append(movedMsgs, moved{seq: seq, uid: uid})
	}

	uidValidity, err := s.srv.stores.MailboxState.UidValidity(userID, destName)
	if err != nil {
		uidValidity = 0
	}
	if err := w.WriteCopyData(&imap.CopyData{
		UIDValidity: uidValidity,
		SourceUIDs:  sourceUIDs,
		DestUIDs:    destUIDs,
	}); err != nil {
		return err
	}
	for _, m := range movedMsgs {
		if err := w.WriteExpunge(m.seq); err != nil {
			return err
		}
	}

	// 源文件夹其余会话收到 EXPUNGE；目标文件夹会话收到 EXISTS
	if hub := s.hubForSelected(); hub != nil {
		for _, m := range movedMsgs {
			hub.enqueue(sessionUpdate{expunge: ptrU32(m.seq)}, s)
		}
	}
	if hub := s.srv.hubFor(s.currentEmail(), destName); hub != nil {
		if count, err := s.srv.stores.Mails.CountByUserAndFolder(userID, destName); err == nil {
			hub.enqueue(sessionUpdate{exists: ptrU32(uint32(count))}, nil)
		}
	}
	return nil
}

// Expunge 永久删除 \Deleted 消息。
// uids == nil：删除该文件夹全部 \Deleted（EXPUNGE/CLOSE）；
// uids != nil：只删除集合内且带 \Deleted 的消息（UID EXPUNGE，RFC 4315）。
func (s *imapSession) Expunge(w *imapserver.ExpungeWriter, uids *imap.UIDSet) error {
	if s.isReadOnly() {
		return &imap.Error{Type: imap.StatusResponseTypeNo, Text: "Mailbox is read-only"}
	}
	userID := s.currentUserID()
	msgs, err := s.srv.stores.Mails.ListAllByUserAndFolder(userID, s.currentMailbox())
	if err != nil {
		return err
	}

	// 删除前计算序号（EXPUNGE 响应序号为删除前状态下的序号）
	type target struct {
		id  uint
		seq uint32
	}
	var targets []target
	for i := range msgs {
		msg := &msgs[i]
		if !msg.IsDeleted {
			continue
		}
		if uids != nil && !uids.Contains(imap.UID(msg.ID)) {
			continue
		}
		targets = append(targets, target{id: msg.ID, seq: uint32(i + 1)})
	}
	if len(targets) == 0 {
		return nil
	}

	ids := make([]uint, len(targets))
	for i, t := range targets {
		ids[i] = t.id
	}
	if err := s.srv.stores.Mails.DeleteMany(ids); err != nil {
		log.Printf("IMAP: failed to expunge %d messages: %v", len(ids), err)
		return err
	}

	// 本连接按序号升序写 EXPUNGE 响应，其余会话经 hub 推送
	for _, t := range targets {
		if err := w.WriteExpunge(t.seq); err != nil {
			return err
		}
	}
	if hub := s.hubForSelected(); hub != nil {
		for _, t := range targets {
			hub.enqueue(sessionUpdate{expunge: ptrU32(t.seq)}, s)
		}
	}
	return nil
}

// Append 追加一封新邮件（IMAP APPEND）。
func (s *imapSession) Append(mailbox string, r imap.LiteralReader, options *imap.AppendOptions) (*imap.AppendData, error) {
	name, ok := canonicalMailboxName(mailbox)
	if !ok {
		return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "No such mailbox"}
	}
	userID := s.currentUserID()

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("failed to read message body: %w", err)
	}

	mr, err := asgomail.CreateReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("failed to parse MIME message: %w", err)
	}
	header := mr.Header
	fromAddr := mailutil.FormatAddressList(&header, "From")
	toAddr := mailutil.FormatAddressList(&header, "To")
	ccAddr := mailutil.FormatAddressList(&header, "Cc")
	subject, _ := header.Subject()
	messageID, _ := header.MessageID()
	msgDate, _ := header.Date()
	if msgDate.IsZero() {
		msgDate = options.Time
	}
	if msgDate.IsZero() {
		msgDate = time.Now()
	}

	var textBody, htmlBody string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		switch h := p.Header.(type) {
		case *asgomail.InlineHeader:
			contentType, params, _ := h.ContentType()
			buf, _ := io.ReadAll(p.Body)
			charset := ""
			if cs, ok := params["charset"]; ok {
				charset = cs
			}
			decoded := mailutil.DecodeCharset(buf, charset)
			if strings.HasPrefix(contentType, "text/plain") {
				textBody = decoded
			} else if strings.HasPrefix(contentType, "text/html") {
				htmlBody = decoded
			}
		case *asgomail.AttachmentHeader:
			// APPEND 附件不落盘（与 v1 行为一致）
		}
	}
	if textBody == "" && htmlBody == "" {
		textBody = string(data)
	}

	var isRead, isFlagged, isDeleted bool
	for _, flag := range options.Flags {
		switch flag {
		case imap.FlagSeen:
			isRead = true
		case imap.FlagFlagged:
			isFlagged = true
		case imap.FlagDeleted:
			isDeleted = true
		}
	}

	msg := &db.Message{
		UserID:    userID,
		MessageID: messageID,
		Folder:    name,
		FromAddr:  fromAddr,
		ToAddr:    toAddr,
		CcAddr:    ccAddr,
		Subject:   subject,
		TextBody:  textBody,
		HtmlBody:  htmlBody,
		RawData:   string(data),
		IsRead:    isRead,
		IsFlagged: isFlagged,
		IsDeleted: isDeleted,
		Date:      msgDate,
	}
	if err := s.srv.stores.Mails.Create(msg); err != nil {
		return nil, fmt.Errorf("failed to create message: %w", err)
	}

	// 该文件夹其余会话收到 EXISTS 通知
	if hub := s.srv.hubFor(s.currentEmail(), name); hub != nil {
		if count, err := s.srv.stores.Mails.CountByUserAndFolder(userID, name); err == nil {
			hub.enqueue(sessionUpdate{exists: ptrU32(uint32(count))}, nil)
		}
	}

	uidValidity, err := s.srv.stores.MailboxState.UidValidity(userID, name)
	if err != nil {
		uidValidity = 0
	}
	return &imap.AppendData{UID: imap.UID(msg.ID), UIDValidity: uidValidity}, nil
}

// Poll 下发积压的推送更新（NOOP/COPY/APPEND 等命令后由库调用）。
func (s *imapSession) Poll(w *imapserver.UpdateWriter, allowExpunge bool) error {
	return writeUpdates(w, s.takeUpdates(allowExpunge))
}

// Idle 阻塞直到收到推送或客户端发 DONE（库负责读取并关闭 stop）。
func (s *imapSession) Idle(w *imapserver.UpdateWriter, stop <-chan struct{}) error {
	for {
		if err := writeUpdates(w, s.takeUpdates(true)); err != nil {
			return err
		}
		select {
		case <-stop:
			return nil
		case <-s.notify:
		}
	}
}

// ---------- 会话内部辅助 ----------

func (s *imapSession) currentUserID() uint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userID
}

func (s *imapSession) currentEmail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.email
}

func (s *imapSession) currentMailbox() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selected
}

func (s *imapSession) isReadOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOnly
}

func (s *imapSession) hubForSelected() *mailboxHub {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hub
}

func ptrU32(v uint32) *uint32 {
	return &v
}

// ---------- Helper functions ----------

// canonicalMailboxName 规范化邮箱名。
func canonicalMailboxName(name string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "INBOX":
		return "INBOX", true
	case "SENT":
		return "Sent", true
	case "DRAFTS":
		return "Drafts", true
	case "TRASH":
		return "Trash", true
	default:
		return "", false
	}
}

// flagsOf 按数据库状态生成 IMAP 标志列表。
func flagsOf(read, flagged, deleted bool) []imap.Flag {
	flags := make([]imap.Flag, 0, 3)
	if read {
		flags = append(flags, imap.FlagSeen)
	}
	if flagged {
		flags = append(flags, imap.FlagFlagged)
	}
	if deleted {
		flags = append(flags, imap.FlagDeleted)
	}
	return flags
}

// seqOf 返回消息在文件夹中的序号（1 基），未找到返回 0。
func seqOf(stores *store.Stores, userID uint, mailbox string, msgID uint) uint32 {
	msgs, err := stores.Mails.ListAllByUserAndFolder(userID, mailbox)
	if err != nil {
		return 0
	}
	for i := range msgs {
		if msgs[i].ID == msgID {
			return uint32(i + 1)
		}
	}
	return 0
}

// messageRawData 返回消息原始字节（优先 RawData，降级按字段重建）。
func messageRawData(msg *db.Message) []byte {
	if msg.RawData != "" {
		return []byte(msg.RawData)
	}
	return buildRawMessage(msg)
}

func headerAndBody(raw []byte) (textproto.Header, io.Reader, error) {
	body := bufio.NewReader(bytes.NewReader(raw))
	hdr, err := textproto.ReadHeader(body)
	return hdr, body, err
}

// fallbackBodyStructure 构造一个 text/plain 单段 BodyStructure（含
// Extended，保证 BODYSTRUCTURE 扩展请求不因 nil Extended 触发库 panic）。
func fallbackBodyStructure(raw []byte) imap.BodyStructure {
	size := uint32(len(raw))
	lines := int64(bytes.Count(raw, []byte{'\n'}))
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		lines++
	}
	return &imap.BodyStructureSinglePart{
		Type:     "text",
		Subtype:  "plain",
		Params:   map[string]string{"charset": "utf-8"},
		Encoding: "8bit",
		Size:     size,
		Text:     &imap.BodyStructureText{NumLines: lines},
		Extended: &imap.BodyStructureSinglePartExt{},
	}
}

// envelopeFromDB 从数据库字段构造信封（原始邮件头解析失败时的降级）。
func envelopeFromDB(msg *db.Message) *imap.Envelope {
	return &imap.Envelope{
		Date:      msg.Date,
		Subject:   msg.Subject,
		From:      parseAddressList(msg.FromAddr),
		Sender:    parseAddressList(msg.FromAddr),
		ReplyTo:   parseAddressList(msg.FromAddr),
		To:        parseAddressList(msg.ToAddr),
		Cc:        parseAddressList(msg.CcAddr),
		MessageID: msg.MessageID,
	}
}

// parseAddressList 解析逗号分隔的地址字符串为 imap.Address 切片。
func parseAddressList(addrStr string) []imap.Address {
	if addrStr == "" {
		return nil
	}
	addresses, err := mail.ParseAddressList(addrStr)
	if err != nil {
		return []imap.Address{{Mailbox: addrStr}}
	}
	result := make([]imap.Address, 0, len(addresses))
	for _, addr := range addresses {
		parts := strings.SplitN(addr.Address, "@", 2)
		mailbox := parts[0]
		host := ""
		if len(parts) > 1 {
			host = parts[1]
		}
		result = append(result, imap.Address{
			Name:    addr.Name,
			Mailbox: mailbox,
			Host:    host,
		})
	}
	return result
}

// buildRawMessage reconstructs a raw RFC822 message from a db.Message.
// If RawData is available, it uses the original raw data directly.
func buildRawMessage(msg *db.Message) []byte {
	if msg.RawData != "" {
		return []byte(msg.RawData)
	}

	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("From: %s\r\n", msg.FromAddr))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", msg.ToAddr))
	if msg.CcAddr != "" {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", msg.CcAddr))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", msg.Subject))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	if msg.MessageID != "" {
		buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	}
	buf.WriteString("MIME-Version: 1.0\r\n")

	if msg.HtmlBody != "" && msg.TextBody != "" {
		boundary := fmt.Sprintf("mailgo_%d", msg.ID)
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	} else if msg.TextBody != "" {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
	} else if msg.HtmlBody != "" {
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HtmlBody)
	} else {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	}

	return buf.Bytes()
}

// 编译期断言：imapSession 满足 Session 与 SessionMove 接口。
var _ imapserver.Session = (*imapSession)(nil)
var _ imapserver.SessionMove = (*imapSession)(nil)
