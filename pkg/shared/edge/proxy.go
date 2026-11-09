// Package edge holds the HTTP edge code shared by the frontend and the backend process: working
// out who the visitor is behind a reverse proxy, the PROXY protocol, and request limiting. It must
// not import the store, so the frontend can use it without database access.
package edge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// Running behind a reverse proxy.
//
// A proxy terminates TLS and forwards plain HTTP from its own address, so the app would see the
// proxy's address instead of the visitor's, and plain HTTP instead of HTTPS. The proxy tells it
// what the visitor used in X-Forwarded-For, X-Forwarded-Proto and X-Forwarded-Host, but anyone can
// send those headers. They are therefore only believed on connections from the configured trusted
// proxies; from everyone else they are ignored.

type (
	secureKey  struct{}
	baseURLKey struct{}
)

// WithSecure records whether the visitor reached the app over HTTPS.
func WithSecure(ctx context.Context, secure bool) context.Context {
	return context.WithValue(ctx, secureKey{}, secure)
}

// WithBaseURL records the configured public address, which wins over the request's own host.
func WithBaseURL(ctx context.Context, base *url.URL) context.Context {
	return context.WithValue(ctx, baseURLKey{}, base)
}

// ParseTrustedProxies turns addresses, CIDR ranges and the keywords "loopback" and "private" into
// prefixes. "private" covers loopback, the RFC 1918 ranges and IPv6 unique local addresses.
func ParseTrustedProxies(items []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	add := func(cidrs ...string) {
		for _, c := range cidrs {
			out = append(out, netip.MustParsePrefix(c))
		}
	}
	for _, item := range items {
		for _, f := range strings.Split(item, ",") {
			f = strings.TrimSpace(f)
			switch {
			case f == "":
			case strings.EqualFold(f, "loopback"):
				add("127.0.0.0/8", "::1/128")
			case strings.EqualFold(f, "private"):
				add("127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")
			case strings.Contains(f, "/"):
				p, err := netip.ParsePrefix(f)
				if err != nil {
					return nil, fmt.Errorf("trusted proxy %q: %w", f, err)
				}
				out = append(out, p.Masked())
			default:
				a, err := netip.ParseAddr(f)
				if err != nil {
					return nil, fmt.Errorf("trusted proxy %q: not an address, a CIDR range, \"loopback\" or \"private\"", f)
				}
				a = a.Unmap()
				out = append(out, netip.PrefixFrom(a, a.BitLen()))
			}
		}
	}
	return out, nil
}

// ParseBaseURL checks the public address of the app, such as https://dexa.example.com. It must be
// an origin: the app serves from the root of its host and cannot live under a sub-path.
func ParseBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("base URL %q: use the form https://dexa.example.com", raw)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("base URL %q: serving under a sub-path is not supported, give only the scheme and host", raw)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// Settings returns the configured public address (nil if none) and the trusted proxies. It is
// asked on every request, so a change on the Admin page applies at once.
type Settings func() (base *url.URL, trusted []netip.Prefix)

// Proxy settles who the visitor is behind a reverse proxy. The zero value is ready to use.
type Proxy struct {
	warned atomic.Bool // the untrusted-forwarding warning was logged
}

// Trusted reports whether a is one of the trusted proxies.
func Trusted(trusted []netip.Prefix, a netip.Addr) bool { return contains(trusted, a) }

func contains(prefixes []netip.Prefix, a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Headers settles who the visitor is and how they reached us, once per request, so the rest of the
// app never reads the forwarding headers itself.
func (p *Proxy) Headers(settings Settings, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base, trusted := settings()
		ctx := r.Context()
		secure := r.TLS != nil
		if base != nil {
			ctx = WithBaseURL(ctx, base)
			secure = secure || base.Scheme == "https"
		}
		r2 := r.WithContext(ctx)

		peer, err := netip.ParseAddrPort(r.RemoteAddr)
		switch {
		case err == nil && contains(trusted, peer.Addr()):
			if ip, ok := forwardedClient(r, trusted); ok {
				r2.RemoteAddr = net.JoinHostPort(ip.String(), "0")
			}
			if strings.EqualFold(LastForwarded(r, "X-Forwarded-Proto"), "https") {
				secure = true
			}
			if host := LastForwarded(r, "X-Forwarded-Host"); ValidHost(host) {
				r2.Host = host
			}
		case r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Forwarded-Proto") != "" || r.Header.Get("X-Forwarded-Host") != "":
			if p.warned.CompareAndSwap(false, true) {
				logUntrustedForwarding(r.RemoteAddr)
			}
		}
		next.ServeHTTP(w, r2.WithContext(WithSecure(r2.Context(), secure)))
	})
}

// forwardedClient finds the visitor's address in X-Forwarded-For: walking from the right, past
// every trusted proxy, the first address nobody we trust vouches for is the visitor. Entries to
// the left of it could have been made up by the visitor and are not looked at.
func forwardedClient(r *http.Request, trusted []netip.Prefix) (netip.Addr, bool) {
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		chain = append(chain, strings.Split(v, ",")...)
	}
	if len(chain) == 0 {
		if a, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("X-Real-IP"))); err == nil {
			return a.Unmap(), true
		}
		return netip.Addr{}, false
	}
	for i := len(chain) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			return netip.Addr{}, false // garbage: keep the proxy's own address
		}
		a = a.Unmap()
		if !contains(trusted, a) || i == 0 {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// LastForwarded returns the last value of a forwarding header: the one the nearest proxy added.
func LastForwarded(r *http.Request, name string) string {
	var last string
	for _, v := range r.Header.Values(name) {
		parts := strings.Split(v, ",")
		last = strings.TrimSpace(parts[len(parts)-1])
	}
	return last
}

// ValidHost reports whether h is plausible as a Host header value.
func ValidHost(h string) bool {
	return h != "" && len(h) <= 255 && !strings.ContainsAny(h, " /\\@?#\t\r\n")
}

// IsSecure reports whether the visitor reached the app over HTTPS: directly, through a trusted
// proxy that said so, or because the configured base URL is https.
func IsSecure(r *http.Request) bool {
	v, _ := r.Context().Value(secureKey{}).(bool)
	return v || r.TLS != nil
}

// ExternalURL builds an absolute URL for this server as seen by the browser. The configured base
// URL wins; without one the request's own host is used, which a proxy must pass on unchanged.
func ExternalURL(r *http.Request, path string) string {
	if base, ok := r.Context().Value(baseURLKey{}).(*url.URL); ok {
		return base.Scheme + "://" + base.Host + path
	}
	scheme := "http"
	if IsSecure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + path
}

func logUntrustedForwarding(peer string) {
	log.Warn().Str("peer", peer).Msg("ignoring X-Forwarded-* headers from a connection that is not a trusted proxy; behind a reverse proxy, add the proxy's address to the trusted proxies on the Admin page")
}
