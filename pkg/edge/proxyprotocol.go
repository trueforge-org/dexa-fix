package edge

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

// ProxyListener applies the PROXY protocol policy to a listener. policy is asked for every new
// connection and reports whether the protocol is enabled and which peers may send the header.
// Wrap a listener once at start; it costs nothing while the setting is off. The HTTPS listener
// uses it too: the header comes before the TLS handshake.
func ProxyListener(ln net.Listener, policy func() (enabled bool, trusted []netip.Prefix)) net.Listener {
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: proxyHeaderTimeout,
		ConnPolicy: func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			enabled, trusted := policy()
			if !enabled {
				return proxyproto.SKIP, nil
			}
			if ap, err := netip.ParseAddrPort(o.Upstream.String()); err == nil {
				if contains(trusted, ap.Addr()) {
					return proxyproto.USE, nil
				}
			}
			return proxyproto.REJECT, nil
		},
	}
}
