package web

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/edge"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The backend half on its own: the process that owns the database, the keys and every security
// decision. It serves two listeners with separate load budgets, so a flood on one cannot starve the
// other: the public API (/api/v1, for scripts and apps, behind the reverse proxy) and the internal
// API (/internal/v1, for the frontend, on a private network).

// Load budgets. They protect the process, so they are constants and not administrator settings.
const (
	publicMaxInFlight = 32
	publicMaxQueue    = 64
	publicRate        = 20 // requests per second per client address
	publicBurst       = 40

	internalMaxInFlight = 128
	internalMaxQueue    = 256
	internalRate        = 50
	internalBurst       = 100

	budgetMaxWait = 2 * time.Second
)

// poolSizer is implemented by stores that know their connection pool size.
type poolSizer interface{ MaxConns() int }

// publicInFlight is how many public API requests are served at once: at most half of the database
// connections, so the internal API (the pages) always finds some free however hard the public one
// is pushed. Stores that do not say how many connections they have get the default.
func publicInFlight(st store.Store) int {
	if p, ok := st.(poolSizer); ok && p.MaxConns() > 0 {
		return max(1, min(publicMaxInFlight, p.MaxConns()/2))
	}
	return publicMaxInFlight
}

// Backend is the half of the application that owns the database, the keys and every security
// decision. It never renders a page. Nothing in the frontend can reach its fields: the Frontend is
// another type, and the two meet only in the internal API (see internal.go).
type Backend struct {
	*jobGroup // background jobs, stopped by Close

	store     store.Store
	importers *importer.Registry

	// Version is shown on the admin page.
	Version string
	// ForcePasswordLogin keeps password sign-in on regardless of the admin setting, to recover
	// from a lockout. It is a flag because it is needed when nobody can reach the Admin page.
	ForcePasswordLogin bool

	live       atomic.Pointer[liveSettings] // administrator settings in force, see serversettings.go
	onSettings func()                       // called after the settings in force change

	authChain      []authenticator
	cookieKey      []byte
	httpClient     *http.Client
	serviceSecrets [][32]byte    // accepted by the internal API, see internal.go
	publicSlots    int           // in-flight limit of the public API, from the database pool
	bcryptSlots    chan struct{} // limits password checks running at the same time
	internalBudget *edge.Limiter // load budget of the internal API; nil in the combined process

	sl      slJobs        // background Strength Level syncs
	slSlots chan struct{} // limits Strength Level syncs running at the same time

	auditNoise *auditNoise
	estCache   *estimateCache
	proxy      edge.Proxy
	limiter    *failLimiter // failed sign-in attempts
	nonces     nonceStore   // sealed values that were already used

	oidcMu    sync.Mutex
	oidcCache map[string]*oidc.Provider
}

// NewBackend builds a backend: the database side only. The internal API serves a frontend (see
// SetServiceSecrets and InternalHandler), and PublicHandler serves /api/v1.
func NewBackend(st store.Store, importers *importer.Registry) (*Backend, error) {
	s := &Backend{
		jobGroup:    newJobGroup(),
		store:       st,
		importers:   importers,
		httpClient:  &http.Client{Timeout: providerTimeout},
		oidcCache:   map[string]*oidc.Provider{},
		limiter:     newFailLimiter(),
		nonces:      newUsedNonces(),
		slSlots:     make(chan struct{}, maxStrengthLevelJobs),
		bcryptSlots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/4)),
		auditNoise:  newAuditNoise(),
		estCache:    newEstimateCache(),
	}
	s.authChain = []authenticator{s.authSession, s.authBearer, s.authBasic}
	ctx := context.Background()
	var err error
	if s.cookieKey, err = s.loadCookieKey(ctx); err != nil {
		return nil, err
	}
	if err := s.applyWithingsCredentials(ctx); err != nil {
		return nil, err
	}
	if err := s.reloadSettings(ctx); err != nil {
		return nil, err
	}
	s.publicSlots = publicInFlight(st)
	s.internalBudget = &edge.Limiter{
		MaxInFlight: internalMaxInFlight, MaxQueue: internalMaxQueue, MaxWait: budgetMaxWait,
		Rate: internalRate, Burst: internalBurst,
		Skip: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/healthz") },
	}
	return s, nil
}

// Close cancels the background jobs and waits for them to stop. Call it after the HTTP servers
// have been shut down.
func (s *Backend) Close() { s.stopJobs() }

// WrapListener makes ln understand the PROXY protocol according to the current settings. Wrap the
// listener once at start; it costs nothing while the setting is off.
func (s *Backend) WrapListener(ln net.Listener) net.Listener {
	return edge.ProxyListener(ln, s.proxyProtocolPolicy)
}

// proxyProtocolPolicy is read by the listener wrapper.
func (s *Backend) proxyProtocolPolicy() (enabled bool, trusted []netip.Prefix) {
	l := s.cfg()
	return l.proxyProtocol, l.trusted
}

func (s *Backend) trusted(a netip.Addr) bool { return edge.Trusted(s.cfg().trusted, a) }

func (s *Backend) proxyHeaders(next http.Handler) http.Handler {
	return s.proxy.Headers(func() (*url.URL, []netip.Prefix) {
		l := s.cfg()
		return l.baseURL, l.trusted
	}, next)
}

// PublicHandler serves the public API, /api/v1, and /healthz. Everything else is not found: pages
// belong to the frontend.
func (s *Backend) PublicHandler() http.Handler {
	mux := s.apiMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	apiOnly := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/healthz" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		mux.ServeHTTP(w, r)
	})
	budget := &edge.Limiter{
		MaxInFlight: s.publicInFlightOrDefault(), MaxQueue: publicMaxQueue, MaxWait: budgetMaxWait,
		Rate: publicRate, Burst: publicBurst,
		Skip: func(r *http.Request) bool { return r.URL.Path == "/healthz" },
	}
	return edge.LogRequests(s.proxyHeaders(edge.SecureHeaders(edge.Compress(budget.Middleware(s.withAuth(apiOnly))))))
}

// apiMux has the routes of the public API.
func (s *Backend) apiMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.apiRoutes(mux)
	s.integrationAPIRoutes(mux)
	s.exportAPIRoutes(mux)
	s.auditRoutes(mux)
	return mux
}

// apiHandler serves the public API, authenticated, without the load budget and the edge handling of
// PublicHandler: the combined process mounts it on the pages' handler, which does both.
func (s *Backend) apiHandler() http.Handler { return s.withAuth(s.apiMux()) }

// settingsPollInterval is how often a backend checks whether another process changed the settings.
var settingsPollInterval = 5 * time.Second

// StartSettingsPoll reloads the settings when another process (a second backend, or the
// `settings reset-network` command) changed them, until Close.
func (s *Backend) StartSettingsPoll() {
	s.startJob(func() {
		seen, _ := s.store.SettingsGeneration(s.jobCtx)
		t := time.NewTicker(settingsPollInterval)
		defer t.Stop()
		for {
			select {
			case <-s.jobCtx.Done():
				return
			case <-t.C:
			}
			g, err := s.store.SettingsGeneration(s.jobCtx)
			if err != nil {
				if s.jobCtx.Err() == nil {
					log.Warn().Err(err).Msg("could not check for changed settings")
				}
				continue
			}
			if g == seen {
				continue
			}
			if err := s.reloadSettings(s.jobCtx); err != nil {
				log.Error().Err(err).Msg("reload the changed settings")
				continue
			}
			seen = g
		}
	})
}

func (s *Backend) publicInFlightOrDefault() int {
	if s.publicSlots > 0 {
		return s.publicSlots
	}
	return publicMaxInFlight
}
