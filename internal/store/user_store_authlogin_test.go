package store

import (
	"testing"

	"mail_go/internal/db"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// TestAuthenticateLoginBareUsername 验证协议层登录支持裸用户名：
// 唯一归属时可用，密码错误/用户不存在/跨域名同名歧义时拒绝。
func TestAuthenticateLoginBareUsername(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := gdb.AutoMigrate(&db.User{}, &db.Domain{}); err != nil {
		t.Fatal(err)
	}
	us := newUserStore(gdb)
	ds := newDomainStore(gdb)

	dom1 := &db.Domain{Name: "example.com"}
	if err := ds.Create(dom1); err != nil {
		t.Fatal(err)
	}
	hashed, _ := bcrypt.GenerateFromPassword([]byte("secret123"), bcrypt.DefaultCost)
	u1 := &db.User{Username: "alice", DomainID: dom1.ID, PasswordHash: string(hashed), IsActive: true}
	if err := us.Create(u1); err != nil {
		t.Fatal(err)
	}

	// 裸用户名 + 正确密码 → 成功
	u, err := us.AuthenticateLogin("alice", "secret123")
	if err != nil {
		t.Fatalf("bare username should succeed: %v", err)
	}
	if u.ID != u1.ID {
		t.Fatalf("wrong user: %d != %d", u.ID, u1.ID)
	}
	// 裸用户名 + 错误密码 → 失败
	if _, err := us.AuthenticateLogin("alice", "wrong"); err == nil {
		t.Fatal("wrong password should fail")
	}
	// 不存在 → 失败
	if _, err := us.AuthenticateLogin("nobody", "secret123"); err == nil {
		t.Fatal("unknown user should fail")
	}
	// 完整邮箱仍然可用
	if _, err := us.AuthenticateLogin("alice@example.com", "secret123"); err != nil {
		t.Fatalf("full email should succeed: %v", err)
	}

	// 跨域名同名 → 歧义拒绝
	dom2 := &db.Domain{Name: "other.com"}
	if err := ds.Create(dom2); err != nil {
		t.Fatal(err)
	}
	u2 := &db.User{Username: "alice", DomainID: dom2.ID, PasswordHash: string(hashed), IsActive: true}
	if err := us.Create(u2); err != nil {
		t.Fatal(err)
	}
	if _, err := us.AuthenticateLogin("alice", "secret123"); err == nil {
		t.Fatal("ambiguous bare username should fail")
	}
	// 歧义时完整邮箱仍可用
	if _, err := us.AuthenticateLogin("alice@example.com", "secret123"); err != nil {
		t.Fatalf("full email should still work: %v", err)
	}
}
