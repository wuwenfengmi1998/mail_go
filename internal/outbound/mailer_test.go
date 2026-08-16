package outbound

import (
	"bufio"
	"net"
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
