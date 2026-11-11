// Package netguard keeps the backend's own outgoing connections (to an administrator's sign-in
// providers and mail server) away from addresses that a typo or a hostile setting must not be able to
// reach: the cloud metadata services and the other addresses on a link, and, unless an administrator
// allows it, the private networks the server sits in. It takes nothing but the standard library, so
// both sides of the process boundary may use it.
//
// The check runs when the connection is made, on the address the name resolved to, so a name that
// answers with a different address on each lookup (DNS rebinding) and a redirect to a private address
// get the same answer as a private address typed in.
package netguard

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// ErrBlocked is returned for an address the guard refuses. The text names no address: it may reach a
// person who must not learn what is behind a name.
var ErrBlocked = errors.New("refusing to connect to that address")

// Always refused, whatever the setting: the metadata services of the big clouds (169.254.169.254 is
// link-local and covered below; these two are not), and the whole "this network" block, which Linux
// routes to the machine itself.
var (
	alwaysBlocked = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud
		netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS, IPv6
	}
	// Refused unless private addresses are allowed: what IsPrivate does not cover (carrier-grade NAT,
	// where some clouds put their own services).
	privateExtra = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")}
)

// Check says whether a connection to ip may be made. Loopback is allowed: the sign-in provider rule
// lets a development server on http://localhost through, and the mail client accepts an unencrypted
// server on this machine, so both are already deliberate.
func Check(ip netip.Addr, allowPrivate bool) error {
	ip = ip.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1
	if ip.IsLoopback() {
		return nil
	}
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return ErrBlocked
	}
	for _, p := range alwaysBlocked {
		if p.Contains(ip) {
			return ErrBlocked
		}
	}
	if !allowPrivate {
		if ip.IsPrivate() {
			return ErrBlocked
		}
		for _, p := range privateExtra {
			if p.Contains(ip) {
				return ErrBlocked
			}
		}
	}
	return nil
}

// Control is a net.Dialer's Control function. allowPrivate is asked at each connection, so a
// setting changed on the Admin page applies to the next one.
func Control(allowPrivate func() bool) func(network, address string, c syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return ErrBlocked
		}
		ip, err := netip.ParseAddr(host)
		if err != nil { // the dialer hands over an address, never a name
			return ErrBlocked
		}
		return Check(ip, allowPrivate())
	}
}
