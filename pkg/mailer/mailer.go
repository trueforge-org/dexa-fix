// Package mailer sends plain-text e-mail through an SMTP server. It exists for password reset
// links and nothing else, so it stays small: one message, one recipient, text only.
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Sender sends one message to one recipient.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Transport security modes.
const (
	StartTLS = "starttls" // plain connection upgraded with STARTTLS, usually port 587
	TLS      = "tls"      // TLS from the first byte, usually port 465
	None     = "none"     // no encryption: only accepted for a server on this machine
)

const (
	dialTimeout = 10 * time.Second
	sendTimeout = 30 * time.Second
)

// Config describes the SMTP server and the sender address.
type Config struct {
	Host     string
	Port     int
	Username string // empty: no authentication
	Password string
	From     string // address, optionally with a display name
	TLS      string // StartTLS (default), TLS or None
}

// SMTP sends through an SMTP server.
type SMTP struct {
	cfg  Config
	from *mail.Address
}

// New checks the configuration. It does not connect.
func New(cfg Config) (*SMTP, error) {
	if cfg.Host == "" {
		return nil, errors.New("smtp host is empty")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.TLS == "" {
		cfg.TLS = StartTLS
	}
	switch cfg.TLS {
	case StartTLS, TLS:
	case None:
		if !isLoopback(cfg.Host) {
			return nil, errors.New("smtp tls mode \"none\" is only allowed for a server on this machine (localhost): it would send the password and the reset links unencrypted")
		}
	default:
		return nil, fmt.Errorf("unknown smtp tls mode %q: use starttls, tls or none", cfg.TLS)
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("smtp from address: %w", err)
	}
	return &SMTP{cfg: cfg, from: from}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Send delivers the message. The recipient must be a plain address (no name, no line breaks).
func (m *SMTP) Send(ctx context.Context, to, subject, body string) error {
	rcpt, err := mail.ParseAddress(to)
	if err != nil || rcpt.Address != to || strings.ContainsAny(to, "\r\n") {
		return fmt.Errorf("invalid recipient address")
	}
	msg, err := m.message(to, subject, body)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := &net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(sendTimeout))
	tlsConf := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}
	if m.cfg.TLS == TLS {
		conn = tls.Client(conn, tlsConf)
	}
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return fmt.Errorf("smtp greeting: %w", err)
	}
	defer c.Close()
	if m.cfg.TLS == StartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the smtp server does not offer STARTTLS; refusing to send without encryption")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if m.cfg.Username != "" {
		// PlainAuth itself refuses to send the password over an unencrypted, non-local connection.
		if err := c.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("smtp login: %w", err)
		}
	}
	if err := c.Mail(m.from.Address); err != nil {
		return fmt.Errorf("smtp sender: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("smtp recipient: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := io.WriteString(w, msg); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	return c.Quit()
}

// message builds the headers and the quoted-printable body. Every header value is either a
// constant, an address that mail.ParseAddress accepted, or encoded, so nothing a user typed can add
// a header line.
func (m *SMTP) message(to, subject, body string) (string, error) {
	if strings.ContainsAny(subject, "\r\n") {
		return "", errors.New("subject contains a line break")
	}
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	domain := "localhost"
	if _, d, ok := strings.Cut(m.from.Address, "@"); ok && d != "" {
		domain = d
	}
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", m.from.String())
	h("To", to)
	h("Subject", mime.QEncoding.Encode("utf-8", subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\r\n", "\n"))); err != nil {
		return "", err
	}
	if err := qp.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}
