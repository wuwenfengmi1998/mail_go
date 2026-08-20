package store

import (
	"fmt"
	"net"
	"time"
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

// RecordAuthFailure 记录一次登录/认证失败（Web 表单、LDAP 与 SMTP/IMAP/POP3
// 协议层统一入口）：
//   - 失败计数累加（每 IP 一条记录，upsert）；
//   - 达到 maxFail 阈值时触发次数 BanCount+1：
//     knownUser（用户名存在、疑似真实用户输错）前 freeTriggers（3）次
//     只计数不封禁（防误封）；
//     !knownUser（用户名不存在，枚举型爆破）跳过宽限，首次触发即按
//     第 1 档封禁；
//     封禁时长按档位递增（stageDuration），上限半年；
//   - reason 为失败场景描述（如“登录失败次数过多”），封禁原因会带上档位。
//
// 返回 (是否触发封禁, 当前失败计数)。成功登录后调用 ResetFail 清零。
func (s *Stores) RecordAuthFailure(ip string, maxFail int, firstBanMin int, reason string, knownUser bool) (banned bool, failCount int) {
	if ip == "" || maxFail <= 0 {
		return false, 0
	}
	failCount, _ = s.Bans.IncrementFail(ip)
	if failCount < maxFail {
		return false, failCount
	}

	entry, err := s.Bans.GetByIP(ip)
	if err != nil || entry == nil {
		return false, failCount
	}
	// 已处于封禁中（例如并发请求竞态）不重复触发、不重设档位
	if entry.ExpiresAt.After(time.Now()) {
		return true, failCount
	}

	banCount := entry.BanCount + 1
	entry.BanCount = banCount
	entry.FailCount = failCount

	// 未知用户名（枚举型爆破）跳过宽限档：等效于已用完 3 次宽限
	effectiveCount := banCount
	if !knownUser {
		effectiveCount += freeTriggers
	}

	// 宽限期内只计数，不封禁（保留零到期时间与空原因）
	if effectiveCount <= freeTriggers {
		if err := s.Bans.Update(entry); err != nil {
			return false, failCount
		}
		return false, failCount
	}

	banNum := effectiveCount - freeTriggers
	if knownUser {
		entry.Reason = fmt.Sprintf("第%d次封禁：%s（第%d次触发，失败%d次）", banNum, reason, banCount, failCount)
	} else {
		entry.Reason = fmt.Sprintf("第%d次封禁：%s（未知用户名，跳过宽限；第%d次触发，失败%d次）", banNum, reason, banCount, failCount)
	}
	entry.ExpiresAt = time.Now().Add(stageDuration(banNum, firstBanMin))
	if err := s.Bans.Update(entry); err != nil {
		return false, failCount
	}
	return true, failCount
}
