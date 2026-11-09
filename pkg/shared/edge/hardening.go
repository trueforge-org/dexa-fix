package edge

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// maxFormBody caps the body of every non-API request. Forms are tiny; the JSON API sets its own,
// larger limit.
const maxFormBody = 1 << 20

// contentSecurityPolicy only allows our own scripts. Styles may be inline because a few templates
// size inputs with a style attribute and htmx injects its indicator rules.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// SecureHeaders adds the browser hardening headers to every response and keeps pages that can
// hold personal data (or a freshly minted token) out of shared and back/forward caches.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if IsSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// LimitBody rejects oversized form posts before any handler parses them, which would otherwise
// buffer up to 32 MiB of multipart data in memory and temp files for anonymous clients.
func LimitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.Body != nil && r.Body != http.NoBody {
			if r.ContentLength > maxFormBody {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP is the address of the peer, after proxy headers were settled by Proxy.Headers.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// IPKey identifies the client for rate limiting. An IPv6 client is counted by its /64, because a
// single subscriber usually controls the whole prefix and could otherwise rotate addresses freely.
func IPKey(r *http.Request) string {
	ip := ClientIP(r)
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() && !addr.Is4In6() {
		if p, err := addr.Prefix(64); err == nil {
			return "ip:" + p.String()
		}
	}
	return "ip:" + ip
}

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func compressible(contentType string) bool {
	for _, p := range []string{"text/", "application/json", "application/javascript", "image/svg+xml"} {
		if strings.HasPrefix(contentType, p) {
			return true
		}
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool
}

func (g *gzipWriter) WriteHeader(code int) {
	if g.started {
		return
	}
	g.started = true
	h := g.Header()
	if code != http.StatusNoContent && code != http.StatusNotModified && h.Get("Content-Encoding") == "" && compressible(h.Get("Content-Type")) {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.started {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gz != nil {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	if g.gz != nil {
		g.gz.Close()
		g.gz.Reset(io.Discard)
		gzipPool.Put(g.gz)
	}
}

// Compress gzips text responses for clients that accept it.
func Compress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}

// LogRequests logs every request at debug level.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug().Str("method", r.Method).Str("path", r.URL.Path).Dur("took", time.Since(start)).Msg("request")
	})
}
