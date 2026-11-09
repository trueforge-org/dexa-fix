package web

import (
	"context"
	"net/http"
	"strings"
	"time"

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

// NewBackend builds a backend: the database side only. There is no in-process frontend; the
// internal API serves a separate frontend process (see SetServiceSecrets and InternalHandler), and
// PublicHandler serves /api/v1.
func NewBackend(st store.Store, importers *importer.Registry) (*Server, error) {
	s, err := newServer()
	if err != nil {
		return nil, err
	}
	s.store, s.importers = st, importers
	ctx := context.Background()
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

// PublicHandler serves the public API, /api/v1, and /healthz. Everything else is not found: pages
// belong to the frontend.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	s.apiRoutes(mux)
	s.integrationAPIRoutes(mux)
	s.exportAPIRoutes(mux)
	s.auditRoutes(mux)
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

// settingsPollInterval is how often a backend checks whether another process changed the settings.
var settingsPollInterval = 5 * time.Second

// StartSettingsPoll reloads the settings when another process (a second backend, or the
// `settings reset-network` command) changed them, until Close.
func (s *Server) StartSettingsPoll() {
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

func (s *Server) publicInFlightOrDefault() int {
	if s.publicSlots > 0 {
		return s.publicSlots
	}
	return publicMaxInFlight
}
