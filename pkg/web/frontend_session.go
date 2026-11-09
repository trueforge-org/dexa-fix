package web

import (
	"context"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// withVisitor attaches the visitor's request to the context, so calls to the backend carry the
// visitor's cookies, address and host, and the backend's Set-Cookie headers reach the browser.
func (s *Frontend) withVisitor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := apiclient.WithRequest(r.Context(), apiclient.Request{
			Cookie:   r.Header.Get("Cookie"),
			ClientIP: clientIP(r),
			Host:     r.Host,
			Secure:   isSecure(r),
			Relay:    w,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// frontAuth finds the signed-in user for pages by asking the backend. The public API (/api/) is the
// backend's own, with its own credentials.
func (s *Frontend) frontAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r) // not ours: the API handler or the api process authenticates it
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookie); err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		u, err := s.api.Session(r.Context())
		if err != nil {
			s.backendDown(w, r, err)
			return
		}
		if u != nil {
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		}
		next.ServeHTTP(w, r)
	})
}

// backendDown answers a page request when the backend cannot be used.
func (s *Frontend) backendDown(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the visitor went away
	}
	log.Error().Err(err).Msg("backend request failed")
	w.Header().Set("Retry-After", "5")
	http.Error(w, "dexa-fix is temporarily unavailable, try again in a moment", http.StatusServiceUnavailable)
}
