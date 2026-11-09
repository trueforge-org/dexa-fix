// Package web serves the HTML frontend (Go templates + htmx) and the JSON API.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

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

var pages = []string{"index.html", "dexa.html", "scale.html", "estimates.html", "settings.html", "admin.html", "docs.html", "twofactor.html", "reset.html"}

// Server is the application as one process: a Frontend and a Backend that talk to each other
// through the internal API over an in-process transport, exactly as two processes would over the
// network. The two halves are separate types, so the compiler keeps the frontend from reaching the
// database, the keys and the other state of the backend; see boundary_test.go.
//
// Names that both halves have are defined here, so they act on the combined process.
type Server struct {
	*Frontend
	*Backend
}

// New builds the combined Server. importers may be nil.
//
// The page handlers call the backend (the internal API) through an in-process transport, and the
// public API is served on the same handler, without the load budget of a separate backend.
func New(st store.Store, importers *importer.Registry) (*Server, error) {
	b, err := NewBackend(st, importers)
	if err != nil {
		return nil, err
	}
	b.internalBudget = nil // no flood protection between the halves of one process
	secret, err := randomHex(32)
	if err != nil {
		b.Close()
		return nil, err
	}
	b.SetServiceSecrets(secret)
	client, err := apiclient.New("http://backend.internal", &http.Client{Transport: apiclient.InProcess(b.InternalHandler())}, secret)
	if err != nil {
		b.Close()
		return nil, err
	}
	f, err := NewFrontend(client)
	if err != nil {
		b.Close()
		return nil, err
	}
	f.unlimited = true
	f.SetAPIHandler(b.apiHandler())
	if err := f.RefreshConfig(context.Background()); err != nil {
		f.Close()
		b.Close()
		return nil, err
	}
	// A setting changed in the backend (the Admin page, a test) reaches the frontend at once.
	b.onSettings = func() {
		if err := f.RefreshConfig(context.Background()); err != nil {
			log.Error().Err(err).Msg("could not pass the changed settings to the frontend")
		}
	}
	return &Server{Frontend: f, Backend: b}, nil
}

// SetVersion sets the version both halves report.
func (s *Server) SetVersion(v string) {
	s.Frontend.Version, s.Backend.Version = v, v
}

// Close stops the background jobs of both halves and the HTTPS and HTTP/3 servers. Call it after
// the HTTP server has been shut down.
func (s *Server) Close() {
	s.Backend.Close()
	s.Frontend.Close()
}

// cfg returns the backend's settings in force.
func (s *Server) cfg() *liveSettings { return s.Backend.cfg() }

// swapSettings puts settings in force in the backend, which passes them on to the frontend.
func (s *Server) swapSettings(l *liveSettings) { s.Backend.swapSettings(l) }

// WrapListener wraps the listener of the pages.
func (s *Server) WrapListener(ln net.Listener) net.Listener { return s.Frontend.WrapListener(ln) }

func (s *Server) proxyHeaders(next http.Handler) http.Handler { return s.Frontend.proxyHeaders(next) }

// assetURL returns the cache-busting URL of an embedded static file.
func (s *Frontend) assetURL(name string) string {
	if v, ok := s.assetVer[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// Handler returns the root HTTP handler.
func (s *Frontend) Handler() http.Handler {
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

	s.integrationPageRoutes(mux)
	s.exportPageRoutes(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	s.frontendAPIRoutes(mux)
	// Same-origin checks protect the cookie-authenticated forms. The Apple callback arrives as a
	// cross-site POST and is protected by its signed state instead.
	// The check is rebuilt when the base URL changes, so it is looked up for every request.
	guarded := s.withVisitor(s.frontAuth(mux))
	sameOrigin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.cop.Load().Handler(guarded).ServeHTTP(w, r)
	})
	var h http.Handler = s.altSvc(edge.SecureHeaders(edge.Compress(edge.LimitBody(sameOrigin))))
	if limitFrontend && !s.unlimited { // shed floods here, before they become calls to the backend
		h = s.frontendLimiter()(h)
	}
	return edge.LogRequests(s.proxyHeaders(h))
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

func (s *Frontend) render(w http.ResponseWriter, page string, status int, data pageData) {
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

func (s *Frontend) fail(w http.ResponseWriter, err error) {
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

func (s *Frontend) home(w http.ResponseWriter, r *http.Request, u models.User) {
	http.Redirect(w, r, userPath(u.ID, "estimates"), http.StatusSeeOther)
}
