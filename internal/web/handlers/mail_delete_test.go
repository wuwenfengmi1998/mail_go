package handlers

// Web 删除语义回归测试：删除=移入垃圾箱（IMAP 语义）、垃圾箱中删除=彻底
// 删除、恢复=回到收件箱、清空=永久删除。文件夹操作全部经由 MailboxService
// （与 IMAP 会话共用同一份实现）。

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mail_go/internal/db"
	"mail_go/internal/imap_server"
	"mail_go/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newMailTestHandler(t *testing.T) (*MailHandler, *store.Stores) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}, &db.Message{}, &db.Attachment{}, &db.Mailbox{}, &db.MailboxState{}); err != nil {
		t.Fatal(err)
	}
	stores := store.NewStores(gdb)
	if err := stores.Users.Create(&db.User{ID: 1, Username: "alice", Domain: db.Domain{Name: "example.com"}, DomainID: 1}); err != nil {
		t.Fatal(err)
	}
	return NewMailHandler(stores, nil, nil, imap_server.NewMailboxService(stores), nil), stores
}

// newMailTestRouter 注册删除/恢复/清空路由并注入认证上下文。
func newMailTestRouter(h *MailHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Folder 页渲染需要模板（与 oauth2 测试共用同一份测试函数表）
	tmpl := template.Must(template.New("").Funcs(testTemplateFuncs()).ParseGlob(filepath.Join("..", "templates", "*.html")))
	r.SetHTMLTemplate(tmpl)
	r.Use(func(c *gin.Context) {
		c.Set("userID", uint(1))
		c.Set("currentUser", &db.User{ID: 1, Username: "alice", Domain: db.Domain{Name: "example.com"}})
		c.Next()
	})
	r.POST("/mail/delete/:id", h.Delete)
	r.POST("/mail/restore/:id", h.Restore)
	r.POST("/mail/purge/:id", h.Purge)
	r.POST("/folder/:name/empty", h.EmptyFolder)
	r.GET("/folder/:name", h.Folder)
	return r
}

func seedWebMsg(t *testing.T, stores *store.Stores, folder string) *db.Message {
	t.Helper()
	msg := &db.Message{
		UserID:   1,
		Folder:   folder,
		FromAddr: "sender@other.com",
		ToAddr:   "alice@example.com",
		Subject:  "测试邮件",
		Date:     time.Now(),
	}
	if err := stores.Mails.Create(msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func msgFolder(t *testing.T, stores *store.Stores, id uint) (string, bool) {
	t.Helper()
	msg, err := stores.Mails.GetByID(id)
	if err != nil {
		return "", false
	}
	return msg.Folder, true
}

func TestWebDeleteMovesToTrash(t *testing.T) {
	h, stores := newMailTestHandler(t)
	msg := seedWebMsg(t, stores, "INBOX")
	r := newMailTestRouter(h)

	// 收件箱删除 → 移入垃圾箱（非物理删除）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mail/delete/"+itoa(msg.ID), nil)
	req.Header.Set("Referer", "/folder/INBOX")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("delete status = %d, want 302", w.Code)
	}
	folder, ok := msgFolder(t, stores, msg.ID)
	if !ok || folder != "Trash" {
		t.Fatalf("deleted msg folder = %q, want Trash", folder)
	}

	// 垃圾箱中再删除 → 彻底删除
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/mail/delete/"+itoa(msg.ID), nil)
	req2.Header.Set("Referer", "/folder/Trash")
	r.ServeHTTP(w2, req2)
	if _, ok := msgFolder(t, stores, msg.ID); ok {
		t.Fatal("trash delete should purge the message")
	}
}

func TestWebRestoreToInbox(t *testing.T) {
	h, stores := newMailTestHandler(t)
	msg := seedWebMsg(t, stores, "Trash")
	r := newMailTestRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mail/restore/"+itoa(msg.ID), nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("restore status = %d, want 302", w.Code)
	}
	folder, ok := msgFolder(t, stores, msg.ID)
	if !ok || folder != "INBOX" {
		t.Fatalf("restored msg folder = %q, want INBOX", folder)
	}
}

func TestWebEmptyFolderPurgesAll(t *testing.T) {
	h, stores := newMailTestHandler(t)
	seedWebMsg(t, stores, "Trash")
	seedWebMsg(t, stores, "Trash")
	r := newMailTestRouter(h)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/folder/Trash/empty", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("empty status = %d, want 302", w.Code)
	}
	count, err := stores.Mails.CountByUserAndFolder(1, "Trash")
	if err != nil || count != 0 {
		t.Fatalf("Trash count after empty = %d, want 0", count)
	}
}

func TestWebFolderPageListsDynamicFolders(t *testing.T) {
	h, stores := newMailTestHandler(t)
	seedWebMsg(t, stores, "INBOX")
	r := newMailTestRouter(h)

	// IMAP CREATE 语义创建的自定义文件夹（经同一 MailboxService）
	svc := imap_server.NewMailboxService(stores)
	if err := svc.Create(1, "工作"); err != nil {
		t.Fatalf("create custom mailbox: %v", err)
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/folder/工作", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("custom folder page status = %d, want 200", w.Code)
	}

	// 不存在的文件夹 → 404
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/folder/不存在", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("missing folder status = %d, want 404", w2.Code)
	}
}

func itoa(n uint) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
