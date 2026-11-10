package frontend

import (
	"net"
	"net/netip"

	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

// PROXY protocol: the policy lives in pkg/shared/edge. See edge.ProxyListener.

// WrapListener makes ln understand the PROXY protocol according to the current settings. Wrap the
// listener once at start; it costs nothing while the setting is off.
//
// It also remembers the address, which the HTTPS and HTTP/3 servers (tlsserve.go) take their
// interface from.
func (s *Frontend) WrapListener(ln net.Listener) net.Listener {
	s.listenAddr = ln.Addr().String()
	return s.proxyListener(ln)
}

// proxyListener applies the PROXY protocol policy to a listener. The HTTPS listener uses it too:
// the header comes before the TLS handshake.
func (s *Frontend) proxyListener(ln net.Listener) net.Listener {
	return edge.ProxyListener(ln, s.proxyProtocolPolicy)
}

// proxyProtocolPolicy is read by the listener wrapper.
func (s *Frontend) proxyProtocolPolicy() (enabled bool, trusted []netip.Prefix) {
	l := s.cfg()
	return l.proxyProtocol, l.trusted
}
