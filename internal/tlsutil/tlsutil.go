// Package tlsutil 提供 TLS 证书热加载：每次 TLS 握手时按需检查
// 证书路径与文件内容是否变化，变化则自动重载，证书更新后无需重启
// 服务即可生效。
package tlsutil

import (
	"crypto/tls"
	"fmt"
	"os"
	"sync"
	"time"
)

// retryInterval 是重载失败后的最小重试间隔，避免证书文件损坏时
// 每个握手都重复做无意义的磁盘读取。
const retryInterval = 5 * time.Second

// Source 返回当前应使用的证书路径。路径可能随时间变化（例如管理后台
// 一键导入证书后切换到新的域名证书）；返回空路径表示暂无可用证书。
type Source func() (certPath, keyPath string)

// Loader 管理一对可热加载的证书。所有方法均并发安全。
type Loader struct {
	mu       sync.Mutex
	source   Source
	certPath string
	keyPath  string
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	lastTry  time.Time
	logf     func(format string, args ...interface{})
}

// NewLoader 立即加载并校验证书，失败返回错误（保持启动时 fail-fast）。
// source 为 nil 时证书路径固定不变，仅检测文件内容变化。
func NewLoader(certPath, keyPath string, source Source, logf func(string, ...interface{})) (*Loader, error) {
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("TLS 证书路径为空")
	}
	l := &Loader{
		source:   source,
		certPath: certPath,
		keyPath:  keyPath,
		logf:     logf,
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// GetCertificate 实现 tls.Config.GetCertificate：
// 每次 TLS 握手时检查证书路径与文件是否有变化，有则自动重载；
// 重载失败时继续使用上一次成功加载的证书，避免中断现有服务。
func (l *Loader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	certPath, keyPath := l.certPath, l.keyPath
	if l.source != nil {
		certPath, keyPath = l.source()
	}
	if certPath == "" || keyPath == "" {
		// 暂无证书可用：继续使用旧证书（若有）
		return l.current()
	}

	changed := certPath != l.certPath || keyPath != l.keyPath
	if !changed {
		changed = l.filesChanged(certPath, keyPath)
	}

	// 仅在重载失败后节流（避免证书文件损坏时每个握手都重复读盘）；
	// 成功后清零节流，保证正常的连续更新立即生效。
	if changed && time.Since(l.lastTry) >= retryInterval {
		l.lastTry = time.Now()
		l.certPath, l.keyPath = certPath, keyPath
		if err := l.load(); err != nil {
			if l.logf != nil {
				l.logf("TLS 证书重载失败 (%s, %s): %v，继续使用旧证书", certPath, keyPath, err)
			}
		} else {
			l.lastTry = time.Time{}
			if l.logf != nil {
				l.logf("TLS 证书已热加载: %s", certPath)
			}
		}
	}

	return l.current()
}

// current 返回当前已加载的证书。
func (l *Loader) current() (*tls.Certificate, error) {
	if l.cert == nil {
		return nil, fmt.Errorf("TLS 证书不可用")
	}
	return l.cert, nil
}

// filesChanged 判断证书/私钥文件自上次加载后是否被修改。
// 文件暂时不可读（如正在原子替换）时视为已变化，触发重载尝试。
func (l *Loader) filesChanged(certPath, keyPath string) bool {
	stC, errC := os.Stat(certPath)
	stK, errK := os.Stat(keyPath)
	if errC != nil || errK != nil {
		return true
	}
	return !stC.ModTime().Equal(l.certMod) || !stK.ModTime().Equal(l.keyMod)
}

// load 从当前路径加载证书对并记录文件修改时间。
func (l *Loader) load() error {
	cert, err := tls.LoadX509KeyPair(l.certPath, l.keyPath)
	if err != nil {
		return err
	}
	if stC, err := os.Stat(l.certPath); err == nil {
		l.certMod = stC.ModTime()
	}
	if stK, err := os.Stat(l.keyPath); err == nil {
		l.keyMod = stK.ModTime()
	}
	l.cert = &cert
	return nil
}
