package db

import (
	"fmt"
	"os"
	"path/filepath"

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

	// Auto-migrate all models
	if err := db.AutoMigrate(&User{}, &Domain{}, &Message{}, &Attachment{}); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return db, nil
}
