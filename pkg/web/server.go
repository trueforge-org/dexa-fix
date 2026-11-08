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
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"

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
	// AuditRetention is how long audit events are kept; zero keeps them for good.
	AuditRetention time.Duration
	// TrustedProxies are the reverse proxies whose X-Forwarded-* headers are believed.
	TrustedProxies []netip.Prefix
	// BaseURL is the public address of the app (scheme and host); nil derives it from requests.
	BaseURL *url.URL
	// ForcePasswordLogin keeps password sign-in on regardless of the admin setting, to recover
	// from a lockout.
	ForcePasswordLogin bool

	authChain  []authenticator
	cookieKey  []byte
	httpClient *http.Client

	sl slJobs // background Strength Level syncs

	jobCtx    context.Context // parent of background jobs; cancelled by Close
	cancelJob context.CancelFunc
	jobs      sync.WaitGroup

	auditNoise      *auditNoise
	estCache        *estimateCache
	warnedUntrusted atomic.Bool
	limiter         *failLimiter      // failed sign-in attempts
	assetVer        map[string]string // content hash per static file, for cache-busting URLs

	oidcMu    sync.Mutex
	oidcCache map[string]*oidc.Provider
}

var pages = []string{"index.html", "dexa.html", "scale.html", "estimates.html", "settings.html", "admin.html", "docs.html"}

// New builds a Server and parses the embedded templates. importers may be nil.
func New(st store.Store, importers *importer.Registry) (*Server, error) {
	funcs := template.FuncMap{
		"day": func(t time.Time) string { return t.Format("2006-01-02") },
		"dt":  func(t time.Time) string { return t.Format("2006-01-02 15:04") },

		"initial": initial,
		"help":    help,
	}
	static, _ := fs.Sub(assets, "static")
	s := &Server{
		store: st, importers: importers, tmpl: map[string]*template.Template{},
		httpClient: &http.Client{Timeout: providerTimeout},
		oidcCache:  map[string]*oidc.Provider{},
		limiter:    newFailLimiter(),
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
	return s, nil
}

// Close cancels the background jobs and waits for them to stop. Call it after the HTTP server has
// been shut down.
func (s *Server) Close() {
	s.cancelJob()
	s.jobs.Wait()
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
	cop := http.NewCrossOriginProtection()
	cop.AddInsecureBypassPattern("POST /auth/{provider}/callback")
	if s.BaseURL != nil {
		if err := cop.AddTrustedOrigin(s.BaseURL.Scheme + "://" + s.BaseURL.Host); err != nil {
			log.Error().Err(err).Msg("base URL not accepted as a trusted origin")
		}
	}
	return logRequests(s.proxyHeaders(secureHeaders(compress(limitBody(cop.Handler(s.withAuth(mux)))))))
}

// integrationView is the state of an OAuth integration for the current user.
type integrationView struct {
	Configured bool // the server has the provider's credentials
	Connected  bool // the user has linked their account
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

	Login *loginView // sign-in page

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
	switch {
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

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Debug().Str("method", r.Method).Str("path", r.URL.Path).Dur("took", time.Since(start)).Msg("request")
	})
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
