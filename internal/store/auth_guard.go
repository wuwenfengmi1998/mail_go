package store

import (
	"fmt"
	"net"
	"time"

	"mail_go/internal/db"
)

// ClientIPFromAddr 从 net.Addr 提取客户端 IP 字符串（去掉端口）。
// 解析失败返回空字符串，调用方应据此跳过封禁逻辑（不误封）。
func ClientIPFromAddr(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}

// RecordAuthFailure 记录一次协议层（SMTP/IMAP/POP3）认证失败：
// 失败计数累加，达到 maxFail 阈值时封禁该 IP（封禁时长 minutes 分钟）。
// 返回 (是否触发封禁, 当前失败计数)。Web 登录的封禁逻辑在
// handlers.AuthHandler 中，与这里独立。
func (s *Stores) RecordAuthFailure(ip string, maxFail int, minutes int) (banned bool, failCount int) {
	if ip == "" || maxFail <= 0 {
		return false, 0
	}
	failCount, _ = s.Bans.IncrementFail(ip)
	if failCount >= maxFail {
		_ = s.Bans.Create(&db.BanEntry{
			IPAddress: ip,
			Reason:    fmt.Sprintf("邮件协议认证失败次数过多 (%d次)", failCount),
			FailCount: failCount,
			ExpiresAt: time.Now().Add(time.Duration(minutes) * time.Minute),
		})
		return true, failCount
	}
	return false, failCount
}
