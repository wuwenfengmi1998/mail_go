package outbound

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTPServer captures a single SMTP transaction for inspection.
type fakeSMTPServer struct {
	ln      net.Listener
	done    chan struct{}
	gotData []byte
	cmds    []string
	err     error
}

func startFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTPServer{ln: ln, done: make(chan struct{})}
	go f.serve()
	t.Cleanup(func() {
		ln.Close()
		<-f.done
	})
	return f
}

func (f *fakeSMTPServer) serve() {
	defer close(f.done)
	conn, err := f.ln.Accept()
	if err != nil {
		f.err = err
		return
	}
	defer conn.Close()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	write := func(s string) {
		_, _ = w.WriteString(s)
		_ = w.Flush()
	}

	write("220 fake.test ESMTP ready\r\n")

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			f.err = err
			return
		}
		line = strings.TrimRight(line, "\r\n")
		f.cmds = append(f.cmds, line)

		switch {
		case strings.HasPrefix(strings.ToUpper(line), "EHLO"), strings.HasPrefix(strings.ToUpper(line), "HELO"):
			write("250-fake.test\r\n250 8BITMIME\r\n")
		case strings.HasPrefix(strings.ToUpper(line), "MAIL FROM"):
			write("250 2.0.0 ok\r\n")
		case strings.HasPrefix(strings.ToUpper(line), "RCPT TO"):
			write("250 2.0.0 ok\r\n")
		case strings.HasPrefix(strings.ToUpper(line), "DATA"):
			write("354 go ahead\r\n")
			// Read until the terminating dot line, un-stuffing dot lines
			// exactly like a real SMTP receiver.
			for {
				dl, err := r.ReadString('\n')
				if err != nil {
					f.err = err
					return
				}
				if strings.TrimRight(dl, "\r\n") == "." {
					break
				}
				if strings.HasPrefix(dl, "..") {
					dl = dl[1:]
				}
				f.gotData = append(f.gotData, []byte(dl)...)
			}
			write("250 2.0.0 queued\r\n")
		case strings.HasPrefix(strings.ToUpper(line), "QUIT"):
			write("221 bye\r\n")
			return
		default:
			write("500 huh\r\n")
		}
	}
}

func (f *fakeSMTPServer) port() int {
	return f.ln.Addr().(*net.TCPAddr).Port
}

func TestMailerWireFormat(t *testing.T) {
	f := startFakeSMTPServer(t)

	m := NewMailer("mail.lmve.net", 10*time.Second)
	m.Port = f.port()

	input := []byte("From: a@lmve.net\r\n" +
		"To: b@fake.test\r\n" +
		"Subject: wire test\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"line one\r\n" +
		".leading dot must be stuffed\r\n" +
		"line three\r\n")

	resp, err := m.deliverToHost("127.0.0.1", "a@lmve.net", "b@fake.test", input)
	if err != nil {
		t.Fatalf("deliverToHost: %v", err)
	}
	if !strings.HasPrefix(resp, "250") {
		t.Fatalf("unexpected response: %q", resp)
	}

	if f.err != nil {
		t.Fatalf("fake server error: %v", f.err)
	}
	if len(f.gotData) == 0 {
		t.Fatal("no DATA received")
	}

	// The captured data must be dot-UNstuffed: identical to the input.
	// (The fake server un-stuffs dot lines like a real receiver, so a match
	// here proves the mailer applied correct dot-stuffing on the wire.)
	if string(f.gotData) != string(input) {
		t.Fatalf("wire data mismatch.\ngot:  %q\nwant: %q", f.gotData, input)
	}
}

func TestMailer8BitMIME(t *testing.T) {
	f := startFakeSMTPServer(t)

	m := NewMailer("mail.lmve.net", 10*time.Second)
	m.Port = f.port()

	// 8-bit body (UTF-8) with a server that advertises 8BITMIME.
	input := []byte("From: a@lmve.net\r\nTo: b@fake.test\r\nSubject: 8bit\r\n\r\n你好世界\r\n")
	if _, err := m.deliverToHost("127.0.0.1", "a@lmve.net", "b@fake.test", input); err != nil {
		t.Fatalf("deliverToHost: %v", err)
	}

	found := false
	for _, cmd := range f.cmds {
		if strings.HasPrefix(strings.ToUpper(cmd), "MAIL FROM") {
			if strings.Contains(strings.ToUpper(cmd), "BODY=8BITMIME") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected MAIL FROM with BODY=8BITMIME, got: %v", f.cmds)
	}
}

func TestMailerPermanentFailure(t *testing.T) {
	// A fake server that rejects the recipient with 550.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		_, _ = w.WriteString("220 reject.test ESMTP\r\n")
		_ = w.Flush()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(strings.ToUpper(line), "EHLO"):
				_, _ = w.WriteString("250 reject.test\r\n")
				_ = w.Flush()
			case strings.HasPrefix(strings.ToUpper(line), "MAIL"):
				_, _ = w.WriteString("250 ok\r\n")
				_ = w.Flush()
			case strings.HasPrefix(strings.ToUpper(line), "RCPT"):
				_, _ = w.WriteString("550 5.1.1 no such user\r\n")
				_ = w.Flush()
			case strings.HasPrefix(strings.ToUpper(line), "QUIT"):
				_, _ = w.WriteString("221 bye\r\n")
				_ = w.Flush()
				return
			}
		}
	}()

	m := NewMailer("mail.lmve.net", 10*time.Second)
	m.Port = ln.Addr().(*net.TCPAddr).Port

	input := []byte("From: a@lmve.net\r\nTo: b@reject.test\r\nSubject: t\r\n\r\nbody\r\n")
	_, err = m.deliverToHost("127.0.0.1", "a@lmve.net", "b@reject.test", input)
	if err == nil {
		t.Fatal("expected error for 550 rejection")
	}
	de, ok := err.(*DeliveryError)
	if !ok {
		t.Fatalf("expected *DeliveryError, got %T: %v", err, err)
	}
	if !de.Permanent {
		t.Fatalf("expected permanent error, got %+v", de)
	}
	if de.Code != 550 {
		t.Fatalf("expected code 550, got %d", de.Code)
	}
	<-done
}

func TestMailerSmarthostRelay(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type result struct {
		gotData  []byte
		authLine string
	}
	ch := make(chan result, 1)

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			ch <- result{}
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		w := bufio.NewWriter(conn)
		_, _ = w.WriteString("220 relay.test ESMTP\r\n")
		_ = w.Flush()

		var authLine string
		var got []byte
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break
			}
			trimmed := strings.TrimRight(line, "\r\n")
			up := strings.ToUpper(trimmed)
			switch {
			case strings.HasPrefix(up, "EHLO"):
				_, _ = w.WriteString("250-relay.test\r\n250-8BITMIME\r\n250 AUTH PLAIN\r\n")
				_ = w.Flush()
			case strings.HasPrefix(up, "AUTH PLAIN"):
				authLine = trimmed
				_, _ = w.WriteString("235 2.0.0 ok\r\n")
				_ = w.Flush()
			case strings.HasPrefix(up, "MAIL FROM"):
				if authLine == "" {
					_, _ = w.WriteString("530 5.7.0 auth required\r\n")
					_ = w.Flush()
					break
				}
				_, _ = w.WriteString("250 ok\r\n")
				_ = w.Flush()
			case strings.HasPrefix(up, "RCPT TO"):
				_, _ = w.WriteString("250 ok\r\n")
				_ = w.Flush()
			case strings.HasPrefix(up, "DATA"):
				_, _ = w.WriteString("354 go\r\n")
				_ = w.Flush()
				for {
					dl, err := r.ReadString('\n')
					if err != nil {
						break
					}
					if strings.TrimRight(dl, "\r\n") == "." {
						break
					}
					if strings.HasPrefix(dl, "..") {
						dl = dl[1:]
					}
					got = append(got, []byte(dl)...)
				}
				_, _ = w.WriteString("250 queued\r\n")
				_ = w.Flush()
			case strings.HasPrefix(up, "QUIT"):
				_, _ = w.WriteString("221 bye\r\n")
				_ = w.Flush()
				ch <- result{gotData: got, authLine: authLine}
				return
			}
		}
		ch <- result{}
	}()

	m := NewMailer("mail.lmve.net", 10*time.Second)
	m.Relay = &RelayConfig{
		Host:     "127.0.0.1",
		Port:     ln.Addr().(*net.TCPAddr).Port,
		Username: "relay-user",
		Password: "relay-pass",
		StartTLS: false,
	}

	// The recipient domain does not even exist — with a relay configured,
	// no MX lookup happens and the relay still receives the message.
	input := []byte("From: a@lmve.net\r\nTo: b@bogus-domain.invalid\r\nSubject: relay\r\n\r\nbody\r\n")
	resp, err := m.Deliver("a@lmve.net", "b@bogus-domain.invalid", input)
	if err != nil {
		t.Fatalf("Deliver via relay: %v", err)
	}
	if !strings.HasPrefix(resp, "250") {
		t.Fatalf("unexpected relay response: %q", resp)
	}

	res := <-ch
	if res.authLine == "" {
		t.Fatal("relay did not receive AUTH PLAIN")
	}
	wantAuth := "AUTH PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00relay-user\x00relay-pass"))
	if res.authLine != wantAuth {
		t.Fatalf("auth line mismatch: got %q want %q", res.authLine, wantAuth)
	}
	if string(res.gotData) != string(input) {
		t.Fatalf("relay data mismatch.\ngot:  %q\nwant: %q", res.gotData, input)
	}
}

// startTLSSMTPServer 起一个支持 STARTTLS 的 SMTP 服务器（自签证书），
// 供 relay TLS 验证测试使用：未升级 TLS 时广告 STARTTLS 能力，
// 收到 STARTTLS 后升级为 TLS 并重新 EHLO。
func startTLSSMTPServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	cert, err := tls.X509KeyPair(makeSelfSignedCertPEM(t))
	if err != nil {
		t.Fatalf("load self-signed cert: %v", err)
	}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := bufio.NewReader(conn)
				w := bufio.NewWriter(conn)
				_, _ = w.WriteString("220 relay.test ESMTP ready\r\n")
				_ = w.Flush()
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					up := strings.ToUpper(strings.TrimRight(line, "\r\n"))
					switch {
					case strings.HasPrefix(up, "STARTTLS"):
						_, _ = w.WriteString("220 2.0.0 ready to start TLS\r\n")
						_ = w.Flush()
						tlsConn := tls.Server(conn, tlsCfg)
						if err := tlsConn.Handshake(); err != nil {
							return
						}
						conn = tlsConn
						r = bufio.NewReader(conn)
						w = bufio.NewWriter(conn)
					case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
						_, _ = w.WriteString("250-relay.test\r\n250-STARTTLS\r\n250 8BITMIME\r\n")
						_ = w.Flush()
					case strings.HasPrefix(up, "AUTH PLAIN"):
						_, _ = w.WriteString("235 2.0.0 ok\r\n")
						_ = w.Flush()
					case strings.HasPrefix(up, "DATA"):
						_, _ = w.WriteString("354 go ahead\r\n")
						_ = w.Flush()
						for {
							dl, err := r.ReadString('\n')
							if err != nil {
								return
							}
							if strings.TrimRight(dl, "\r\n") == "." {
								break
							}
						}
						_, _ = w.WriteString("250 2.0.0 queued\r\n")
						_ = w.Flush()
					case strings.HasPrefix(up, "QUIT"):
						_, _ = w.WriteString("221 bye\r\n")
						_ = w.Flush()
						return
					default:
						_, _ = w.WriteString("250 ok\r\n")
						_ = w.Flush()
					}
				}
			}()
		}
	}()

	addr = ln.Addr().String()
	return addr, func() { ln.Close() }
}

// makeSelfSignedCertPEM 生成一对自签证书（CN=relay.test）。
func makeSelfSignedCertPEM(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     []string{"relay.test"},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return certPEM, keyPEM
}

// TestMailerRelayRejectsUntrustedCert 中继使用自签证书时默认必须拒绝
// （证书验证开启，防止凭据被中间人截获）。
func TestMailerRelayRejectsUntrustedCert(t *testing.T) {
	addr, cleanup := startTLSSMTPServer(t)
	defer cleanup()

	m := NewMailer("mail.lmve.net", 5*time.Second)
	m.Relay = &RelayConfig{
		Host:        "127.0.0.1",
		Port:        mustPort(t, addr),
		Username:    "relay-user",
		Password:    "relay-pass",
		TLSInsecure: false,
	}

	input := []byte("From: a@lmve.net\r\nTo: b@bogus-domain.invalid\r\nSubject: relay\r\n\r\nbody\r\n")
	_, err := m.Deliver("a@lmve.net", "b@bogus-domain.invalid", input)
	if err == nil {
		t.Fatal("relay with untrusted self-signed cert should be rejected")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected certificate verification error, got: %v", err)
	}
}

// TestMailerRelayInsecureSkipsVerification 显式开启 relay_tls_insecure
// 后，自签证书的中继可以完成 TLS 握手并进入 SMTP 会话。
func TestMailerRelayInsecureSkipsVerification(t *testing.T) {
	addr, cleanup := startTLSSMTPServer(t)
	defer cleanup()

	m := NewMailer("mail.lmve.net", 5*time.Second)
	m.Relay = &RelayConfig{
		Host:        "127.0.0.1",
		Port:        mustPort(t, addr),
		Username:    "relay-user",
		Password:    "relay-pass",
		TLSInsecure: true,
	}

	input := []byte("From: a@lmve.net\r\nTo: b@bogus-domain.invalid\r\nSubject: relay\r\n\r\nbody\r\n")
	resp, err := m.Deliver("a@lmve.net", "b@bogus-domain.invalid", input)
	if err != nil {
		t.Fatalf("relay with TLSInsecure should proceed: %v", err)
	}
	if !strings.HasPrefix(resp, "250") {
		t.Fatalf("unexpected response: %q", resp)
	}
}

func mustPort(t *testing.T, addr string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}
	return port
}
