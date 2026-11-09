package backend

import (
	"net/http"
	"net/netip"
	"net/url"

	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
)

// Running behind a reverse proxy: the logic lives in pkg/shared/edge, which the frontend and the backend
// share. These are thin wrappers over the current settings.

// ParseTrustedProxies turns addresses, CIDR ranges and the keywords "loopback" and "private" into
// prefixes. See edge.ParseTrustedProxies.
func ParseTrustedProxies(items []string) ([]netip.Prefix, error) {
	return edge.ParseTrustedProxies(items)
}

// ParseBaseURL checks the public address of the app. See edge.ParseBaseURL.
func ParseBaseURL(raw string) (*url.URL, error) { return edge.ParseBaseURL(raw) }

func isSecure(r *http.Request) bool { return edge.IsSecure(r) }

func externalURL(r *http.Request, path string) string { return edge.ExternalURL(r, path) }
