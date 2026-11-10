// Package mainbackend is the main backend, the process behind `dexa-fix serve api`. It owns the
// database, the keys and every security decision: the internal API that the frontend calls, the
// public API (/api/v1), the importers and the maintenance jobs. It never renders a page, and it
// does not import the frontend. (The directory is pkg/backend/main; a package cannot be named main
// and be imported.)
package mainbackend

import (
	"context"
	"errors"
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

	"github.com/trueforge-org/dexa-fix/pkg/shared/edge"
	"github.com/trueforge-org/dexa-fix/pkg/shared/jobs"
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
	bg *jobs.Group // background jobs, stopped by Close

	store store.Store
	body  bodyService // scans, readings and estimates live in the body service, see body.go

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

	// Work for the workers: the queue is in the database; these are the registry of providers and
	// what the worker endpoints need. See workerproviders.go and internal_worker.go.
	providers     []workerProvider
	kinds         map[string]*kindSpec
	workerSecrets [][32]byte    // accepted by the worker endpoints
	workerBudget  *edge.Limiter // load budget of the worker endpoints; nil in the combined process
	leasePolls    chan struct{} // worker requests waiting for a job
	lastSweep     atomic.Int64  // when this process last ended expired leases (UnixNano)

	auditNoise *auditNoise
	proxy      edge.Proxy
	limiter    *failureLimiter // failed sign-in attempts, counted in the database or in Valkey
	nonces     nonceStore      // sealed values that were already used, recorded in the database
	noCertGen  bool            // do not generate or renew the self-signed certificate on our own

	oidcMu    sync.Mutex
	oidcCache map[string]*oidc.Provider

	statusCache statusCache // the last answer of /status, kept for a few seconds
	state       StateServer // the Valkey server of WithState, or nil: then the database holds the state
}

// Option changes how NewBackend builds a backend.
type Option func(*Backend)

// WithState keeps the failed-attempt counters and the single-use nonces on a server of their own
// (Valkey) instead of in the database, so that a flood of wrong passwords costs the database
// nothing. It fails closed like the database does: when the server does not answer, every address
// counts as locked out and every nonce as spent. The server must not evict keys
// (maxmemory-policy noeviction), or a counter or nonce could vanish and let an attacker try again or
// replay a value. /status shows it, and Close of the backend does not close it: the caller owns it.
func WithState(v StateServer) Option {
	return func(s *Backend) {
		s.limiter = newFailureLimiter(v)
		s.nonces = spentNonces{v}
		s.state = v
	}
}

// WithoutCertificateGeneration makes the backend only load the stored self-signed certificate. It
// never generates or renews one by itself (the maintenance backend does); only an administrator's
// explicit TLS change on the Admin page still generates one. For backends that run with
// --maintenance=false.
func WithoutCertificateGeneration() Option { return func(s *Backend) { s.noCertGen = true } }

// NewBackend builds a backend: the database side only. The internal API serves a frontend (see
// SetServiceSecrets and InternalHandler), and PublicHandler serves /api/v1.
//
// Any number of backends may share one database; exactly one of them should run the maintenance
// (StartMaintenance) and the others should be made WithoutCertificateGeneration.
func NewBackend(st store.Store, opts ...Option) (*Backend, error) {
	s := &Backend{
		bg:          jobs.New(),
		store:       st,
		httpClient:  &http.Client{Timeout: providerTimeout},
		oidcCache:   map[string]*oidc.Provider{},
		limiter:     newFailureLimiter(st),
		nonces:      spentNonces{st},
		leasePolls:  make(chan struct{}, maxLeasePolls),
		bcryptSlots: make(chan struct{}, max(1, runtime.GOMAXPROCS(0)/4)),
		auditNoise:  newAuditNoise(),
	}
	for _, o := range opts {
		o(s)
	}
	if s.body.c == nil {
		return nil, errors.New("a backend needs a body service for the scans, readings and estimates: pass WithBody")
	}
	// The providers that ship with the program; their settings are read below.
	s.addWorkerProvider(withingsProvider())
	s.addWorkerProvider(strengthLevelProviderSpec())
	s.authChain = []authenticator{s.authSession, s.authBearer, s.authBasic}
	ctx := context.Background()
	var err error
	if s.cookieKey, err = s.loadCookieKey(ctx); err != nil {
		return nil, err
	}
	if err := s.reloadSettings(ctx); err != nil {
		return nil, err
	}
	s.publicSlots = publicInFlight(st)
	s.workerBudget = newWorkerBudget()
	s.internalBudget = &edge.Limiter{
		MaxInFlight: internalMaxInFlight, MaxQueue: internalMaxQueue, MaxWait: budgetMaxWait,
		Rate: internalRate, Burst: internalBurst,
		Skip: func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/healthz") },
	}
	return s, nil
}

// Close cancels the background jobs and waits for them to stop. Call it after the HTTP servers
// have been shut down.
func (s *Backend) Close() { s.bg.Stop() }

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

// APIHandler serves the public API, authenticated, without the load budget and the edge handling of
// PublicHandler: the combined process mounts it on the pages' handler, which does both.
func (s *Backend) APIHandler() http.Handler { return s.withAuth(s.apiMux()) }

// DisableInternalBudget removes the load budget of the internal API, and of the worker endpoints.
// The combined process uses it: there is no flood protection between the halves of one process.
func (s *Backend) DisableInternalBudget() { s.internalBudget, s.workerBudget = nil, nil }

// OnSettingsChange sets a function that is called after the settings in force change. The combined
// process uses it to pass a changed setting on to the frontend at once. Set it before serving.
func (s *Backend) OnSettingsChange(f func()) { s.onSettings = f }

// settingsPollInterval is how often a backend checks whether another process changed the settings.
var settingsPollInterval = 5 * time.Second

// StartSettingsPoll reloads the settings when another process (a second backend, or the
// `settings reset-network` command) changed them, until Close.
func (s *Backend) StartSettingsPoll() {
	s.bg.Start(func() {
		seen, _ := s.store.SettingsGeneration(s.bg.Ctx)
		t := time.NewTicker(settingsPollInterval)
		defer t.Stop()
		for {
			select {
			case <-s.bg.Ctx.Done():
				return
			case <-t.C:
			}
			g, err := s.store.SettingsGeneration(s.bg.Ctx)
			if err != nil {
				if s.bg.Ctx.Err() == nil {
					log.Warn().Err(err).Msg("could not check for changed settings")
				}
				continue
			}
			if g == seen {
				continue
			}
			if err := s.reloadSettings(s.bg.Ctx); err != nil {
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
