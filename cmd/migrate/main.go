// migrate 一次性工具：把 SQLite 数据迁移到 MySQL（mailgo 库）。
// 用法：go run ./cmd/migrate -from /srv/mail_go/mail.db -dsn "mailgo:密码@tcp(127.0.0.1:3306)/mailgo?charset=utf8mb4&parseTime=True&loc=UTC"
package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"mail_go/config"
	"mail_go/internal/db"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	fromDSN = flag.String("from", "/srv/mail_go/mail.db", "SQLite 数据库路径")
	mysqlDSN = flag.String("dsn", "", "MySQL DSN（目标库，需已创建 mailgo 库与用户）")
)

func main() {
	flag.Parse()
	if *mysqlDSN == "" {
		log.Fatal("缺少 -dsn")
	}

	// 目标：MySQL（InitDB 内含 AutoMigrate，按当前模型建表）
	mdb, err := db.InitDB(config.DatabaseConfig{Driver: "mysql", DSN: *mysqlDSN}, config.StorageConfig{BaseDir: "/srv/mail_go/"})
	if err != nil {
		log.Fatalf("连接 MySQL 失败: %v", err)
	}
	log.Println("MySQL 建表完成（AutoMigrate）")

	// 源：SQLite（只读）
	sdb, err := db.InitDB(config.DatabaseConfig{Driver: "sqlite", DSN: *fromDSN}, config.StorageConfig{BaseDir: "/srv/mail_go/"})
	if err != nil {
		log.Fatalf("连接 SQLite 失败: %v", err)
	}
	sdb.Logger = logger.Default.LogMode(logger.Silent)

	// 关闭 GORM 自动时间戳（保留原始 CreatedAt/UpdatedAt）
	mw := mdb.Session(&gorm.Session{SkipHooks: true})

	stateWant := int64(-1) // mailbox_states 期望行数；-1 = 源库无此表不校验

	// 按外键依赖顺序复制：domains → users → messages → attachments → 其余
	// 所有时间统一 UTC（MySQL DATETIME 无时区）。
	utc := func(t time.Time) time.Time {
		if t.IsZero() {
			// MySQL DATETIME 最小年份 1000；零值由调用方转 NULL
			return t
		}
		return t.UTC()
	}
	_ = utc

	// ---- domains ----
	var domains []db.Domain
	if err := sdb.Order("id").Find(&domains).Error; err != nil {
		log.Fatalf("读 domains: %v", err)
	}
	for i := range domains {
		domains[i].CreatedAt = domains[i].CreatedAt.UTC()
		domains[i].UpdatedAt = domains[i].UpdatedAt.UTC()
	}
	if err := mw.Create(&domains).Error; err != nil {
		log.Fatalf("写 domains: %v", err)
	}
	log.Printf("domains: %d", len(domains))

	// ---- users ----
	var users []db.User
	if err := sdb.Order("id").Find(&users).Error; err != nil {
		log.Fatalf("读 users: %v", err)
	}
	for i := range users {
		users[i].CreatedAt = users[i].CreatedAt.UTC()
		users[i].UpdatedAt = users[i].UpdatedAt.UTC()
	}
	if err := mw.Create(&users).Error; err != nil {
		log.Fatalf("写 users: %v", err)
	}
	log.Printf("users: %d", len(users))

	// ---- messages ----
	var msgs []db.Message
	if err := sdb.Order("id").Find(&msgs).Error; err != nil {
		log.Fatalf("读 messages: %v", err)
	}
	for i := range msgs {
		msgs[i].Date = msgs[i].Date.UTC()
		msgs[i].CreatedAt = msgs[i].CreatedAt.UTC()
	}
	if err := mw.Create(&msgs).Error; err != nil {
		log.Fatalf("写 messages: %v", err)
	}
	log.Printf("messages: %d", len(msgs))

	// ---- attachments ----
	var atts []db.Attachment
	if err := sdb.Order("id").Find(&atts).Error; err != nil {
		log.Fatalf("读 attachments: %v", err)
	}
	for i := range atts {
		atts[i].CreatedAt = atts[i].CreatedAt.UTC()
	}
	if err := mw.Create(&atts).Error; err != nil {
		log.Fatalf("写 attachments: %v", err)
	}
	log.Printf("attachments: %d", len(atts))

	// ---- outbound_messages（原样，含时间转 UTC）----
	var outs []db.OutboundMessage
	if err := sdb.Order("id").Find(&outs).Error; err != nil {
		log.Fatalf("读 outbound_messages: %v", err)
	}
	for i := range outs {
		outs[i].NextAttemptAt = outs[i].NextAttemptAt.UTC()
		if outs[i].CompletedAt != nil && !outs[i].CompletedAt.IsZero() {
			u := outs[i].CompletedAt.UTC()
			outs[i].CompletedAt = &u
		}
		outs[i].CreatedAt = outs[i].CreatedAt.UTC()
		outs[i].UpdatedAt = outs[i].UpdatedAt.UTC()
	}
	if err := mw.Create(&outs).Error; err != nil {
		log.Fatalf("写 outbound_messages: %v", err)
	}
	log.Printf("outbound_messages: %d", len(outs))

	// ---- ban_entries（expires_at 零值 → NULL）----
	rows, err := sdb.Raw("SELECT id, ip_address, reason, fail_count, ban_count, expires_at, created_at, updated_at FROM ban_entries ORDER BY id").Rows()
	if err != nil {
		log.Fatalf("读 ban_entries: %v", err)
	}
	defer rows.Close()
	bans := 0
	for rows.Next() {
		var (
			id        uint
			ip        string
			reason    *string
			failCount int
			banCount  int
			expires   *time.Time
			created   *time.Time
			updated   *time.Time
		)
		if err := rows.Scan(&id, &ip, &reason, &failCount, &banCount, &expires, &created, &updated); err != nil {
			log.Fatalf("扫 ban_entries: %v", err)
		}
		norm := func(t *time.Time) *time.Time {
			if t == nil || t.IsZero() {
				return nil
			}
			u := t.UTC()
			return &u
		}
		if err := mdb.Exec("INSERT INTO ban_entries (id, ip_address, reason, fail_count, ban_count, expires_at, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)",
			id, ip, reason, failCount, banCount, norm(expires), norm(created), norm(updated)).Error; err != nil {
			log.Fatalf("写 ban_entries id=%d: %v", id, err)
		}
		bans++
	}
	log.Printf("ban_entries: %d", bans)

	// ---- protocol_logs ----
	var logs []db.ProtocolLog
	if err := sdb.Order("id").Find(&logs).Error; err != nil {
		log.Fatalf("读 protocol_logs: %v", err)
	}
	for i := range logs {
		logs[i].CreatedAt = logs[i].CreatedAt.UTC()
	}
	if err := mw.Create(&logs).Error; err != nil {
		log.Fatalf("写 protocol_logs: %v", err)
	}
	log.Printf("protocol_logs: %d", len(logs))

	// ---- mailbox_states（原生 SQL：该表随 UIDVALIDITY 特性存在，旧版本源库可能没有）----
	var stateCount int64
	sdb.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='mailbox_states'").Scan(&stateCount)
	if stateCount > 0 {
		// 目标库若没有该表（上游模型未含 MailboxState 时 AutoMigrate 不会建），先建表
		var tcnt int64
		mdb.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'mailbox_states'").Scan(&tcnt)
		if tcnt == 0 {
			if err := mdb.Exec(`CREATE TABLE mailbox_states (
				user_id bigint unsigned NOT NULL,
				folder varchar(64) NOT NULL,
				uid_validity bigint unsigned NOT NULL,
				created_at datetime(3) NULL,
				updated_at datetime(3) NULL,
				PRIMARY KEY (user_id, folder))`).Error; err != nil {
				log.Fatalf("建 mailbox_states 表: %v", err)
			}
			log.Println("mailbox_states: 目标库已建表")
		}
		srows, err := sdb.Raw("SELECT user_id, folder, uid_validity, created_at, updated_at FROM mailbox_states ORDER BY user_id, folder").Rows()
		if err != nil {
			log.Fatalf("读 mailbox_states: %v", err)
		}
		defer srows.Close()
		states := 0
		for srows.Next() {
			var (
				userID   uint
				folder   string
				validity uint32
				created  *time.Time
				updated  *time.Time
			)
			if err := srows.Scan(&userID, &folder, &validity, &created, &updated); err != nil {
				log.Fatalf("扫 mailbox_states: %v", err)
			}
			norm := func(t *time.Time) *time.Time {
				if t == nil || t.IsZero() {
					return nil
				}
				u := t.UTC()
				return &u
			}
			if err := mdb.Exec("INSERT INTO mailbox_states (user_id, folder, uid_validity, created_at, updated_at) VALUES (?,?,?,?,?)",
				userID, folder, validity, norm(created), norm(updated)).Error; err != nil {
				log.Fatalf("写 mailbox_states: %v", err)
			}
			states++
		}
		log.Printf("mailbox_states: %d", states)
		stateWant = int64(states)
	} else {
		log.Println("mailbox_states: 源库无此表，跳过")
	}

	// ---- 校验 ----
	check := func(table string, want int64) {
		var got int64
		if err := mdb.Table(table).Count(&got).Error; err != nil {
			log.Fatalf("校验 %s: %v", table, err)
		}
		if got != want {
			log.Fatalf("校验 %s 失败: got %d want %d", table, got, want)
		}
		fmt.Printf("校验 %s: %d/%d ✓\n", table, got, want)
	}
	check("domains", int64(len(domains)))
	check("users", int64(len(users)))
	check("messages", int64(len(msgs)))
	check("attachments", int64(len(atts)))
	check("outbound_messages", int64(len(outs)))
	check("ban_entries", int64(bans))
	check("protocol_logs", int64(len(logs)))
	if stateWant >= 0 {
		check("mailbox_states", stateWant)
	}

	log.Println("迁移完成 ✅")
}
