package db

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"mail_go/config"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// InitDB initializes the database connection and performs auto-migration.
// It selects the appropriate driver based on cfg.Driver and resolves
// the DSN path for SQLite relative to the storage base directory.
func InitDB(cfg config.DatabaseConfig, storageCfg config.StorageConfig) (*gorm.DB, error) {
	var dialector gorm.Dialector

	switch cfg.Driver {
	case "sqlite":
		dsn := cfg.DSN
		// If the DSN is the default relative path, prepend the storage base directory
		if dsn == config.DefaultDSNWin || dsn == config.DefaultDSNLinux {
			dsn = filepath.Join(storageCfg.BaseDir, "mail.db")
		}
		// Ensure the parent directory exists for SQLite
		dir := filepath.Dir(dsn)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("创建数据库目录失败 %s: %w", dir, err)
		}
		// 多连接并发（SMTP/IMAP/POP3/Web/外发 worker/推送）下：
		// - WAL 模式：读不阻塞写，消除瞬时 SQLITE_BUSY 导致写失败被吞
		// - busy_timeout=5000ms：写竞争时等待而非立刻失败
		// - synchronous=NORMAL：WAL 下安全且写入更快
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn = dsn + sep + "_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
		dialector = sqlite.Open(dsn)
	case "mysql":
		dialector = mysql.Open(cfg.DSN)
	default:
		return nil, fmt.Errorf("不支持的数据库驱动: %s", cfg.Driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// AutoMigrate 前清理 ban_entries 的历史重复行（保留每 IP 最大 id）：
	// ip_address 将升级为唯一索引，重复行会使索引创建失败。
	// 首次安装表不存在时忽略错误（AutoMigrate 会建新表）。
	dedupeBanEntries(db)
	// 再清理旧模型遗留的同名非唯一索引（见 dropLegacyBanIndex）。
	dropLegacyBanIndex(db)

	// Auto-migrate all models
	if err := db.AutoMigrate(&User{}, &Domain{}, &Message{}, &Attachment{}, &BanEntry{}, &OutboundMessage{}, &ProtocolLog{}, &MailboxState{}, &Mailbox{}); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return db, nil
}

// dedupeBanEntries 删除 ban_entries 中同一 ip_address 的重复行（保留
// 每组 id 最大的一条），为 ip_address 唯一索引的 AutoMigrate 扫清障碍。
// 表不存在（首次安装）时静默跳过；清理失败仅告警，不阻断启动
// （索引创建失败会在 AutoMigrate 中显式报错）。
func dedupeBanEntries(db *gorm.DB) {
	// SQLite 与 MySQL 均支持；MySQL 不允许 DELETE 子查询直接引用同表
	// （1093），因此用派生表包一层。
	sql := "DELETE FROM ban_entries WHERE id NOT IN (" +
		"SELECT mid FROM (SELECT MAX(id) AS mid FROM ban_entries GROUP BY ip_address) AS t)"
	if err := db.Exec(sql).Error; err != nil {
		// 表不存在（首次安装）为预期情况
		msg := err.Error()
		if strings.Contains(msg, "no such table") || strings.Contains(msg, "doesn't exist") {
			return
		}
		log.Printf("清理 ban_entries 重复行失败（唯一索引可能无法创建）: %v", err)
	}
}

// dropLegacyBanIndex 删除 ban_entries 上历史遗留的同名非唯一索引
// （旧模型 IPAddress 为普通 `index` 时由 AutoMigrate 创建）。
// IPAddress 升级为 uniqueIndex 后，MySQL 的 MigrateColumnUnique 会对
// 非唯一列无条件执行 CREATE UNIQUE INDEX（该路径不检查 HasIndex），
// 与同名旧索引冲突时报 Error 1061 Duplicate key name，导致启动即失败
// （AutoMigrate 末尾按名查 HasIndex 的保护根本执行不到）。
// 仅 MySQL 存在此问题；SQLite 迁移路径不受影响。
// 表不存在（首次安装）时静默跳过；失败仅告警，不阻断启动
// （索引创建失败会在 AutoMigrate 中显式报错）。
func dropLegacyBanIndex(db *gorm.DB) {
	if db.Dialector.Name() != "mysql" {
		return
	}
	var cnt int64
	err := db.Raw(
		"SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema = DATABASE() AND table_name = 'ban_entries' AND index_name = 'idx_ban_entries_ip_address' AND non_unique = 1",
	).Scan(&cnt).Error
	if err != nil {
		// 表不存在（首次安装）为预期情况
		if strings.Contains(err.Error(), "doesn't exist") {
			return
		}
		log.Printf("检查 ban_entries 遗留索引失败（唯一索引可能无法创建）: %v", err)
		return
	}
	if cnt == 0 {
		return
	}
	if err := db.Exec("ALTER TABLE `ban_entries` DROP INDEX `idx_ban_entries_ip_address`").Error; err != nil {
		log.Printf("删除 ban_entries 遗留非唯一索引失败（唯一索引可能无法创建）: %v", err)
		return
	}
	log.Printf("已删除 ban_entries 遗留非唯一索引 idx_ban_entries_ip_address（AutoMigrate 将重建为唯一索引）")
}
