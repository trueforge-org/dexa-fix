package web

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxFormBody caps the body of every non-API request. Forms are tiny; the JSON API sets its own,
// larger limit.
const maxFormBody = 1 << 20

// contentSecurityPolicy only allows our own scripts. Styles may be inline because a few templates
// size inputs with a style attribute and htmx injects its indicator rules.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// secureHeaders adds the browser hardening headers to every response and keeps pages that can
// hold personal data (or a freshly minted token) out of shared and back/forward caches.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if isSecure(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// limitBody rejects oversized form posts before any handler parses them, which would otherwise
// buffer up to 32 MiB of multipart data in memory and temp files for anonymous clients.
func limitBody(next http.Handler) http.Handler {
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
	failWindow   = 15 * time.Minute
	maxLoginFail = 10 // per login name
	maxIPFail    = 60 // per client address: generous, because a proxy makes many users share one

	maxLimiterKeys = 50000
)

// failLimiter counts failed attempts per key in a sliding window. It only counts failures, so
// people who type the right password never notice it.
type failLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time
}

func newFailLimiter() *failLimiter {
	return &failLimiter{fails: map[string][]time.Time{}, now: time.Now}
}

// prune drops attempts older than the window and returns what is left. Callers hold the lock.
func (l *failLimiter) prune(key string) []time.Time {
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

// blocked reports whether key already used up its attempts.
func (l *failLimiter) blocked(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= max
}

func (l *failLimiter) fail(key string) {
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
func (l *failLimiter) evict() {
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

func (l *failLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// usedNonces remembers the nonces of sealed values that were already used, until the value would
// have expired anyway, so that a value which may work only once cannot be replayed. It lives in
// memory: with several replicas behind one database the guarantee holds per replica.
type usedNonces struct {
	mu    sync.Mutex
	seen_ map[string]time.Time // nonce -> when the sealed value expires
	now   func() time.Time
}

func newUsedNonces() *usedNonces {
	return &usedNonces{seen_: map[string]time.Time{}, now: time.Now}
}

// seen reports whether nonce was already used, without recording it. A flow checks it before it
// spends anything (a code) and records the nonce with use only once it has succeeded.
func (u *usedNonces) seen(nonce string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	exp, ok := u.seen_[nonce]
	return ok && !exp.Before(u.now())
}

// use records nonce and reports whether this is the first time it is seen. A nonce that was
// forgotten under memory pressure can only belong to a value that is about to expire.
func (u *usedNonces) use(nonce string, expires time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	if len(u.seen_) > maxLimiterKeys {
		for k, exp := range u.seen_ {
			if exp.Before(now) {
				delete(u.seen_, k)
			}
		}
		if len(u.seen_) > maxLimiterKeys { // still full: drop everything rather than grow
			u.seen_ = map[string]time.Time{}
		}
	}
	if exp, ok := u.seen_[nonce]; ok && !exp.Before(now) {
		return false
	}
	u.seen_[nonce] = expires
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loginKey(login string) string { return "login:" + strings.ToLower(strings.TrimSpace(login)) }

// ipKey identifies the client for rate limiting. An IPv6 client is counted by its /64, because a
// single subscriber usually controls the whole prefix and could otherwise rotate addresses freely.
func ipKey(r *http.Request) string {
	ip := clientIP(r)
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is6() && !addr.Is4In6() {
		if p, err := addr.Prefix(64); err == nil {
			return "ip:" + p.String()
		}
	}
	return "ip:" + ip
}

// throttled reports whether this client or this login name made too many failed attempts.
func (s *Server) throttled(r *http.Request, login string) bool {
	return s.limiter.blocked(ipKey(r), maxIPFail) || (login != "" && s.limiter.blocked(loginKey(login), maxLoginFail))
}

func (s *Server) recordFailure(r *http.Request, login string) {
	s.limiter.fail(ipKey(r))
	if login != "" {
		s.limiter.fail(loginKey(login))
	}
}

// ---- compression ----

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

// compress gzips text responses for clients that accept it.
func compress(next http.Handler) http.Handler {
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

// ---- static assets ----

// staticVersions maps every embedded static file to a short content hash.
func staticVersions(fsys fs.FS) map[string]string {
	out := map[string]string{}
	fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil
		}
		sum := sha256.Sum256(b)
		out[p] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return out
}

// staticHandler serves the embedded files. Embedded files have no modification time, so without
// help browsers would download them again on every page. Pages reference them as
// /static/name?v=<hash>, which can be cached for good; a bare URL revalidates through its ETag.
func staticHandler(fsys fs.FS, versions map[string]string) http.Handler {
	files := http.StripPrefix("/static/", http.FileServerFS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if v, ok := versions[name]; ok {
			w.Header().Set("ETag", `"`+v+`"`)
			if r.URL.Query().Get("v") == v {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
		}
		files.ServeHTTP(w, r)
	})
}
