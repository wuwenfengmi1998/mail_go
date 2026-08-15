// Package outbound implements external (outbound) email delivery.
//
// Messages queued for external recipients are stored in the outbound_messages
// table and delivered by the Manager's background worker: MX lookup, SMTP
// transaction over port 25 with opportunistic STARTTLS, exponential backoff
// retries, permanent-failure bounces and DKIM signing.
package outbound

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DeliveryError wraps an SMTP delivery failure and records whether it is
// permanent (5xx / NXDOMAIN / invalid address) or temporary (4xx / network /
// timeout). Temporary failures are retried by the queue worker.
type DeliveryError struct {
	Permanent bool
	Code      int
	Msg       string
}

func (e *DeliveryError) Error() string {
	if e.Code > 0 {
		return fmt.Sprintf("%d %s", e.Code, e.Msg)
	}
	return e.Msg
}

// newTempError creates a temporary delivery error.
func newTempError(format string, args ...interface{}) *DeliveryError {
	return &DeliveryError{Permanent: false, Msg: fmt.Sprintf(format, args...)}
}

// newPermError creates a permanent delivery error.
func newPermError(format string, args ...interface{}) *DeliveryError {
	return &DeliveryError{Permanent: true, Msg: fmt.Sprintf(format, args...)}
}

// Mailer performs direct MX delivery of a single message.
type Mailer struct {
	Hostname       string // EHLO hostname presented to remote servers
	Port           int    // destination port, 0 means the default SMTP port 25
	ConnectTimeout time.Duration
}

// NewMailer creates a Mailer with the given EHLO hostname and connect timeout.
func NewMailer(hostname string, connectTimeout time.Duration) *Mailer {
	if hostname == "" {
		hostname = "localhost"
	}
	return &Mailer{Hostname: hostname, ConnectTimeout: connectTimeout}
}

// port returns the destination port, defaulting to 25.
func (m *Mailer) port() int {
	if m.Port == 0 {
		return 25
	}
	return m.Port
}

// Deliver sends one message to one recipient via the recipient domain's MX.
// It returns the final SMTP response text on success and a *DeliveryError on
// failure.
func (m *Mailer) Deliver(from, to string, data []byte) (string, error) {
	at := strings.LastIndex(to, "@")
	if at < 0 || at == len(to)-1 {
		return "", newPermError("invalid recipient address: %s", to)
	}
	domain := strings.ToLower(strings.TrimSpace(to[at+1:]))

	mxHosts, err := lookupMX(domain)
	if err != nil {
		var de *DeliveryError
		if errors.As(err, &de) {
			return "", de
		}
		return "", newTempError("MX lookup failed for %s: %v", domain, err)
	}

	var lastErr *DeliveryError
	for _, host := range mxHosts {
		resp, err := m.deliverToHost(host, from, to, data)
		if err == nil {
			return resp, nil
		}
		var de *DeliveryError
		if errors.As(err, &de) {
			lastErr = de
			// A permanent failure from one MX applies to the whole message,
			// do not try other MX hosts.
			if de.Permanent {
				return "", de
			}
			continue
		}
		lastErr = newTempError("delivery to %s failed: %v", host, err)
	}
	if lastErr == nil {
		lastErr = newTempError("no MX hosts available for %s", domain)
	}
	return "", lastErr
}

// smtpClient wraps a textproto connection to a remote SMTP server.
type smtpClient struct {
	conn net.Conn
	txt  *textproto.Conn
	host string
	exts map[string]string // advertised EHLO extensions (upper-case key -> params)
}

func (c *smtpClient) Close() {
	if c.txt != nil {
		_ = c.txt.Close()
	}
}

// cmd sends a command and expects the given reply codes, returning the
// response text. Codes other than expected are returned as a DeliveryError.
func (c *smtpClient) cmd(expectCode int, format string, args ...interface{}) (int, string, error) {
	if err := c.txt.PrintfLine(format, args...); err != nil {
		return 0, "", newTempError("write to %s failed: %v", c.host, err)
	}
	code, msg, err := c.txt.ReadResponse(expectCode)
	if err != nil {
		return code, msg, classifyResponse(err, msg)
	}
	return code, msg, nil
}

// classifyResponse converts a textproto error (wrong reply code) into a
// DeliveryError, keeping the actual SMTP code and text.
func classifyResponse(err error, fallback string) *DeliveryError {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return &DeliveryError{
			Permanent: protoErr.Code >= 500,
			Code:      protoErr.Code,
			Msg:       protoErr.Msg,
		}
	}
	if fallback != "" {
		return newTempError("%s", fallback)
	}
	return newTempError("%v", err)
}

// hello sends EHLO and records the advertised extensions. If EHLO fails it
// falls back to HELO for very old servers.
func (c *smtpClient) hello(hostname string) error {
	if err := c.txt.PrintfLine("EHLO %s", hostname); err != nil {
		return newTempError("write EHLO to %s failed: %v", c.host, err)
	}
	code, msg, err := c.txt.ReadResponse(250)
	if err != nil {
		// Fall back to HELO.
		if err := c.txt.PrintfLine("HELO %s", hostname); err != nil {
			return newTempError("write HELO to %s failed: %v", c.host, err)
		}
		code, msg, err = c.txt.ReadResponse(250)
		if err != nil {
			return classifyResponse(err, msg)
		}
		return nil
	}
	_ = code
	c.exts = map[string]string{}
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		key := strings.ToUpper(parts[0])
		val := ""
		if len(parts) == 2 {
			val = parts[1]
		}
		c.exts[key] = val
	}
	return nil
}

// deliverToHost performs a full SMTP transaction with a single MX host.
func (m *Mailer) deliverToHost(host, from, to string, data []byte) (string, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(m.port()))

	ctx, cancel := context.WithTimeout(context.Background(), m.ConnectTimeout)
	defer cancel()

	dialer := &net.Dialer{Timeout: m.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", newTempError("connect to %s failed: %v", addr, err)
	}

	c := &smtpClient{conn: conn, txt: textproto.NewConn(conn), host: host}
	defer c.Close()

	// Read greeting (expect 220).
	if _, msg, err := c.txt.ReadResponse(220); err != nil {
		return "", classifyResponse(err, msg)
	}

	if err := c.hello(m.Hostname); err != nil {
		return "", err
	}

	// Opportunistic STARTTLS (RFC 3207): only when the server advertises it.
	if _, ok := c.exts["STARTTLS"]; ok {
		if _, _, err := c.cmd(220, "STARTTLS"); err != nil {
			return "", err
		}
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: true, // remote MX certificates often cannot be verified
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return "", newTempError("TLS handshake with %s failed: %v", host, err)
		}
		c.txt = textproto.NewConn(tlsConn)
		if err := c.hello(m.Hostname); err != nil {
			return "", err
		}
	}

	// MAIL FROM with BODY=8BITMIME when the message contains 8-bit bytes and
	// the remote server supports it.
	mailCmd := "MAIL FROM:<%s>"
	if is8Bit(data) {
		if _, ok := c.exts["8BITMIME"]; ok {
			mailCmd = "MAIL FROM:<%s> BODY=8BITMIME"
		} else {
			return "", newPermError("%s does not advertise 8BITMIME and the message contains 8-bit data", host)
		}
	}
	if _, _, err := c.cmd(250, mailCmd, from); err != nil {
		return "", err
	}
	if _, _, err := c.cmd(250, "RCPT TO:<%s>", to); err != nil {
		return "", err
	}
	if _, _, err := c.cmd(354, "DATA"); err != nil {
		return "", err
	}

	// Write the message body with dot-stuffing.
	dw := c.txt.DotWriter()
	if _, err := dw.Write(data); err != nil {
		_ = dw.Close()
		return "", newTempError("writing message data to %s failed: %v", host, err)
	}
	if err := dw.Close(); err != nil {
		return "", newTempError("finalizing message data to %s failed: %v", host, err)
	}

	code, msg, err := c.txt.ReadResponse(250)
	if err != nil {
		return "", classifyResponse(err, msg)
	}

	// Best-effort QUIT.
	_ = c.txt.PrintfLine("QUIT")
	_, _, _ = c.txt.ReadResponse(221)

	return fmt.Sprintf("%d %s", code, msg), nil
}

// is8Bit reports whether the data contains any byte >= 0x80.
func is8Bit(data []byte) bool {
	for _, b := range data {
		if b >= 0x80 {
			return true
		}
	}
	return false
}

// lookupMX resolves the MX hosts for a domain, sorted by preference.
// Per RFC 5321 section 5.1, when no MX record exists the domain itself is
// used as an implicit MX with preference 0.
func lookupMX(domain string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mxs, err := net.DefaultResolver.LookupMX(ctx, domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, newPermError("domain does not exist: %s", domain)
		}
		return nil, err
	}

	if len(mxs) == 0 {
		// Implicit MX: fall back to the domain's A/AAAA records.
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, domain)
		if err != nil {
			return nil, err
		}
		hosts := make([]string, 0, len(ips))
		for _, ip := range ips {
			hosts = append(hosts, ip.String())
		}
		if len(hosts) == 0 {
			return nil, fmt.Errorf("no MX or A records for %s", domain)
		}
		return hosts, nil
	}

	sort.Slice(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	hosts := make([]string, 0, len(mxs))
	for _, mx := range mxs {
		h := strings.TrimSuffix(mx.Host, ".")
		if h != "" {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("no usable MX hosts for %s", domain)
	}
	return hosts, nil
}
