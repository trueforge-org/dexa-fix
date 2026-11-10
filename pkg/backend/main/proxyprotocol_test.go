package mainbackend

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/frontend"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// setProxyPolicy puts the PROXY protocol setting and the trusted peers in force in the combined
// server: in the backend, which passes them on to the frontend.
func setProxyPolicy(t *testing.T, s *Server, enabled bool, trusted string) {
	t.Helper()
	p, err := ParseTrustedProxies([]string{trusted})
	if err != nil {
		t.Fatal(err)
	}
	l := *s.cfg()
	l.proxyProtocol = enabled
	l.trusted = p
	s.swapSettings(&l)
}

func TestProxyProtocolReachesTheAuditLog(t *testing.T) {
	skipInSplit(t)
	ts, _, st := newAuthServer(t)
	srv := serverOf(ts)
	setProxyPolicy(t, srv, true, "127.0.0.1/32")
	ln := listenWrapped(t, srv.Frontend)
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { hs.Close() })
	addr := ln.Addr().String()

	if code, page := rawHTTP(t, addr, "", "GET", "/healthz", ""); code != 200 || page != "ok" {
		t.Errorf("healthz without a header: %d %q", code, page)
	}
	if code, page := rawHTTP(t, addr, v1Line, "GET", "/healthz", ""); code != 200 || page != "ok" {
		t.Errorf("healthz with a header from a trusted peer: %d %q", code, page)
	}

	// a failed sign-in through a connection that claims to come from 203.0.113.7
	if code, _ := rawHTTP(t, addr, v1Line, "POST", "/login", "email=root%40example.com&password=wrong-password"); code != http.StatusUnauthorized {
		t.Fatalf("failed sign-in: %d, want 401", code)
	}
	events, err := st.ListAuditEvents(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Action == auditLoginFailed {
			found = true
			if e.IP != "203.0.113.7" {
				t.Errorf("failed sign-in recorded from %q, want the address in the PROXY header", e.IP)
			}
		}
	}
	if !found {
		t.Errorf("no %s event in %+v", auditLoginFailed, events)
	}
}

// rawHTTP sends one HTTP/1.1 request on a new connection, after an optional PROXY header, and
// returns the status and body. An empty form sends no body.
func rawHTTP(t *testing.T, addr, prefix, method, path, form string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	req := method + " " + path + " HTTP/1.1\r\nHost: dexa.test\r\nConnection: close\r\n"
	if form != "" {
		req += "Content-Type: application/x-www-form-urlencoded\r\nContent-Length: " + strconv.Itoa(len(form)) + "\r\n"
	}
	req += "\r\n" + form
	if _, err := io.WriteString(conn, prefix+req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// listenWrapped listens on a local port through the frontend's listener wrapper.
func listenWrapped(t *testing.T, s *frontend.Frontend) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := s.WrapListener(ln)
	t.Cleanup(func() { wrapped.Close() })
	return wrapped
}

// v1Line is a PROXY protocol v1 header for a client at 203.0.113.7.
const v1Line = "PROXY TCP4 203.0.113.7 10.0.0.1 51234 8080\r\n"
