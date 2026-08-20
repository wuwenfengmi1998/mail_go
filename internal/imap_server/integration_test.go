// 集成测试：启动真实 IMAP 监听 + 脚本客户端（go-imap v2 imapclient）。
// v2 库为 race-clean（此前 v1.2.1 库内数据竞争导致本文件必须带
// !race 构建标签，升级后已移除）。推送逻辑的单元覆盖由 notify_test.go 承担。
package imap_server

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"mail_go/config"
	"mail_go/internal/connhub"
	"mail_go/internal/db"
	"mail_go/internal/store"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// startIntegrationServer 启动一个真实的 IMAP 监听（随机端口）供客户端测试。
func startIntegrationServer(t *testing.T) (*store.Stores, string) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.ProtocolLog{}, &db.BanEntry{}, &db.MailboxState{}, &db.Mailbox{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	stores := store.NewStores(gdb)

	domain := &db.Domain{Name: "example.com"}
	if err := stores.Domains.Create(domain); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	user := &db.User{Username: "alice", DomainID: domain.ID, PasswordHash: string(hashed), IsActive: true}
	if err := stores.Users.Create(user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	srv := NewIMAPServer(config.IMAPConfig{}, stores, nil, config.BanConfig{}, connhub.New())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	imapSrv := srv.newServer(ln.Addr().String(), nil)
	go imapSrv.Serve(ln)

	return stores, ln.Addr().String()
}

// seedMailbox 创建 n 封按时间递增的邮件（id 与 date 顺序一致时 id ASC == date ASC）。
func seedMailbox(t *testing.T, stores *store.Stores, userID uint, n int) []uint {
	t.Helper()
	ids := make([]uint, 0, n)
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	for i := 0; i < n; i++ {
		msg := &db.Message{
			UserID:    userID,
			Folder:    "INBOX",
			FromAddr:  "x@y",
			ToAddr:    "alice@example.com",
			Subject:   "m",
			Date:      base.Add(time.Duration(i) * time.Hour), // 时间递增：id 越大日期越新
			CreatedAt: time.Now(),
		}
		if err := stores.Mails.Create(msg); err != nil {
			t.Fatalf("create message: %v", err)
		}
		ids = append(ids, msg.ID)
	}
	return ids
}

func loginAndSelect(t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Logout().Wait() })
	if err := c.Login("alice@example.com", "secret123").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select: %v", err)
	}
	return c
}

func assertReadState(t *testing.T, stores *store.Stores, msgID uint, want bool) {
	t.Helper()
	msg, err := stores.Mails.GetByID(msgID)
	if err != nil {
		t.Fatalf("get msg: %v", err)
	}
	if msg.IsRead != want {
		t.Fatalf("msg %d IsRead = %v, want %v", msgID, msg.IsRead, want)
	}
}

// TestUidStorePersists 验证 UID STORE +FLAGS(\Seen) 持久化（RFC 标准流程）。
func TestUidStorePersists(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3)

	c := loginAndSelect(t, addr)
	cmd := c.Store(imap.UIDSetNum(imap.UID(ids[1])), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil)
	if _, err := cmd.Collect(); err != nil {
		t.Fatalf("uid store: %v", err)
	}

	assertReadState(t, stores, ids[1], true)
	assertReadState(t, stores, ids[0], false)
	assertReadState(t, stores, ids[2], false)
}

// TestSeqStoreServerIssued 验证客户端用服务器下发的序号（FETCH 结果）做
// seq 式 STORE：任何排序下都应正确持久化。
func TestSeqStoreServerIssued(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3)

	c := loginAndSelect(t, addr)

	// 拉取全部消息，找到 ids[2]（最新一封）的服务器序号
	msgs, err := c.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var targetSeq uint32
	for _, m := range msgs {
		if m.UID == imap.UID(ids[2]) {
			targetSeq = m.SeqNum
		}
	}
	if targetSeq == 0 {
		t.Fatal("target message not found in fetch")
	}

	cmd := c.Store(imap.SeqSetNum(targetSeq), &imap.StoreFlags{
		Op:    imap.StoreFlagsAdd,
		Flags: []imap.Flag{imap.FlagSeen},
	}, nil)
	if _, err := cmd.Collect(); err != nil {
		t.Fatalf("store: %v", err)
	}

	assertReadState(t, stores, ids[2], true)
}

// TestSeqOrderArrival 验证序号按到达顺序（id ASC，最早 = seq 1）分配：
// 与主流服务器行为一致——新邮件永远追加到末尾（seq = 新 EXISTS 数），
// 既不位移既有邮件序号，也能被 seq 增量同步（seq 4）正确获取；
// INTERNALDATE 返回到达时间（CreatedAt）而非 Date 头。
func TestSeqOrderArrival(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3)

	c := loginAndSelect(t, addr)

	msgs, err := c.Fetch(imap.SeqSetNum(1, 2, 3), &imap.FetchOptions{UID: true}).Collect()
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	seqOf := map[imap.UID]uint32{}
	for _, m := range msgs {
		seqOf[m.UID] = m.SeqNum
	}
	if seqOf[imap.UID(ids[0])] != 1 || seqOf[imap.UID(ids[1])] != 2 || seqOf[imap.UID(ids[2])] != 3 {
		t.Fatalf("seq mapping = %v, want ids[0]=1 ids[1]=2 ids[2]=3", seqOf)
	}

	// 新邮件到达（Date 头较旧）：仍追加到末尾，不移位既有邮件
	late := &db.Message{
		UserID:    1,
		Folder:    "INBOX",
		FromAddr:  "x@y",
		ToAddr:    "alice@example.com",
		Subject:   "late",
		Date:      time.Now().Add(-24 * time.Hour),
		CreatedAt: time.Now(),
	}
	if err := stores.Mails.Create(late); err != nil {
		t.Fatalf("create late message: %v", err)
	}

	msgs, err = c.Fetch(imap.SeqSetNum(4), &imap.FetchOptions{UID: true, InternalDate: true}).Collect()
	if err != nil {
		t.Fatalf("fetch seq 4: %v", err)
	}
	if len(msgs) != 1 || msgs[0].UID != imap.UID(late.ID) {
		t.Fatalf("seq 4 = %v, want 新邮件 uid=%d", msgs, late.ID)
	}

	// INTERNALDATE = 到达时间（CreatedAt），而不是 Date 头（协议格式仅到秒）
	stored, err := stores.Mails.GetByID(late.ID)
	if err != nil {
		t.Fatalf("get late msg: %v", err)
	}
	if !msgs[0].InternalDate.Equal(stored.CreatedAt.Truncate(time.Second)) {
		t.Fatalf("internaldate = %v, want CreatedAt %v", msgs[0].InternalDate, stored.CreatedAt)
	}
}

// TestFetchBodyMalformedMIME 回归：消息包含无法解析的 MIME（base64 编码的
// message/rfc822 附件 / 截断的 multipart）时，FETCH BODY/BODYSTRUCTURE
// 不得 panic（v2 的 ExtractBodyStructure 对畸形 MIME 返回降级结构，
// WriteBodyStructure 要求 Extended 非 nil，两者都需满足）。
func TestFetchBodyMalformedMIME(t *testing.T) {
	stores, addr := startIntegrationServer(t)

	// 1) base64 编码的 message/rfc822 附件（转发邮件场景）
	rfc822Body := "UmVjZWl2ZWQ6IGZyb20gb3V0Ym91bmQuY2kuaWNsb3VkLmNvbSAodW5rbm93biBbMTI3LjAuMC4yKVxuXHQgYnkgcDAwLWljbG91ZG10YS1hc210cC11cy1jZW50cmFsLTFrLTEwMC1wZXJjZW50LTggKFBvc3RmaXgpIHdpdGggRVNNVFBTIGlkIDIxRTlBMThDQURDRjM4MlxuXHQgZm9yIDxkc2hAbG12ZS5uZXQ+OyBTdW4sIDE2IEF1ZyAyMDI2IDEzOjU4OjIxICswMDAwIChVVEMpXG5YLUlDTC1SZXBJZDogRURWY1BlQ3RlWG4tZ0Z1T0xxUWhfSjZvcE9fN1B2OEtsOW1mMDg2VUFxZ29zXG5EYXRlOiBTdW4sIDE2IEF1ZyAyMDI2IDEzOjU4OjIxICswMDAwXG5Gcm9tOiBkYXZpZEB5YW5kZXguY29tXG5UbzogZHNoQGxtdmUubmV0XG5NZXNzYWdlLUlEOiA8QTIxNzBEMTEtMkI1MC00MTQwLTlEQTMtMkI3M0U2RUIwQTc4QHlhbmRleC5jb20+XG5TdWJqZWN0OiB0ZXN0XG5cbmhlbGxvXG4="
	msgWithRFC822 := &db.Message{
		UserID:   1,
		Folder:   "INBOX",
		FromAddr: "alice@example.com",
		ToAddr:   "alice@example.com",
		Subject:  "fwd",
		Date:     time.Now().Add(-2 * time.Hour),
		RawData: "From: alice@example.com\r\n" +
			"To: alice@example.com\r\n" +
			"Subject: fwd\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/mixed; boundary=\"==fwd==\"\r\n\r\n" +
			"--==fwd==\r\n" +
			"Content-Type: text/plain; charset=\"utf-8\"\r\n" +
			"Content-Transfer-Encoding: 8bit\r\n\r\n" +
			"正文\r\n\r\n" +
			"--==fwd==\r\n" +
			"Content-Type: message/rfc822\r\n" +
			"Content-Transfer-Encoding: base64\r\n" +
			"Content-Disposition: attachment; filename=\"original.eml\"\r\n" +
			"MIME-Version: 1.0\r\n\r\n" +
			rfc822Body + "\r\n" +
			"--==fwd==--\r\n",
	}
	// 2) 截断的 multipart（缺少结束边界）
	msgTruncated := &db.Message{
		UserID:   1,
		Folder:   "INBOX",
		FromAddr: "alice@example.com",
		ToAddr:   "alice@example.com",
		Subject:  "truncated",
		Date:     time.Now().Add(-1 * time.Hour),
		RawData: "From: alice@example.com\r\n" +
			"To: alice@example.com\r\n" +
			"Subject: truncated\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: multipart/alternative; boundary=\"==trunc==\"\r\n\r\n" +
			"--==trunc==\r\n" +
			"Content-Type: text/plain\r\n\r\n" +
			"hello\r\n",
		// 无结束边界
	}
	if err := stores.Mails.Create(msgWithRFC822); err != nil {
		t.Fatalf("create msg: %v", err)
	}
	if err := stores.Mails.Create(msgTruncated); err != nil {
		t.Fatalf("create msg: %v", err)
	}

	c := loginAndSelect(t, addr)

	seqSet := imap.SeqSetNum(1, 2)

	// BODY（非扩展）
	msgs, err := c.Fetch(seqSet, &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{}}).Collect()
	if err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("FETCH BODY 返回 %d/2 封", len(msgs))
	}

	// BODYSTRUCTURE（扩展）
	msgs2, err := c.Fetch(seqSet, &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}).Collect()
	if err != nil {
		t.Fatalf("fetch bodystructure: %v", err)
	}
	if len(msgs2) != 2 {
		t.Fatalf("FETCH BODYSTRUCTURE 返回 %d/2 封", len(msgs2))
	}
}

// TestUidExpungeDeleteFlow 核心回归：模拟 Apple Mail / iOS Mail 的删除流程
// （UID COPY → Trash + UID STORE \Deleted + UID EXPUNGE）。修复前：
// \Deleted 只存内存会话、UID EXPUNGE 不被 go-imap v1 支持 → 原邮件留在
// INBOX，垃圾桶只有副本。修复后：原邮件被真正删除，Trash 留有一份副本。
func TestUidExpungeDeleteFlow(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3)
	uid := imap.UID(ids[1])

	c := loginAndSelect(t, addr)

	// 1) UID COPY → Trash
	if _, err := c.Copy(imap.UIDSetNum(uid), "Trash").Wait(); err != nil {
		t.Fatalf("uid copy: %v", err)
	}

	// 2) UID STORE +FLAGS.SILENT (\Deleted)
	if _, err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatalf("uid store deleted: %v", err)
	}

	// 3) UID EXPUNGE
	seqs, err := c.UIDExpunge(imap.UIDSetNum(uid)).Collect()
	if err != nil {
		t.Fatalf("uid expunge: %v", err)
	}
	if len(seqs) != 1 {
		t.Fatalf("uid expunge seqs = %v, want 1 条", seqs)
	}

	// 原邮件必须被真正删除
	if _, err := stores.Mails.GetByID(ids[1]); err == nil {
		t.Fatal("原邮件仍在数据库，UID EXPUNGE 未生效")
	}
	// 其余邮件保留
	assertReadState(t, stores, ids[0], false)
	assertReadState(t, stores, ids[2], false)
	// Trash 中有一份副本
	trashCount, err := stores.Mails.CountByUserAndFolder(1, "Trash")
	if err != nil || trashCount != 1 {
		t.Fatalf("Trash count = %d, want 1", trashCount)
	}
}

// TestDeletedPersistsAcrossSessions 验证 \Deleted 落库：标记后断开重连
// （模拟客户端切换文件夹/重连），普通 EXPUNGE 仍能删除。修复前标记只存
// 内存会话对象，重连后 EXPUNGE 什么都不删。
func TestDeletedPersistsAcrossSessions(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3)
	uid := imap.UID(ids[1])

	c := loginAndSelect(t, addr)
	if _, err := c.Store(imap.UIDSetNum(uid), &imap.StoreFlags{
		Op:     imap.StoreFlagsAdd,
		Silent: true,
		Flags:  []imap.Flag{imap.FlagDeleted},
	}, nil).Collect(); err != nil {
		t.Fatalf("uid store deleted: %v", err)
	}
	if err := c.Logout().Wait(); err != nil {
		t.Fatalf("logout: %v", err)
	}

	// 重新连接（新的会话对象，此前标记必须仍在库中）
	c2, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	t.Cleanup(func() { c2.Logout().Wait() })
	if err := c2.Login("alice@example.com", "secret123").Wait(); err != nil {
		t.Fatalf("login2: %v", err)
	}
	if _, err := c2.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("select2: %v", err)
	}
	seqs, err := c2.Expunge().Collect()
	if err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if len(seqs) != 1 {
		t.Fatalf("expunge seqs = %v, want 1 条", seqs)
	}
	if _, err := stores.Mails.GetByID(ids[1]); err == nil {
		t.Fatal("标记 \\Deleted 的邮件在重连后未被 EXPUNGE 删除")
	}
}

// TestUidMoveFlow 验证 UID MOVE：目标文件夹出现该邮件且源文件夹删除。
func TestUidMoveFlow(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 2)
	uid := imap.UID(ids[1])

	c := loginAndSelect(t, addr)
	if _, err := c.Move(imap.UIDSetNum(uid), "Trash").Wait(); err != nil {
		t.Fatalf("uid move: %v", err)
	}

	if _, err := stores.Mails.GetByID(ids[1]); err != nil {
		t.Fatalf("moved message missing: %v", err)
	}
	msg, err := stores.Mails.GetByID(ids[1])
	if err != nil {
		t.Fatalf("get moved msg: %v", err)
	}
	if msg.Folder != "Trash" {
		t.Fatalf("moved msg folder = %q, want Trash", msg.Folder)
	}
}

// TestListDataDriven 验证文件夹目录由 mailboxes 表驱动：LIST 返回
// 4 个系统文件夹 + CREATE 的自定义文件夹，且带正确的 SPECIAL-USE 属性；
// DELETE 非空/系统文件夹被拒绝，LSUB 按订阅过滤。
func TestListDataDriven(t *testing.T) {
	stores, addr := startIntegrationServer(t)

	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Logout().Wait() })
	if err := c.Login("alice@example.com", "secret123").Wait(); err != nil {
		t.Fatalf("login: %v", err)
	}

	// LIST：系统文件夹齐全且属性正确
	datas, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byName := map[string]*imap.ListData{}
	for _, d := range datas {
		byName[d.Mailbox] = d
	}
	for _, want := range []string{"INBOX", "Sent", "Drafts", "Trash"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("LIST 缺少 %s", want)
		}
	}
	if !containsAttr(byName["Trash"].Attrs, imap.MailboxAttrTrash) {
		t.Fatalf("Trash attrs = %v, want \\Trash", byName["Trash"].Attrs)
	}
	if !containsAttr(byName["Sent"].Attrs, imap.MailboxAttrSent) {
		t.Fatalf("Sent attrs = %v, want \\Sent", byName["Sent"].Attrs)
	}

	// CREATE 自定义文件夹 → LIST 立即可见
	if err := c.Create("工作", nil).Wait(); err != nil {
		t.Fatalf("create: %v", err)
	}
	datas2, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("list after create: %v", err)
	}
	found := false
	for _, d := range datas2 {
		if d.Mailbox == "工作" {
			found = true
		}
	}
	if !found {
		t.Fatal("CREATE 后的自定义文件夹未出现在 LIST 中")
	}

	// 重名创建 → NO
	if err := c.Create("工作", nil).Wait(); err == nil {
		t.Fatal("重复 CREATE 应返回 NO")
	}

	// 系统文件夹不可删除
	if err := c.Delete("Trash").Wait(); err == nil {
		t.Fatal("删除系统文件夹应返回 NO")
	}

	// 非空自定义文件夹不可删除
	ids := seedMailbox(t, stores, 1, 1)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Copy(imap.UIDSetNum(imap.UID(ids[0])), "工作").Wait(); err != nil {
		t.Fatalf("copy to custom: %v", err)
	}
	if err := c.Delete("工作").Wait(); err == nil {
		t.Fatal("删除非空文件夹应返回 NO")
	}

	// RENAME 自定义文件夹
	if err := c.Rename("工作", "归档", nil).Wait(); err != nil {
		t.Fatalf("rename: %v", err)
	}
	datas3, err := c.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("list after rename: %v", err)
	}
	renamed := false
	for _, d := range datas3 {
		if d.Mailbox == "归档" {
			renamed = true
		}
		if d.Mailbox == "工作" {
			t.Fatal("旧文件夹名仍出现在 LIST 中")
		}
	}
	if !renamed {
		t.Fatal("重命名后的文件夹未出现在 LIST 中")
	}

	// UNSUBSCRIBE → LSUB 不再返回
	if err := c.Unsubscribe("归档").Wait(); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	lsub, err := c.List("", "*", &imap.ListOptions{SelectSubscribed: true}).Collect()
	if err != nil {
		t.Fatalf("lsub: %v", err)
	}
	for _, d := range lsub {
		if d.Mailbox == "归档" {
			t.Fatal("退订后的文件夹不应出现在 LSUB 中")
		}
	}
}

// containsAttr 判断属性列表是否包含目标属性。
func containsAttr(attrs []imap.MailboxAttr, want imap.MailboxAttr) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}
