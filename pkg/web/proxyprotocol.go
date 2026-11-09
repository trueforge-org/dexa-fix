package web

import (
	"net"
	"net/netip"
	"time"

	"github.com/pires/go-proxyproto"
)

// PROXY protocol (HAProxy, versions 1 and 2) lets a TCP load balancer pass the visitor's real
// address on to the app in a header at the start of each connection, for balancers that cannot or
// do not add X-Forwarded-For. It is an Admin setting, turned on together with the trusted proxies:
//
//   - a connection from a trusted proxy may start with the header, and the address in it becomes
//     the visitor's address (the header is optional there, so a balancer that does not send it
//     cannot lock anybody out of the Admin page);
//   - a connection from anyone else may not send one (it is refused), so nobody can claim an
//     address that is not theirs;
//   - with the setting off, connections are passed through untouched.
//
// The policy is asked for every new connection, so a change on the Admin page applies at once.
// The PROXY header carries the visitor's address but not whether they used HTTPS: set the base URL
// (https://...) so that cookies are marked Secure.

// proxyHeaderTimeout bounds how long a connection may take to send its header.
const proxyHeaderTimeout = 10 * time.Second

// WrapListener makes ln understand the PROXY protocol according to the current settings. Wrap the
// listener once at start; it costs nothing while the setting is off.
//
// It also remembers the address, which the HTTPS and HTTP/3 servers (tlsserve.go) take their
// interface from.
func (s *Server) WrapListener(ln net.Listener) net.Listener {
	s.listenAddr = ln.Addr().String()
	return s.proxyListener(ln)
}

// proxyListener applies the PROXY protocol policy to a listener. The HTTPS listener uses it too:
// the header comes before the TLS handshake.
func (s *Server) proxyListener(ln net.Listener) net.Listener {
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: proxyHeaderTimeout,
		ConnPolicy:        s.proxyPolicy,
	}
}

func (s *Server) proxyPolicy(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
	enabled, trusted := s.proxyProtocolPolicy()
	if !enabled {
		return proxyproto.SKIP, nil
	}
	if ap, err := netip.ParseAddrPort(o.Upstream.String()); err == nil {
		peer := ap.Addr().Unmap()
		for _, p := range trusted {
			if p.Contains(peer) {
				return proxyproto.USE, nil
			}
		}
	}
	return proxyproto.REJECT, nil
}
