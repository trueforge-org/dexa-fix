// Package web serves the HTML frontend (Go templates + htmx) and the JSON API.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/quic-go/quic-go/http3"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/edge"
	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

//go:embed templates static
var assets embed.FS

// Server wires the store to HTTP handlers.
type Server struct {
	store     store.Store
	importers *importer.Registry
	tmpl      map[string]*template.Template

	// Version is shown on the admin page.
	Version string
	// ForcePasswordLogin keeps password sign-in on regardless of the admin setting, to recover
	// from a lockout. It is a flag because it is needed when nobody can reach the Admin page.
	ForcePasswordLogin bool

	listenAddr string     // address of the TCP listener, set by WrapListener; HTTP/3 uses the same port on UDP
	lmu        sync.Mutex // guards the HTTPS and HTTP/3 servers below
	handler    http.Handler
	https      *http.Server
	httpsPort  string
	h3         *http3.Server
	h3port     string
	bindErr    string

	live atomic.Pointer[liveSettings]               // administrator settings in force, see serversettings.go
	cop  atomic.Pointer[http.CrossOriginProtection] // rebuilt when the base URL changes

	authChain []authenticator

	api            *apiclient.Client // how the page handlers reach the backend
	serviceSecrets [][32]byte        // accepted by the internal API, see internal.go
	cookieKey      []byte
	httpClient     *http.Client

	sl slJobs // background Strength Level syncs

	jobCtx    context.Context // parent of background jobs; cancelled by Close
	cancelJob context.CancelFunc
	jobs      sync.WaitGroup
	jobsMu    sync.Mutex // guards jobsDone and the Add in startJob against Close's Wait
	jobsDone  bool
	slSlots   chan struct{} // limits Strength Level syncs running at the same time

	auditNoise *auditNoise
	estCache   *estimateCache
	proxy      edge.Proxy
	limiter    *failLimiter      // failed sign-in attempts
	nonces     nonceStore        // sealed values that were already used
	assetVer   map[string]string // content hash per static file, for cache-busting URLs

	oidcMu    sync.Mutex
	oidcCache map[string]*oidc.Provider
}

var pages = []string{"index.html", "dexa.html", "scale.html", "estimates.html", "settings.html", "admin.html", "docs.html", "twofactor.html", "reset.html"}

// New builds a Server and parses the embedded templates. importers may be nil.
func New(st store.Store, importers *importer.Registry) (*Server, error) {
	funcs := template.FuncMap{
		"day": func(t time.Time) string { return t.Format("2006-01-02") },
		"dt":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },

		"initial":   initial,
		"help":      help,
		"timezones": func() []string { return commonTimezones },
	}
	static, _ := fs.Sub(assets, "static")
	s := &Server{
		store: st, importers: importers, tmpl: map[string]*template.Template{},
		httpClient: &http.Client{Timeout: providerTimeout},
		oidcCache:  map[string]*oidc.Provider{},
		limiter:    newFailLimiter(),
		nonces:     newUsedNonces(),
		slSlots:    make(chan struct{}, maxStrengthLevelJobs),
		auditNoise: newAuditNoise(),
		estCache:   newEstimateCache(),
		assetVer:   staticVersions(static),
	}
	funcs["asset"] = s.assetURL
	s.jobCtx, s.cancelJob = context.WithCancel(context.Background())
	s.authChain = []authenticator{s.authSession, s.authBearer, s.authBasic}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs).ParseFS(assets, "templates/base.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		s.tmpl[p] = t
	}
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
	if err := s.connectInProcess(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close cancels the background jobs and waits for them to stop. Call it after the HTTP server has
// been shut down.
func (s *Server) Close() {
	s.jobsMu.Lock()
	s.jobsDone = true
	s.jobsMu.Unlock()
	s.cancelJob()
	s.jobs.Wait()
	s.closeListeners()
}

// startJob runs f in the background, tracked by Close. It reports false, without running f, once
// the server is closing.
func (s *Server) startJob(f func()) bool {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if s.jobsDone {
		return false
	}
	s.jobs.Add(1)
	go func() {
		defer s.jobs.Done()
		f()
	}()
	return true
}

// assetURL returns the cache-busting URL of an embedded static file.
func (s *Server) assetURL(name string) string {
	if v, ok := s.assetVer[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", staticHandler(static, s.assetVer))

	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /docs", s.docsPage)
	s.authRoutes(mux)
	s.adminRoutes(mux)
	s.twoFactorRoutes(mux)
	s.resetRoutes(mux)
	s.passkeyRoutes(mux)
	mux.HandleFunc("GET /u/{id}", s.user(s.home))

	mux.HandleFunc("GET /u/{id}/dexa", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) { s.dexaPage(w, r, u, "", http.StatusOK) }))
	mux.HandleFunc("POST /u/{id}/dexa", s.user(s.dexaCreate))
	mux.HandleFunc("POST /u/{id}/dexa/{item}/delete", s.user(s.dexaDelete))

	mux.HandleFunc("GET /u/{id}/scale", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) { s.scalePage(w, r, u, "", http.StatusOK) }))
	mux.HandleFunc("POST /u/{id}/scale", s.user(s.scaleCreate))
	mux.HandleFunc("POST /u/{id}/scale/{item}/delete", s.user(s.scaleDelete))
	mux.HandleFunc("GET /u/{id}/estimates", s.user(s.estimatesPage))
	mux.HandleFunc("POST /u/{id}/estimates/columns", s.user(s.columnsSave))
	mux.HandleFunc("GET /u/{id}/settings", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		s.settingsPage(w, r, u, "", flashParam(r), http.StatusOK)
	}))
	mux.HandleFunc("POST /u/{id}/settings", s.user(s.settingsSave))
	mux.HandleFunc("POST /u/{id}/strengthlevel/login", s.user(s.strengthLevelSaveLogin))
	mux.HandleFunc("POST /u/{id}/strengthlevel/sync", s.user(s.strengthLevelSync))
	mux.HandleFunc("GET /u/{id}/strengthlevel/status", s.user(s.strengthLevelStatus))
	mux.HandleFunc("POST /u/{id}/strengthlevel/day", s.user(s.strengthLevelDay))
	mux.HandleFunc("POST /u/{id}/strengthlevel/login/delete", s.user(s.strengthLevelDeleteLogin))

	s.apiRoutes(mux)
	s.integrationRoutes(mux)
	s.exportRoutes(mux)
	s.auditRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	// Same-origin checks protect the cookie-authenticated forms. The Apple callback arrives as a
	// cross-site POST and is protected by its signed state instead.
	// The check is rebuilt when the base URL changes, so it is looked up for every request.
	guarded := s.withVisitor(s.frontAuth(mux))
	sameOrigin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cop.Load().Handler(guarded).ServeHTTP(w, r)
	})
	return edge.LogRequests(s.proxyHeaders(s.altSvc(edge.SecureHeaders(edge.Compress(edge.LimitBody(sameOrigin))))))
}

// pageData is the data passed to every template.
type pageData struct {
	Title  string
	Tab    string
	User   *models.User
	Users  []models.User
	Error  string
	Fields []field
	Rows   []row
	Today  string
	Now    string
	// Timezone names the time zone of the date-times on the page (the user's).
	Timezone string

	Methods        []models.MethodInfo // measurement methods for the scale form
	AthleteDefault bool                // preset of the athlete-mode checkbox
	Ranking        []rankRow           // settings page
	Saved          string

	Msg      string // flash message shown at the top of the estimates page
	Withings *integrationView
	Resync   bool // show the per-day re-import button

	EstFields   []estimate.Field
	EstRange    string // description of the period the daily table covers
	EstRangeKey string
	EstColumns  []estColumn // every column, for the picker
	EstRows     []estRow
	Cals        []calRow
	Overview    *overviewView // summary above the estimates table, nil without data
	Pager       *pager        // scale page: links to older readings

	Login     *loginView     // sign-in page
	TwoFactor *twoFactorView // second step of a sign-in
	Reset     *resetView     // password reset pages

	SLSend        bool          // estimates page: show the button that sends one day to Strength Level
	StrengthLevel *slView       // settings page: only for users an administrator has enabled it for
	Security      *securityView // settings page: password, tokens and linked accounts
	Admin         *adminView    // admin page
}

func (s *Server) render(w http.ResponseWriter, page string, status int, data pageData) {
	var buf bytes.Buffer
	if err := s.tmpl[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		log.Error().Err(err).Str("page", page).Msg("render failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var apiErr *apiclient.Error
	switch {
	case errors.Is(err, apiclient.ErrUnavailable):
		log.Error().Err(err).Msg("backend request failed")
		w.Header().Set("Retry-After", "5")
		http.Error(w, "dexa-fix is temporarily unavailable, try again in a moment", http.StatusServiceUnavailable)
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			http.Error(w, apiErr.Message, apiErr.Status)
		case http.StatusInternalServerError:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			http.Error(w, apiErr.Message, http.StatusBadRequest)
		}
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, store.ErrInvalid), errors.Is(err, store.ErrExists):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		log.Error().Err(err).Msg("request failed")
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func itemID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	return id, err == nil
}

// initial returns the upper-cased first character of a name, for avatars.
func initial(name string) string {
	r, _ := utf8.DecodeRuneInString(strings.TrimSpace(name))
	if r == utf8.RuneError {
		return "?"
	}
	return strings.ToUpper(string(r))
}

func userPath(id int64, tab string) string {
	p := "/u/" + strconv.FormatInt(id, 10)
	if tab != "" {
		p += "/" + tab
	}
	return p
}

func (s *Server) home(w http.ResponseWriter, r *http.Request, u models.User) {
	http.Redirect(w, r, userPath(u.ID, "estimates"), http.StatusSeeOther)
}
