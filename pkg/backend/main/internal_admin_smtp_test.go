package mainbackend

import (
	"bufio"
	"context"
	"io"
	"mime/quotedprintable"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// tinySMTP is a mail server on this machine that takes a message and keeps it, or refuses every
// recipient when told to. It never offers STARTTLS or AUTH.
type tinySMTP struct {
	ln     net.Listener
	refuse bool
	mu     sync.Mutex
	mails  []string // each message as received: "rcpt\n" + data
}

func newTinySMTP(t *testing.T, refuse bool) *tinySMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &tinySMTP{ln: ln, refuse: refuse}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(conn)
		}
	}()
	return s
}

func (s *tinySMTP) port() string { return strconv.Itoa(s.ln.Addr().(*net.TCPAddr).Port) }

func (s *tinySMTP) received() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mails...)
}

func (s *tinySMTP) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(line string) { io.WriteString(conn, line+"\r\n") }
	say("220 tiny ESMTP")
	rcpt := ""
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.TrimSpace(l))
		switch {
		case strings.HasPrefix(verb, "EHLO"):
			say("250 tiny")
		case strings.HasPrefix(verb, "MAIL FROM:"):
			say("250 ok")
		case strings.HasPrefix(verb, "RCPT TO:"):
			if s.refuse {
				say("550 no such mailbox")
				continue
			}
			rcpt = strings.Trim(strings.TrimSpace(l[len("RCPT TO:"):]), "<>")
			say("250 ok")
		case verb == "DATA":
			say("354 go on")
			var data []string
			for {
				d, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(d, "\r\n") == "." {
					break
				}
				data = append(data, d)
			}
			s.mu.Lock()
			s.mails = append(s.mails, rcpt+"\n"+strings.Join(data, ""))
			s.mu.Unlock()
			say("250 queued")
		case verb == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

// useMailServer saves the e-mail settings through the Admin page, as an administrator does.
func (e *adminEnv) useMailServer(t *testing.T, s *tinySMTP) {
	t.Helper()
	code, out := e.do(t, "POST", "/admin/smtp", url.Values{"host": {"127.0.0.1"}, "port": {s.port()}, "tls": {"none"}, "from": {"DexaFix <noreply@example.com>"}})
	if code != 303 || !strings.Contains(adminFlash(t, out), "E-mail settings saved") {
		t.Fatalf("saving the e-mail settings: %d %q", code, out)
	}
}

func TestAdminSendsATestEmailThroughTheMailServer(t *testing.T) {
	t.Run("with a working server the test mail arrives", func(t *testing.T) {
		e := newAdminEnv(t)
		mail := newTinySMTP(t, false)
		e.useMailServer(t, mail)
		code, out := e.do(t, "POST", "/admin/smtp/test", url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "A test e-mail was sent to root@example.com." {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
		got := mail.received()
		if len(got) != 1 || !strings.HasPrefix(got[0], "root@example.com\n") || !strings.Contains(got[0], "E-mail is working.") {
			t.Fatalf("the server received %q", got)
		}
	})
	t.Run("a server that refuses the recipient is reported", func(t *testing.T) {
		e := newAdminEnv(t)
		e.useMailServer(t, newTinySMTP(t, true))
		code, out := e.do(t, "POST", "/admin/smtp/test", url.Values{})
		if got := adminFlash(t, out); code != 303 || !strings.HasPrefix(got, "Could not send the test e-mail: ") {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
	})
	t.Run("without e-mail settings the administrator is told to set them up", func(t *testing.T) {
		e := newAdminEnv(t)
		code, out := e.do(t, "POST", "/admin/smtp/test", url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "Set up and save the e-mail settings first." {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
	})
	t.Run("an account without an address has nowhere to send the test", func(t *testing.T) {
		e := newAdminEnv(t)
		e.useMailServer(t, newTinySMTP(t, false))
		if err := e.st.ExecForTests(context.Background(), "UPDATE users SET email = '' WHERE id = 1"); err != nil {
			t.Fatal(err)
		}
		code, out := e.do(t, "POST", "/admin/smtp/test", url.Values{})
		if got := adminFlash(t, out); code != 303 || got != "Your account has no e-mail address to send the test to." {
			t.Fatalf("test e-mail: %d %q", code, got)
		}
	})
}

func TestAdminMailsAResetLinkWhenTheServerKnowsItsAddress(t *testing.T) {
	e := newAdminEnv(t)
	mail := newTinySMTP(t, false)
	e.useMailServer(t, mail)
	if code, out := e.do(t, "POST", "/admin/server", url.Values{"base_url": {e.ts.URL}}); code != 303 || !strings.Contains(adminFlash(t, out), "apply now") {
		t.Fatalf("saving the base URL: %d %q", code, out)
	}
	code, page := e.do(t, "POST", "/admin/users/2/reset-link", url.Values{"email": {"1"}})
	if code != 200 || !strings.Contains(page, "bob") {
		t.Fatalf("reset link page: %d", code)
	}
	got := mail.received()
	if len(got) != 1 || !strings.HasPrefix(got[0], "bob@example.com\n") {
		t.Fatalf("the reset e-mail: %q", got)
	}
	_, encoded, _ := strings.Cut(got[0], "\r\n\r\n")
	text, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(encoded)))
	if err != nil || !strings.Contains(string(text), e.ts.URL+"/password/reset?t=") {
		t.Fatalf("the reset e-mail does not carry the link: %v %q", err, text)
	}
}
