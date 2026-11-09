package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"

	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// setProxyPolicy puts the PROXY protocol setting and the trusted peers in force on s.
func setProxyPolicy(t *testing.T, s *Server, enabled bool, trusted string) {
	t.Helper()
	p, err := ParseTrustedProxies([]string{trusted})
	if err != nil {
		t.Fatal(err)
	}
	l := liveSettings{}
	if cur := s.live.Load(); cur != nil {
		l = *cur
	}
	l.proxyProtocol = enabled
	l.trusted = p
	s.swapSettings(&l)
}

// proxyPolicyServer is a Server that only needs the PROXY protocol settings.
func proxyPolicyServer(t *testing.T, enabled bool, trusted string) *Server {
	t.Helper()
	s := &Server{}
	setProxyPolicy(t, s, enabled, trusted)
	return s
}

// listenWrapped listens on a local port through the server's listener wrapper.
func listenWrapped(t *testing.T, s *Server) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := s.WrapListener(ln)
	t.Cleanup(func() { wrapped.Close() })
	return wrapped
}

type acceptResult struct {
	conn net.Conn
	err  error
}

// acceptAsync accepts one connection in the background.
func acceptAsync(ln net.Listener) <-chan acceptResult {
	ch := make(chan acceptResult, 1)
	go func() {
		c, err := ln.Accept()
		ch <- acceptResult{c, err}
	}()
	return ch
}

// nextConn waits for the accepted connection.
func nextConn(t *testing.T, ch <-chan acceptResult) net.Conn {
	t.Helper()
	select {
	case a := <-ch:
		if a.err != nil {
			t.Fatal(a.err)
		}
		t.Cleanup(func() { a.conn.Close() })
		return a.conn
	case <-time.After(5 * time.Second):
		t.Fatal("no connection accepted")
	}
	return nil
}

// dialTo connects a client to the listener. The client closes when the test ends.
func dialTo(t *testing.T, ln net.Listener) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// addrHost returns the host part of an address.
func addrHost(t *testing.T, a net.Addr) string {
	t.Helper()
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		t.Fatal(err)
	}
	return host
}

// readExactly reads n bytes from the server side of a connection.
func readExactly(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

const v1Line = "PROXY TCP4 203.0.113.7 10.0.0.1 51234 8080\r\n"

func TestProxyProtocolV1FromTrustedPeerSetsTheAddress(t *testing.T) {
	ln := listenWrapped(t, proxyPolicyServer(t, true, "127.0.0.1/32"))
	ch := acceptAsync(ln)
	client := dialTo(t, ln)
	if _, err := io.WriteString(client, v1Line+"hello"); err != nil {
		t.Fatal(err)
	}

	conn := nextConn(t, ch)
	if host := addrHost(t, conn.RemoteAddr()); host != "203.0.113.7" {
		t.Errorf("RemoteAddr host %q, want the address from the header", host)
	}
	if got := readExactly(t, conn, 5); got != "hello" {
		t.Errorf("data after the header = %q", got)
	}
}

func TestProxyProtocolV2FromTrustedPeerSetsTheAddress(t *testing.T) {
	ln := listenWrapped(t, proxyPolicyServer(t, true, "127.0.0.1/32"))
	ch := acceptAsync(ln)
	client := dialTo(t, ln)
	h := proxyproto.Header{
		Version:           2,
		Command:           proxyproto.PROXY,
		TransportProtocol: proxyproto.TCPv4,
		SourceAddr:        &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 51234},
		DestinationAddr:   &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8080},
	}
	if _, err := h.WriteTo(client); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(client, "hello"); err != nil {
		t.Fatal(err)
	}

	conn := nextConn(t, ch)
	if host := addrHost(t, conn.RemoteAddr()); host != "203.0.113.7" {
		t.Errorf("RemoteAddr host %q, want the address from the v2 header", host)
	}
	if got := readExactly(t, conn, 5); got != "hello" {
		t.Errorf("data after the v2 header = %q", got)
	}
}

func TestProxyProtocolFromUntrustedPeerIsRefused(t *testing.T) {
	// the test client is 127.0.0.1, which is not among the trusted peers
	ln := listenWrapped(t, proxyPolicyServer(t, true, "10.0.0.0/8"))
	ch := acceptAsync(ln)
	client := dialTo(t, ln)
	if _, err := io.WriteString(client, v1Line+"hello"); err != nil {
		t.Fatal(err)
	}

	conn := nextConn(t, ch)
	if host := addrHost(t, conn.RemoteAddr()); host != "127.0.0.1" {
		t.Errorf("RemoteAddr host %q: the address in the header was taken from an untrusted peer", host)
	}
	buf := make([]byte, 16)
	if _, err := conn.Read(buf); !errors.Is(err, proxyproto.ErrSuperfluousProxyHeader) {
		t.Errorf("read after an untrusted PROXY header: %v, want ErrSuperfluousProxyHeader", err)
	}
}

func TestTrustedPeerMayOmitTheProxyHeader(t *testing.T) {
	ln := listenWrapped(t, proxyPolicyServer(t, true, "127.0.0.1/32"))
	ch := acceptAsync(ln)
	client := dialTo(t, ln)
	// more than the length of the header signature, so the header check ends at once
	const msg = "hello, no header here"
	if _, err := io.WriteString(client, msg); err != nil {
		t.Fatal(err)
	}

	conn := nextConn(t, ch)
	if host := addrHost(t, conn.RemoteAddr()); host != "127.0.0.1" {
		t.Errorf("RemoteAddr host %q, want the peer's own address", host)
	}
	if got := readExactly(t, conn, len(msg)); got != msg {
		t.Errorf("data = %q, want %q", got, msg)
	}
}

func TestProxyProtocolOffPassesBytesThrough(t *testing.T) {
	ln := listenWrapped(t, proxyPolicyServer(t, false, "127.0.0.1/32"))
	ch := acceptAsync(ln)
	client := dialTo(t, ln)
	line := v1Line + "hello"
	if _, err := io.WriteString(client, line); err != nil {
		t.Fatal(err)
	}

	conn := nextConn(t, ch)
	if host := addrHost(t, conn.RemoteAddr()); host != "127.0.0.1" {
		t.Errorf("RemoteAddr host %q with the setting off", host)
	}
	// the header is ordinary data now
	if got := readExactly(t, conn, len(line)); got != line {
		t.Errorf("data = %q, want the PROXY line passed through as data", got)
	}
}

func TestProxyProtocolReachesTheAuditLog(t *testing.T) {
	skipInSplit(t)
	ts, _, st := newAuthServer(t)
	srv := serverOf(ts)
	setProxyPolicy(t, srv, true, "127.0.0.1/32")
	ln := listenWrapped(t, srv)
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
