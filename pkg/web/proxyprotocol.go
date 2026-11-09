package web

import (
	"net"

	"github.com/trueforge-org/dexa-fix/pkg/edge"
)

// PROXY protocol: the policy lives in pkg/edge. See edge.ProxyListener.

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
	return edge.ProxyListener(ln, s.proxyProtocolPolicy)
}
