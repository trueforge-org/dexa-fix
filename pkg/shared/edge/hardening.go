package edge

import (
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
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

// ---- failure rate limiting ----

const (
	failWindow = 15 * time.Minute

	maxLimiterKeys = 50000
)

// FailLimiter counts failed attempts per key in a sliding window. It only counts failures, so
// people who type the right password never notice it.
type FailLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time
}

func NewFailLimiter() *FailLimiter {
	return &FailLimiter{fails: map[string][]time.Time{}, now: time.Now}
}

// prune drops attempts older than the window and returns what is left. Callers hold the lock.
func (l *FailLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-failWindow)
	list := l.fails[key]
	i := 0
	for i < len(list) && list[i].Before(cutoff) {
		i++
	}
	list = list[i:]
	if len(list) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = list
	return list
}

// Blocked reports whether key already used up its attempts.
func (l *FailLimiter) Blocked(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= max
}

func (l *FailLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > maxLimiterKeys { // bound memory under a flood of distinct keys
		l.evict()
	}
	l.prune(key)
	l.fails[key] = append(l.fails[key], l.now())
}

// evict makes room by dropping the keys with the fewest recent failures (ties: the oldest), so
// the counters of keys that are close to being blocked survive a flood of one-off keys. Callers
// hold the lock.
func (l *FailLimiter) evict() {
	type entry struct {
		key  string
		n    int
		last time.Time
	}
	entries := make([]entry, 0, len(l.fails))
	for k := range l.fails {
		if list := l.prune(k); list != nil {
			entries = append(entries, entry{k, len(list), list[len(list)-1]})
		}
	}
	if len(entries) <= maxLimiterKeys {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].n != entries[j].n {
			return entries[i].n < entries[j].n
		}
		return entries[i].last.Before(entries[j].last)
	})
	for _, e := range entries[:len(entries)-maxLimiterKeys*4/5] {
		delete(l.fails, e.key)
	}
}

func (l *FailLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
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
