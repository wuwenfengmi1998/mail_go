// Package caddycert 从本机 Caddy 的证书存储中查找并读取某个域名
// 的 TLS 证书与私钥，供 MailGo 一键导入使用。
//
// Caddy（certmagic）将 ACME 证书保存在其数据目录下的
//
//	<data>/certificates/<CA 目录>/<域名>/<域名>.crt
//	<data>/certificates/<CA 目录>/<域名>/<域名>.key
//
// 数据目录默认是 $HOME/.local/share/caddy（systemd 服务通常是
// /var/lib/caddy/.local/share/caddy），可通过配置 caddy.data_dir 覆盖。
package caddycert

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDataDirs 是未显式配置时依次探测的 Caddy 数据目录。
var DefaultDataDirs = []string{
	"/var/lib/caddy/.local/share/caddy", // Debian/Ubuntu 软件包的 systemd 服务默认 HOME
	"/root/.local/share/caddy",          // 直接以 root 运行的 caddy
	"/home/caddy/.local/share/caddy",
}

// Cert 是从 Caddy 存储中找到的一对证书与私钥（PEM 编码）。
type Cert struct {
	CertPEM []byte // 证书链（含叶子证书）
	KeyPEM  []byte // 私钥
	Source  string // 来源 .crt 文件的绝对路径
}

// Fetch 在给定的 Caddy 证书数据目录中查找 domain 的证书与私钥。
//
// dataDirs 按优先级从高到低排列，每个目录都是包含 certificates/ 子目录的
// Caddy 数据目录（如 /var/lib/caddy/.local/share/caddy，或 mail_go 的同步
// 镜像目录 /srv/mail_go/tls/caddy）；空字符串项被忽略。dataDirs 为空时仅
// 探测 DefaultDataDirs 及当前进程用户的数据目录。
//
// 返回的证书保证：能组成有效的密钥对、尚未过期、且证书 SAN 覆盖 domain
// （支持通配符证书，例如 *.example.com 的证书可匹配 mail.example.com）。
func Fetch(domain string, dataDirs []string) (*Cert, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("域名为空")
	}

	roots := dataRoots(dataDirs)

	var (
		permDenied []string
		seen       []string // 找到同名/相关文件但证书无效的来源
	)
	for _, root := range roots {
		cert, found, invalid, err := searchRoot(domain, root)
		if err != nil {
			if os.IsPermission(err) {
				permDenied = append(permDenied, root)
				continue
			}
			continue
		}
		if found {
			return cert, nil
		}
		seen = append(seen, invalid...)
	}

	msg := fmt.Sprintf("在 Caddy 证书存储中未找到域名 %q 的证书（请确认 Caddy 已为该域名签发证书）", domain)
	if len(seen) > 0 {
		msg += fmt.Sprintf("；发现相关文件但证书无效/已过期/不匹配域名: %s", strings.Join(seen, "、"))
	}
	if len(permDenied) > 0 {
		msg += fmt.Sprintf("；另有目录因权限不足未能检查: %s，可运行 install.sh 的 setup-caddy-cert 授予 %s 用户读取权限",
			strings.Join(permDenied, "、"), currentUsername())
	}
	return nil, fmt.Errorf("%s", msg)
}

// dataRoots 返回要探测的候选数据目录列表（去重，保留优先级顺序）。
func dataRoots(dataDirs []string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimRight(filepath.Clean(p), string(filepath.Separator))
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		roots = append(roots, p)
	}

	for _, d := range dataDirs {
		add(d)
	}
	for _, d := range DefaultDataDirs {
		add(d)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".local", "share", "caddy"))
	}
	return roots
}

// searchRoot 在单个数据目录中查找 domain 的证书。
// 返回 (证书, 是否找到, 找到但无效的来源列表, 错误)。
func searchRoot(domain, root string) (*Cert, bool, []string, error) {
	// 允许把 certificates/ 目录本身当作 data_dir 传入
	certsDir := root
	if filepath.Base(certsDir) != "certificates" {
		certsDir = filepath.Join(root, "certificates")
	}

	info, err := os.Stat(certsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, err
	}
	if !info.IsDir() {
		return nil, false, nil, nil
	}

	caDirs, err := os.ReadDir(certsDir)
	if err != nil {
		return nil, false, nil, err
	}

	var invalid []string

	// 1) 直接路径: certificates/<CA>/<domain>/<domain>.crt|.key
	for _, ca := range caDirs {
		if !ca.IsDir() {
			continue
		}
		cert, found, bad, err := readDomainDir(filepath.Join(certsDir, ca.Name(), domain), domain)
		if err != nil {
			return nil, false, nil, err
		}
		if found {
			return cert, true, nil, nil
		}
		invalid = append(invalid, bad...)
	}

	// 2) 全量扫描，处理通配符证书（如 *.example.com 目录）等情况
	for _, ca := range caDirs {
		if !ca.IsDir() {
			continue
		}
		caPath := filepath.Join(certsDir, ca.Name())
		domDirs, err := os.ReadDir(caPath)
		if err != nil {
			return nil, false, nil, err
		}
		for _, d := range domDirs {
			if !d.IsDir() {
				continue
			}
			cert, found, bad, err := readDomainDir(filepath.Join(caPath, d.Name()), domain)
			if err != nil {
				return nil, false, nil, err
			}
			if found {
				return cert, true, nil, nil
			}
			invalid = append(invalid, bad...)
		}
	}

	sort.Strings(invalid)
	return nil, false, invalid, nil
}

// readDomainDir 读取 Caddy 某个域名目录下的 <name>.crt 与 <name>.key，
// 校验其是否为 domain 的有效证书。bad 返回“存在但无效”的来源路径。
func readDomainDir(dirPath, domain string) (*Cert, bool, []string, error) {
	name := filepath.Base(dirPath)
	certPath := filepath.Join(dirPath, name+".crt")
	keyPath := filepath.Join(dirPath, name+".key")

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil, nil
		}
		return nil, false, nil, err
	}

	// 只有与目标域名相关的目录才值得报“无效”，否则静默跳过
	related := strings.TrimSuffix(name, "."+domain) == domain ||
		name == domain || strings.HasPrefix(name, "*.") && strings.HasSuffix(domain, name[1:])

	if !validPair(certPEM, keyPEM, domain) {
		if related {
			return nil, false, []string{certPath}, nil
		}
		return nil, false, nil, nil
	}

	return &Cert{CertPEM: certPEM, KeyPEM: keyPEM, Source: certPath}, true, nil, nil
}

// validPair 校验证书/私钥是否组成有效密钥对、未过期且 SAN 覆盖 domain。
func validPair(certPEM, keyPEM []byte, domain string) bool {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false
	}
	if len(pair.Certificate) == 0 {
		return false
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false
	}
	if time.Now().After(leaf.NotAfter) {
		return false
	}
	return leaf.VerifyHostname(domain) == nil
}

// currentUsername 返回当前进程的运行用户（错误提示用）。
func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}
