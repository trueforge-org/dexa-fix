package mailer

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedSMTP is an SMTP server that answers every command well, except where the test says
// otherwise. The keys of reply are GREET, EHLO, AUTH, STARTTLS, MAIL, RCPT, DATA (the answer to the
// DATA command), END (the answer after the message), QUIT. CUT (any value) hangs up right after the
// answer to DATA. The value is the reply line (several lines
// separated by \n) or "CLOSE" to drop the connection instead of answering.
type scriptedSMTP struct {
	ln    net.Listener
	reply map[string]string
	mu    sync.Mutex
	seen  []string // verbs received, in order
}

const closeConn = "CLOSE"

func newScriptedSMTP(t *testing.T, reply map[string]string) *scriptedSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptedSMTP{ln: ln, reply: reply}
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

func (s *scriptedSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *scriptedSMTP) verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// say sends the scripted or default answer for key; it reports false when the script says to drop.
func (s *scriptedSMTP) say(conn net.Conn, key, def string) bool {
	a, ok := s.reply[key]
	if !ok {
		a = def
	}
	if a == closeConn {
		return false
	}
	_, _ = conn.Write([]byte(strings.ReplaceAll(a, "\n", "\r\n") + "\r\n"))
	return true
}

func (s *scriptedSMTP) handle(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if !s.say(conn, "GREET", "220 scripted ESMTP") {
		return
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		verb := strings.ToUpper(strings.Fields(line + " ")[0])
		s.mu.Lock()
		s.seen = append(s.seen, verb)
		s.mu.Unlock()
		var ok bool
		switch verb {
		case "EHLO":
			ok = s.say(conn, "EHLO", "250-scripted\n250 AUTH PLAIN")
		case "STARTTLS":
			ok = s.say(conn, "STARTTLS", "220 go ahead")
		case "AUTH":
			ok = s.say(conn, "AUTH", "235 ok")
		case "MAIL":
			ok = s.say(conn, "MAIL", "250 ok")
		case "RCPT":
			ok = s.say(conn, "RCPT", "250 ok")
		case "DATA":
			if ok = s.say(conn, "DATA", "354 send it"); ok {
				if s.reply["CUT"] != "" { // hang up before reading the message
					return
				}
				for { // read to the lone dot
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
				}
				ok = s.say(conn, "END", "250 queued")
			}
		case "QUIT":
			s.say(conn, "QUIT", "221 bye")
			return
		default:
			ok = s.say(conn, "OTHER", "502 not implemented")
		}
		if !ok {
			return
		}
	}
}

// senderFor returns a sender for the scripted server in the given TLS mode.
func senderFor(t *testing.T, s *scriptedSMTP, cfg Config) *SMTP {
	t.Helper()
	cfg.Host, cfg.Port, cfg.From = "127.0.0.1", s.port(), "Dexa <dexa@example.com>"
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSendSaysWhichStepTheSMTPServerBrokeAt(t *testing.T) {
	big := strings.Repeat("a long line of text to fill the buffer\n", 20000)
	for _, tc := range []struct {
		name  string
		reply map[string]string
		cfg   Config
		body  string
		want  string // part of the error
	}{
		{"the server hangs up at once", map[string]string{"GREET": closeConn}, Config{TLS: None}, "b", "smtp greeting"},
		{"the greeting is a refusal", map[string]string{"GREET": "554 no service"}, Config{TLS: None}, "b", "smtp greeting"},
		{"the server does not speak TLS on a TLS port", nil, Config{TLS: TLS}, "b", "smtp greeting"},
		{"the server offers STARTTLS but cannot do it", map[string]string{"EHLO": "250-scripted\n250 STARTTLS", "STARTTLS": "454 TLS not available"}, Config{TLS: StartTLS}, "b", "starttls"},
		{"the server refuses the login", map[string]string{"AUTH": "535 bad credentials"}, Config{TLS: None, Username: "u", Password: "p"}, "b", "smtp login"},
		{"the server refuses the sender", map[string]string{"MAIL": "550 sender not allowed"}, Config{TLS: None}, "b", "smtp sender"},
		{"the server refuses the recipient", map[string]string{"RCPT": "550 no such user"}, Config{TLS: None}, "b", "smtp recipient"},
		{"the server refuses the message data command", map[string]string{"DATA": "554 no"}, Config{TLS: None}, "b", "smtp data"},
		{"the server drops the connection at the data command", map[string]string{"DATA": closeConn}, Config{TLS: None}, "b", "smtp data"},
		{"the server refuses the finished message", map[string]string{"END": "552 too big"}, Config{TLS: None}, "b", "smtp data"},
		{"the server hangs up before the message is read", map[string]string{"CUT": "1"}, Config{TLS: None}, "b", "smtp data"},
		{"the server hangs up while a long message is sent", map[string]string{"CUT": "1"}, Config{TLS: None}, big, "smtp data"},
		{"the server hangs up at goodbye", map[string]string{"QUIT": closeConn}, Config{TLS: None}, "b", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScriptedSMTP(t, tc.reply)
			err := senderFor(t, s, tc.cfg).Send(context.Background(), "alice@example.com", "Subject", tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want one that mentions %q", err, tc.want)
			}
		})
	}
}

func TestSendToAServerThatIsNotThereNamesTheAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nobody listens any more
	m, err := New(Config{Host: "127.0.0.1", Port: port, From: "dexa@example.com", TLS: None})
	if err != nil {
		t.Fatal(err)
	}
	err = m.Send(context.Background(), "alice@example.com", "Subject", "body")
	if err == nil || !strings.Contains(err.Error(), "connect to 127.0.0.1:") {
		t.Fatalf("got %v", err)
	}
}
