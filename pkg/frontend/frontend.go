package frontend

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
	"github.com/trueforge-org/dexafix/pkg/shared/jobs"
	"github.com/trueforge-org/dexafix/pkg/shared/must"
	"github.com/trueforge-org/dexafix/pkg/shared/timezones"
	"github.com/trueforge-org/dexafix/pkg/shared/tlsutil"
)

// The frontend half on its own: a process with no database that renders pages from what the
// backend's internal API answers, serves the static files, and owns the public listeners (plain,
// HTTPS and HTTP/3). The administrator settings it acts on come from the backend, see
// RefreshConfig.

// Frontend is the half of the application that renders pages. It has no database and no keys: it
// asks the backend for everything through api, the internal API over HTTP, and acts only on the
// few administrator settings the backend hands it (frontendSettings). The Backend is another type,
// so nothing here can reach its fields.
type Frontend struct {
	bg *jobs.Group // the settings poll, stopped by Close

	api       *apiclient.Client // how the page handlers reach the backend
	tmpl      map[string]*template.Template
	assetVer  map[string]string // content hash per static file, for cache-busting URLs
	proxy     edge.Proxy
	unlimited bool // the combined process has no request limits of its own

	// Version is shown on the admin page.
	Version string

	live atomic.Pointer[frontendSettings]           // the settings in force, see RefreshConfig
	cop  atomic.Pointer[http.CrossOriginProtection] // rebuilt when the base URL changes

	cfgMu         sync.Mutex // serialises RefreshConfig
	configETag    string     // of the frontend config in force
	warnedVersion bool

	apiUpstream *httputil.ReverseProxy // where /api/ goes, if set
	apiHandler  http.Handler           // the public API of an in-process backend, if set

	listenAddr string     // address of the TCP listener, set by WrapListener; HTTP/3 uses the same port on UDP
	lmu        sync.Mutex // guards the HTTPS and HTTP/3 servers below
	handler    http.Handler
	https      *http.Server
	httpsPort  string
	h3         *http3.Server
	h3port     string
	bindErr    string
}

// frontendSettings is the set of administrator settings the frontend acts on, a part of the
// backend's (see liveSettings). It is replaced as a whole, so a request sees one consistent set.
type frontendSettings struct {
	baseURL       *url.URL
	trusted       []netip.Prefix
	proxyProtocol bool
	tlsMode       string // off, upload or selfsigned
	tlsCert       *tls.Certificate
	tlsInfo       *tlsutil.Info
	http3         bool
	httpsPort     string // TCP port for HTTPS, and UDP port for HTTP/3
	analytics     apiclient.Analytics
}

// NewFrontend builds a frontend that talks to the backend through api and parses the embedded
// templates. Call RefreshConfig before serving, and StartConfigPoll to follow changes.
func NewFrontend(api *apiclient.Client) (*Frontend, error) {
	s := &Frontend{
		bg:   jobs.New(),
		api:  api,
		tmpl: map[string]*template.Template{},
	}
	static, _ := fs.Sub(assets, "static")
	s.assetVer = staticVersions(static)
	funcs := template.FuncMap{
		"initial":    initial,
		"help":       help,
		"timezones":  func() []string { return timezones.Common },
		"asset":      s.assetURL,
		"repoURL":    func() string { return repoURL },
		"sponsorURL": func() string { return sponsorURL },
	}
	for _, p := range pages {
		// The templates are embedded in the binary, so a parse error is a bug found by any test.
		s.tmpl[p] = must.Value(template.New(p).Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+p))
	}
	s.swapSettings(&frontendSettings{tlsMode: apiclient.TLSOff, httpsPort: defaultHTTPSPort})
	return s, nil
}

// DisableLimits turns off the request limits of the frontend. The combined process uses it: it has
// a load budget of its own, shared with the API.
func (s *Frontend) DisableLimits() { s.unlimited = true }

// Close cancels the background jobs and waits for them to stop, and stops the HTTPS and HTTP/3
// servers. Call it after the HTTP server has been shut down.
func (s *Frontend) Close() {
	s.bg.Stop()
	s.closeListeners()
}

// cfg returns the settings in force. It is never nil.
func (s *Frontend) cfg() *frontendSettings { return s.live.Load() }

// swapSettings replaces the settings in force and rebuilds what depends on them.
func (s *Frontend) swapSettings(l *frontendSettings) {
	s.live.Store(l)
	s.cop.Store(newCrossOriginProtection(l.baseURL))
}

func (s *Frontend) proxyHeaders(next http.Handler) http.Handler {
	return s.proxy.Headers(func() (*url.URL, []netip.Prefix) {
		l := s.cfg()
		return l.baseURL, l.trusted
	}, next)
}

// RefreshConfig fetches the settings from the backend and puts them in force if they changed.
func (s *Frontend) RefreshConfig(ctx context.Context) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cfg, etag, err := s.api.FrontendConfig(ctx, s.configETag)
	if err != nil {
		return err
	}
	if cfg == nil { // not modified
		return nil
	}
	if err := s.applyFrontendConfig(*cfg); err != nil {
		return err
	}
	s.configETag = etag
	if cfg.Version != "" && s.Version != "" && cfg.Version != s.Version && !s.warnedVersion {
		s.warnedVersion = true
		log.Warn().Str("frontend", s.Version).Str("backend", cfg.Version).Msg("the frontend and the backend run different versions: deploy both from the same release")
	}
	return nil
}

// applyFrontendConfig puts the settings in force and rebuilds what depends on them: the
// same-origin check and the listeners.
func (s *Frontend) applyFrontendConfig(c apiclient.FrontendConfig) error {
	l := &frontendSettings{tlsMode: c.TLSMode, httpsPort: c.HTTPSPort, proxyProtocol: c.ProxyProtocol, analytics: c.Analytics}
	if l.tlsMode != apiclient.TLSUpload && l.tlsMode != apiclient.TLSSelfSigned {
		l.tlsMode = apiclient.TLSOff
	}
	if l.httpsPort == "" {
		l.httpsPort = defaultHTTPSPort
	}
	var err error
	if l.baseURL, err = edge.ParseBaseURL(c.BaseURL); err != nil {
		return fmt.Errorf("base URL from the backend: %w", err)
	}
	if l.trusted, err = edge.ParseTrustedProxies(c.TrustedProxies); err != nil {
		return fmt.Errorf("trusted proxies from the backend: %w", err)
	}
	if c.CertPEM != "" && l.tlsMode != apiclient.TLSOff {
		cert, info, err := tlsutil.Parse(c.CertPEM, c.KeyPEM)
		if err != nil {
			log.Warn().Err(err).Msg("ignoring the TLS certificate from the backend")
		} else {
			l.tlsCert, l.tlsInfo = cert, info
		}
	}
	l.http3 = c.HTTP3 && l.tlsCert != nil
	s.swapSettings(l)
	s.applyListeners()
	return nil
}

// WaitForBackend fetches the first configuration, retrying until the backend answers or timeout
// passes, so a frontend started together with its backend does not give up too early.
func (s *Frontend) WaitForBackend(ctx context.Context, timeout time.Duration) error {
	started := time.Now()
	deadline := started.Add(timeout)
	delay := 250 * time.Millisecond
	var lastSaid time.Time
	for {
		err := s.RefreshConfig(ctx)
		if err == nil {
			return nil
		}
		if apiErr := new(apiclient.Error); errors.As(err, &apiErr) && apiErr.Status == 401 {
			return fmt.Errorf("the backend refused the service secret: use the same --service-secret-file as the api process")
		}
		if time.Now().Add(delay).After(deadline) {
			return fmt.Errorf("the backend is not available: %w", err)
		}
		// Say it once, then every ten seconds: the retries are quick at first and would fill the log.
		if lastSaid.IsZero() || time.Since(lastSaid) >= waitingLogEvery {
			log.Info().Err(err).Dur("waited", time.Since(started).Round(time.Second)).Msg("waiting for the backend")
			lastSaid = time.Now()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// waitingLogEvery is how often WaitForBackend repeats that it is still waiting.
const waitingLogEvery = 10 * time.Second

// configPollInterval is how often the frontend asks whether the settings changed. The answer is a
// cheap "not modified" in the normal case.
var configPollInterval = 5 * time.Second

// StartConfigPoll follows changes to the settings until Close. A backend that is down is not an
// error here: the frontend keeps the last good settings.
func (s *Frontend) StartConfigPoll() {
	s.bg.Start(func() {
		for {
			wait := configPollInterval + time.Duration(rand.Int64N(int64(configPollInterval/5)+1))
			select {
			case <-s.bg.Ctx.Done():
				return
			case <-time.After(wait):
			}
			ctx, cancel := context.WithTimeout(s.bg.Ctx, 10*time.Second)
			if err := s.RefreshConfig(ctx); err != nil && s.bg.Ctx.Err() == nil {
				log.Warn().Err(err).Msg("could not refresh the settings from the backend; keeping the last ones")
			}
			cancel()
		}
	})
}

// Load budget of the frontend. Static files and /healthz are never limited; everything else is
// shed here, before it turns into calls to the backend.
const (
	frontendMaxInFlight = 64
	frontendMaxQueue    = 128
	frontendRate        = 10 // requests per second per client address
	frontendBurst       = 30
	signInRate          = 2 // unauthenticated POSTs: sign-in, sign-up, reset, second step, callbacks
	signInBurst         = 10
)

// frontendLimiter returns the middleware that applies the frontend's load budget: a cap on
// requests being served at once (excess ones get a cheap 503 page) and a rate per client address,
// stricter for the unauthenticated POSTs that cost the backend the most.
func (s *Frontend) frontendLimiter() func(http.Handler) http.Handler {
	general := &edge.Limiter{
		MaxInFlight: frontendMaxInFlight, MaxQueue: frontendMaxQueue, MaxWait: 2 * time.Second,
		Rate: frontendRate, Burst: frontendBurst,
		Skip: func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
		},
	}
	signIn := &edge.Limiter{Rate: signInRate, Burst: signInBurst, Key: func(r *http.Request) string { return "signin:" + edge.IPKey(r) }}
	return func(next http.Handler) http.Handler {
		inner := general.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && unauthenticatedPost(r.URL.Path) && !signIn.Allow(signIn.Key(r)) {
				w.Header().Set("Retry-After", "2")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			inner.ServeHTTP(w, r)
		})
	}
}

// unauthenticatedPost reports whether path takes a POST from someone who is not signed in yet.
func unauthenticatedPost(path string) bool {
	switch path {
	case "/login", "/signup", "/password/forgot", "/password/reset", "/login/2fa", "/login/2fa/setup",
		"/passkey/login/begin", "/passkey/login/finish":
		return true
	}
	return strings.HasPrefix(path, "/auth/")
}

// SetAPIUpstream makes the frontend pass /api/ on to the backend's public listener, for setups with
// one public address and no reverse proxy that routes by path. The backend must list this process
// among its trusted proxies to see visitors' addresses.
func (s *Frontend) SetAPIUpstream(u *url.URL) {
	s.apiUpstream = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			// Rebuilt from what this process settled, never copied from the visitor.
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Proto")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Real-IP")
			pr.Out.Header.Set("X-Forwarded-For", edge.ClientIP(pr.In))
			if edge.IsSecure(pr.In) {
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			} else {
				pr.Out.Header.Set("X-Forwarded-Proto", "http")
			}
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Host = pr.In.Host
		},
		// This process adds the security headers to every response; the upstream's copies would
		// repeat them.
		ModifyResponse: func(resp *http.Response) error {
			for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options",
				"Referrer-Policy", "Strict-Transport-Security"} {
				resp.Header.Del(h)
			}
			// The API chooses its own browser caching (the estimates are "private, no-cache" with an
			// ETag), but nothing it answers may reach a shared cache, and a missing or public value
			// from a misbehaving upstream must not either.
			if cc := strings.ToLower(resp.Header.Get("Cache-Control")); cc == "" || strings.Contains(cc, "public") {
				resp.Header.Set("Cache-Control", "no-store")
			}
			resp.Header.Set("CDN-Cache-Control", "no-store")
			resp.Header.Set("Surrogate-Control", "no-store")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error().Err(err).Msg("the API upstream failed")
			edge.NoStore(w.Header()) // the mux removed the header the proxy would have replaced
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "the API is temporarily unavailable"})
		},
	}
}

// SetAPIHandler serves /api/ with h, the public API of a backend in the same process.
func (s *Frontend) SetAPIHandler(h http.Handler) { s.apiHandler = h }

// frontendAPIRoutes are the routes the frontend adds to the pages: /api/ (served by an in-process
// backend, passed on to the api process, or a clear answer where the API lives) and the readiness check.
func (s *Frontend) frontendAPIRoutes(mux *http.ServeMux) {
	if s.apiHandler != nil {
		mux.Handle("/api/", s.apiHandler)
	} else if s.apiUpstream != nil {
		mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Del("Cache-Control") // the API chooses its own caching
			s.apiUpstream.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "the API is served by the api process, not by this address"})
		})
	}
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.api.Health(ctx); err != nil {
			http.Error(w, "the backend is not available", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
}
