package mailer

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSMTP is a small SMTP server on this machine. It speaks just enough of the protocol for one
// message (EHLO, AUTH PLAIN, MAIL, RCPT, DATA, RSET, NOOP, QUIT) and records what it was given. It
// never offers STARTTLS.
type fakeSMTP struct {
	ln   net.Listener
	auth bool // advertise and accept AUTH PLAIN

	conns atomic.Int32

	mu     sync.Mutex
	msgs   []fakeMsg
	logins [][2]string // username and password of each AUTH PLAIN
}

type fakeMsg struct {
	from  string
	rcpts []string
	data  string // the message as the server received it, dots unstuffed, CRLF line endings
}

func newFakeSMTP(t *testing.T, auth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, auth: auth}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.conns.Add(1)
		go f.handle(conn)
	}
}

// addrIn turns "<a@b.c>" (the argument of MAIL FROM or RCPT TO) into a@b.c.
func addrIn(arg string) string {
	return strings.Trim(strings.TrimSpace(arg), "<>")
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	reply := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
	line := func() (string, bool) {
		s, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(s, "\r\n"), true
	}

	reply("220 fake.test ESMTP ready")
	var cur fakeMsg
	for {
		cmd, ok := line()
		if !ok {
			return
		}
		verb := strings.ToUpper(cmd)
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			ext := "250-fake.test\r\n"
			if f.auth {
				ext += "250-AUTH PLAIN\r\n"
			}
			_, _ = io.WriteString(conn, ext+"250 OK\r\n")
		case strings.HasPrefix(verb, "HELO"):
			reply("250 fake.test")
		case strings.HasPrefix(verb, "AUTH PLAIN"):
			if !f.auth {
				reply("504 5.5.4 authentication is not enabled")
				continue
			}
			arg := strings.TrimSpace(cmd[len("AUTH PLAIN"):])
			raw, err := base64.StdEncoding.DecodeString(arg)
			parts := strings.Split(string(raw), "\x00") // authzid, username, password
			if arg == "" || err != nil || len(parts) != 3 {
				reply("501 5.7.0 bad credentials")
				continue
			}
			f.mu.Lock()
			f.logins = append(f.logins, [2]string{parts[1], parts[2]})
			f.mu.Unlock()
			reply("235 2.7.0 authentication succeeded")
		case strings.HasPrefix(verb, "MAIL FROM:"):
			cur = fakeMsg{from: addrIn(cmd[len("MAIL FROM:"):])}
			reply("250 2.1.0 sender ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			cur.rcpts = append(cur.rcpts, addrIn(cmd[len("RCPT TO:"):]))
			reply("250 2.1.5 recipient ok")
		case verb == "DATA":
			reply("354 end with <CRLF>.<CRLF>")
			var lines []string
			for {
				l, ok := line()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				lines = append(lines, strings.TrimPrefix(l, ".")) // undo dot-stuffing
			}
			cur.data = strings.Join(lines, "\r\n") + "\r\n"
			f.mu.Lock()
			f.msgs = append(f.msgs, cur)
			f.mu.Unlock()
			cur = fakeMsg{}
			reply("250 2.0.0 queued")
		case verb == "RSET":
			cur = fakeMsg{}
			reply("250 2.0.0 reset")
		case verb == "NOOP":
			reply("250 2.0.0 ok")
		case verb == "QUIT":
			reply("221 2.0.0 bye")
			return
		default:
			reply("502 5.5.2 command not implemented")
		}
	}
}

func (f *fakeSMTP) messages() []fakeMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeMsg(nil), f.msgs...)
}

func (f *fakeSMTP) logged() [][2]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][2]string(nil), f.logins...)
}

// newLocalSender returns a sender that talks to f without encryption, which is allowed on this machine.
func newLocalSender(t *testing.T, f *fakeSMTP, cfg Config) *SMTP {
	t.Helper()
	cfg.Host = "127.0.0.1"
	cfg.Port = f.port()
	if cfg.TLS == "" {
		cfg.TLS = None
	}
	if cfg.From == "" {
		cfg.From = "Dexa <dexa@example.com>"
	}
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNewValidatesConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		cfg     Config
		wantErr string // a part of the error message; "" means accepted
	}{
		{"empty host", Config{From: "dexa@example.com"}, "host is empty"},
		{"bad from address", Config{Host: "smtp.example.com", From: "not an address"}, "from address"},
		{"empty from address", Config{Host: "smtp.example.com"}, "from address"},
		{"tls none to a remote host", Config{Host: "smtp.example.com", From: "dexa@example.com", TLS: None}, "only allowed"},
		{"tls none to localhost", Config{Host: "localhost", From: "dexa@example.com", TLS: None}, ""},
		{"tls none to 127.0.0.1", Config{Host: "127.0.0.1", From: "dexa@example.com", TLS: None}, ""},
		{"tls none to ::1", Config{Host: "::1", From: "dexa@example.com", TLS: None}, ""},
		{"unknown tls mode", Config{Host: "smtp.example.com", From: "dexa@example.com", TLS: "plain"}, "unknown smtp tls mode"},
		{"tls mode tls", Config{Host: "smtp.example.com", From: "dexa@example.com", TLS: TLS}, ""},
		{"display name in from", Config{Host: "smtp.example.com", From: "Dexa <dexa@example.com>"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("accepted, want an error with %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	m, err := New(Config{Host: "smtp.example.com", From: "dexa@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if m.cfg.Port != 587 {
		t.Errorf("default port %d, want 587", m.cfg.Port)
	}
	if m.cfg.TLS != StartTLS {
		t.Errorf("default tls mode %q, want %q", m.cfg.TLS, StartTLS)
	}
}

func TestSendDelivers(t *testing.T) {
	f := newFakeSMTP(t, false)
	m := newLocalSender(t, f, Config{})

	const subject = "Passwort zurücksetzen – dexa-fix"
	const text = "Hallo Zoë,\n\nbitte zahle 5 € bis Montag. Der Kaffee im café ist gut.\n.Ein Punkt am Zeilenanfang\n\nBis bald.\n"
	if err := m.Send(context.Background(), "alice@example.com", subject, text); err != nil {
		t.Fatal(err)
	}

	msgs := f.messages()
	if len(msgs) != 1 {
		t.Fatalf("%d messages received, want 1", len(msgs))
	}
	got := msgs[0]
	if got.from != "dexa@example.com" || len(got.rcpts) != 1 || got.rcpts[0] != "alice@example.com" {
		t.Fatalf("envelope: from %q, to %v", got.from, got.rcpts)
	}

	parsed, err := mail.ReadMessage(strings.NewReader(got.data))
	if err != nil {
		t.Fatalf("unparsable message: %v\n%s", err, got.data)
	}
	h := parsed.Header
	if from, err := mail.ParseAddress(h.Get("From")); err != nil || from.Address != "dexa@example.com" || from.Name != "Dexa" {
		t.Errorf("From: %q (%v)", h.Get("From"), err)
	}
	if h.Get("To") != "alice@example.com" {
		t.Errorf("To: %q", h.Get("To"))
	}
	if raw := h.Get("Subject"); !strings.HasPrefix(raw, "=?utf-8?q?") {
		t.Errorf("Subject not encoded: %q", raw)
	} else if dec, err := new(mime.WordDecoder).DecodeHeader(raw); err != nil || dec != subject {
		t.Errorf("Subject decodes to %q (%v), want %q", dec, err, subject)
	}
	if _, err := time.Parse(time.RFC1123Z, h.Get("Date")); err != nil {
		t.Errorf("Date: %q (%v)", h.Get("Date"), err)
	}
	if !regexp.MustCompile(`^<[0-9a-f]{24}@example\.com>$`).MatchString(h.Get("Message-ID")) {
		t.Errorf("Message-ID: %q", h.Get("Message-ID"))
	}
	if h.Get("MIME-Version") != "1.0" {
		t.Errorf("MIME-Version: %q", h.Get("MIME-Version"))
	}
	if h.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type: %q", h.Get("Content-Type"))
	}
	if h.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Errorf("Content-Transfer-Encoding: %q", h.Get("Content-Transfer-Encoding"))
	}

	body, err := io.ReadAll(quotedprintable.NewReader(parsed.Body))
	if err != nil {
		t.Fatal(err)
	}
	if decoded := strings.ReplaceAll(string(body), "\r\n", "\n"); decoded != text {
		t.Errorf("body decodes to\n%q\nwant\n%q", decoded, text)
	}
}

func TestSendRefusesLineBreaksThatWouldAddHeaders(t *testing.T) {
	f := newFakeSMTP(t, false)
	m := newLocalSender(t, f, Config{})
	ctx := context.Background()

	if err := m.Send(ctx, "alice@example.com", "Hi\r\nBcc: x@y.z", "body"); err == nil {
		t.Fatal("subject with a line break accepted")
	}
	if n := len(f.messages()); n != 0 {
		t.Fatalf("%d messages sent for a refused subject", n)
	}

	for _, to := range []string{
		"a@b.c\r\nBcc: x@y.z",
		"Dana <a@b.c>",
		"a@b.c, x@y.z",
	} {
		if err := m.Send(ctx, to, "Subject", "body"); err == nil {
			t.Errorf("recipient %q accepted", to)
		}
	}
	if n := f.conns.Load(); n != 0 {
		t.Fatalf("%d connections made for refused recipients, want none", n)
	}
	if n := len(f.messages()); n != 0 {
		t.Fatalf("%d messages sent for refused recipients", n)
	}
}

func TestSendStartTLSNeedsTheServerToOfferIt(t *testing.T) {
	f := newFakeSMTP(t, false) // does not offer STARTTLS
	m, err := New(Config{Host: "127.0.0.1", Port: f.port(), From: "dexa@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(context.Background(), "alice@example.com", "Subject", "body")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("error %v, want one that mentions STARTTLS", err)
	}
	if n := len(f.messages()); n != 0 {
		t.Fatalf("%d messages sent without encryption", n)
	}
}

func TestSendLogsInWithPlainAuth(t *testing.T) {
	f := newFakeSMTP(t, true)
	m := newLocalSender(t, f, Config{Username: "reset-bot", Password: "s3cret-Pass"})
	if err := m.Send(context.Background(), "alice@example.com", "Subject", "body"); err != nil {
		t.Fatal(err)
	}
	if got := f.logged(); len(got) != 1 || got[0] != [2]string{"reset-bot", "s3cret-Pass"} {
		t.Fatalf("credentials received: %v", got)
	}
	if n := len(f.messages()); n != 1 {
		t.Fatalf("%d messages after login, want 1", n)
	}
}

func TestSendWithoutUsernameDoesNotLogIn(t *testing.T) {
	f := newFakeSMTP(t, true)
	m := newLocalSender(t, f, Config{Password: "ignored"})
	if err := m.Send(context.Background(), "alice@example.com", "Subject", "body"); err != nil {
		t.Fatal(err)
	}
	if got := f.logged(); len(got) != 0 {
		t.Fatalf("login attempted without a username: %v", got)
	}
	if n := len(f.messages()); n != 1 {
		t.Fatalf("%d messages, want 1", n)
	}
}
