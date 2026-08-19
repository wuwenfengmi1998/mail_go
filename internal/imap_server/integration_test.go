//go:build !race

// 集成测试：启动真实 IMAP 监听 + 脚本客户端（go-imap client）。
// 注意：仅在非 -race 构建下运行——go-imap v1.2.1 存在库内数据竞争
// （cmd_selected.go STORE 写 *conn.silent() vs listenUpdates 读），
// 启用 backend 推送（Updates != nil）时必然触发，-race 下会误报。
// 推送逻辑的竞态覆盖由单元测试（notify_test.go）承担。
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

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
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
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.ProtocolLog{}, &db.BanEntry{}); err != nil {
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

func loginAndSelect(t *testing.T, addr string) *client.Client {
	t.Helper()
	c, err := client.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Logout() })
	if err := c.Login("alice@example.com", "secret123"); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		t.Fatalf("select: %v", err)
	}
	return c
}

// uidOf returns the message id of the newest message by date.
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
	seqset := new(imap.SeqSet)
	seqset.AddNum(uint32(ids[1])) // UID = 第二条消息
	ch := make(chan *imap.Message, 1)
	if err := c.UidStore(seqset, imap.AddFlags, []interface{}{imap.SeenFlag}, ch); err != nil {
		t.Fatalf("uid store: %v", err)
	}
	<-ch

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
	seqsetAll := new(imap.SeqSet)
	seqsetAll.AddRange(1, 3)
	messages := make(chan *imap.Message, 3)
	if err := c.Fetch(seqsetAll, []imap.FetchItem{imap.FetchFlags, imap.FetchUid}, messages); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	var targetSeq uint32
	for m := range messages {
		if m.Uid == uint32(ids[2]) {
			targetSeq = m.SeqNum
		}
	}
	if targetSeq == 0 {
		t.Fatal("target message not found in fetch")
	}

	seqset := new(imap.SeqSet)
	seqset.AddNum(targetSeq)
	ch := make(chan *imap.Message, 1)
	if err := c.Store(seqset, imap.AddFlags, []interface{}{imap.SeenFlag}, ch); err != nil {
		t.Fatalf("store: %v", err)
	}
	<-ch

	assertReadState(t, stores, ids[2], true)
}

// TestSeqStoreClientSelfNumbered 复现风险场景：客户端不信任服务器序号，
// 按自己的视图（日期倒序，最新在前）自行编号后发 seq 式 STORE。
// 服务器规范排序必须与常见客户端视图一致（date DESC, id DESC），
// 否则会把另一封邮件标为已读、目标邮件永远未读。
func TestSeqStoreClientSelfNumbered(t *testing.T) {
	stores, addr := startIntegrationServer(t)
	ids := seedMailbox(t, stores, 1, 3) // 3 封，日期递增，最新的是 ids[2]

	c := loginAndSelect(t, addr)

	// 客户端按日期倒序视图：最新一封 = seq 1
	seqset := new(imap.SeqSet)
	seqset.AddNum(1)
	ch := make(chan *imap.Message, 1)
	if err := c.Store(seqset, imap.AddFlags, []interface{}{imap.SeenFlag}, ch); err != nil {
		t.Fatalf("store: %v", err)
	}
	<-ch

	// 客户端意图是标记最新一封（ids[2]）为已读
	assertReadState(t, stores, ids[2], true)
}

// TestFetchBodyMalformedMIME 回归：消息包含无法解析的 MIME（base64 编码的
// message/rfc822 附件 / 截断的 multipart）时，FETCH BODY/BODYSTRUCTURE
// 不得因 nil BodyStructure 触发服务器 panic（否则连接中断，客户端只收到
// 部分邮件或一直卡在同步）。修复前 go-imap send() 协程会 nil 指针崩溃。
func TestFetchBodyMalformedMIME(t *testing.T) {
	stores, addr := startIntegrationServer(t)

	// 1) base64 编码的 message/rfc822 附件（转发邮件场景）：
	//    backendutil.FetchBodyStructure 不解码 base64，直接把编码文本
	//    当嵌套消息头解析 → "malformed MIME header line" 错误。
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
	// 2) 截断的 multipart（缺少结束边界）：BODYSTRUCTURE(extended) 解析报错
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

	seqset := new(imap.SeqSet)
	seqset.AddRange(1, 2)

	// BODY：历史上 message/rfc822 消息解析失败 → nil BodyStructure → panic
	msgs := make(chan *imap.Message, 10)
	if err := c.Fetch(seqset, []imap.FetchItem{imap.FetchBody}, msgs); err != nil {
		t.Fatalf("fetch body: %v", err)
	}
	got := 0
	for range msgs {
		got++
	}
	if got != 2 {
		t.Fatalf("FETCH BODY 返回 %d/2 封", got)
	}

	// BODYSTRUCTURE：截断 multipart 在 extended 解析时报错 → nil → panic
	msgs2 := make(chan *imap.Message, 10)
	if err := c.Fetch(seqset, []imap.FetchItem{imap.FetchBodyStructure}, msgs2); err != nil {
		t.Fatalf("fetch bodystructure: %v", err)
	}
	got2 := 0
	for range msgs2 {
		got2++
	}
	if got2 != 2 {
		t.Fatalf("FETCH BODYSTRUCTURE 返回 %d/2 封", got2)
	}
}
