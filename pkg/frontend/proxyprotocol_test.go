package frontend

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

// proxyPolicyServer is a Frontend that only needs the PROXY protocol settings.
func proxyPolicyServer(t *testing.T, enabled bool, trusted string) *Frontend {
	t.Helper()
	p, err := edge.ParseTrustedProxies([]string{trusted})
	if err != nil {
		t.Fatal(err)
	}
	s := &Frontend{}
	s.swapSettings(&frontendSettings{proxyProtocol: enabled, trusted: p})
	return s
}

// listenWrapped listens on a local port through the server's listener wrapper.
func listenWrapped(t *testing.T, s *Frontend) net.Listener {
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
