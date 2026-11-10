// Package frontend serves the HTML pages (Go templates + htmx). It has no database: everything else it asks of the backend through the internal API.
package frontend

import (
	"bytes"
	"embed"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/estimate"
	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

//go:embed templates static
var assets embed.FS

var pages = []string{"index.html", "dexa.html", "scale.html", "estimates.html", "settings.html", "admin.html", "adminworkers.html", "status.html", "docs.html", "twofactor.html", "reset.html"}

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
	s.statusRoutes(mux)
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
	if !s.unlimited { // shed floods here, before they become calls to the backend
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

	SLSend        bool                   // estimates page: show the button that sends one day to Strength Level
	StrengthLevel *slView                // settings page: only for users an administrator has enabled it for
	Security      *securityView          // settings page: password, tokens and linked accounts
	Workers       *apiclient.WorkersPage // the workers page of the Admin section
	Status        *statusView            // the status page
	Admin         *adminView             // admin page
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
		http.Error(w, "DexaFix is temporarily unavailable, try again in a moment", http.StatusServiceUnavailable)
	case errors.As(err, &apiErr):
		switch apiErr.Status {
		case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
			http.Error(w, apiErr.Message, apiErr.Status)
		case http.StatusInternalServerError:
			http.Error(w, "internal error", http.StatusInternalServerError)
		default:
			http.Error(w, apiErr.Message, http.StatusBadRequest)
		}
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
