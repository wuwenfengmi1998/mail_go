package db

// P4 #17 回归：dedupeBanEntries 在 AutoMigrate 前清理历史重复行，
// 为 ip_address 唯一索引扫清障碍；表不存在时静默跳过。

import (
	"path/filepath"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// createLegacyBanTable 按旧版结构建表（ip_address 无唯一索引）。
func createLegacyBanTable(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.Exec(`CREATE TABLE ban_entries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ip_address TEXT NOT NULL,
		reason TEXT,
		fail_count INTEGER DEFAULT 0,
		ban_count INTEGER DEFAULT 0,
		expires_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	return gdb
}

func TestDedupeBanEntriesRemovesDuplicates(t *testing.T) {
	gdb := createLegacyBanTable(t)

	// 同一 IP 三条旧记录（id 1,2,3），另一 IP 一条
	for _, rec := range []struct {
		ip  string
		fc  int
	}{
		{"1.2.3.4", 1},
		{"1.2.3.4", 2},
		{"1.2.3.4", 3},
		{"5.6.7.8", 4},
	} {
		if err := gdb.Exec(
			"INSERT INTO ban_entries (ip_address, fail_count) VALUES (?, ?)", rec.ip, rec.fc,
		).Error; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	dedupeBanEntries(gdb)

	// 每 IP 仅剩 id 最大的一条
	var rows []struct {
		IP string `gorm:"column:ip_address"`
		FC int    `gorm:"column:fail_count"`
	}
	if err := gdb.Raw("SELECT ip_address, fail_count FROM ban_entries ORDER BY ip_address").Scan(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows after dedupe = %d, want 2", len(rows))
	}
	if rows[0].IP != "1.2.3.4" || rows[0].FC != 3 {
		t.Fatalf("1.2.3.4 row = %+v, want the max-id row (fail_count=3)", rows[0])
	}
	if rows[1].IP != "5.6.7.8" || rows[1].FC != 4 {
		t.Fatalf("5.6.7.8 row = %+v, want fail_count=4", rows[1])
	}
}

func TestDedupeBanEntriesMissingTableSilent(t *testing.T) {
	gdb, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// 表不存在时不应 panic/报错（首次安装场景）
	dedupeBanEntries(gdb)
}
