package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	oauthCookiePrefix = "oauth_state_"
	oauthStateTTL     = 10 * time.Minute
	maxFlashLen       = 300
)

// parseOptions reads the optional since (RFC 3339) and day (YYYY-MM-DD) values.
func parseOptions(since, day string) (importer.Options, error) {
	var o importer.Options
	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return o, errors.New("since must be an RFC 3339 timestamp")
		}
		o.Since = t
	}
	if day != "" {
		t, err := time.Parse(dateLayout, day)
		if err != nil {
			return o, errors.New("day must be a date like 2026-03-01")
		}
		o.Day = t
	}
	return o, nil
}

// summary renders an import result for the flash message.
func summary(provider string, res importer.Result, day string) string {
	var parts []string
	add := func(n int, what string) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, what))
		}
	}
	add(res.Imported, "imported")
	add(res.Updated, "updated")
	add(res.Removed, "removed")
	add(res.Unchanged, "unchanged")
	if res.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d day(s) skipped because they have manual readings (%s)", res.Skipped, strings.Join(res.SkippedDays, ", ")))
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing found")
	}
	scope := ""
	if day != "" {
		scope = " for " + day
	}
	return provider + scope + ": " + strings.Join(parts, ", ")
}

// importError turns an import failure into a user-facing sentence; unexpected errors are logged.
func importError(provider string, err error) string {
	switch {
	case errors.Is(err, importer.ErrNotConnected):
		return provider + " is not connected."
	case errors.Is(err, importer.ErrNotConfigured):
		return provider + " is not configured on this server."
	}
	log.Error().Err(err).Str("provider", provider).Msg("import failed")
	return provider + " import failed. Try again later; the details are in the server log."
}

func flashRedirect(w http.ResponseWriter, r *http.Request, userID int64, msg string) {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, userPath(userID, "estimates")+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// oauthProvider returns the named importer if it supports OAuth and is configured.
func (s *Server) oauthProvider(name string) (importer.OAuthProvider, bool) {
	imp, ok := s.importers.Get(name)
	if !ok {
		return nil, false
	}
	p, ok := imp.(importer.OAuthProvider)
	return p, ok && p.Configured()
}

// integrationAPIRoutes registers the public API for provider imports; the backend serves it.
func (s *Server) integrationAPIRoutes(mux *http.ServeMux) {
	// POST /api/v1/users/{id}/import/{provider}?since=<RFC 3339>&day=<YYYY-MM-DD>
	mux.HandleFunc("POST /api/v1/users/{id}/import/{provider}", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		imp, ok := s.importers.Get(r.PathValue("provider"))
		if !ok {
			apiError(w, store.ErrNotFound)
			return
		}
		o, err := parseOptions(r.URL.Query().Get("since"), r.URL.Query().Get("day"))
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		res, err := imp.Import(r.Context(), s.store, u.ID, o)
		if err != nil {
			apiError(w, err)
			return
		}
		s.audit(r, u.ID, auditImport, imp.Name())
		writeJSON(w, http.StatusOK, struct {
			Provider string `json:"provider"`
			importer.Result
		}{imp.Name(), res})
	}))
}

// integrationPageRoutes registers the pages of imports and the OAuth flow. Everything is per user.
func (s *Server) integrationPageRoutes(mux *http.ServeMux) {
	// POST /u/{id}/import/{provider} with an optional day form value (the estimates page buttons).
	mux.HandleFunc("POST /u/{id}/import/{provider}", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		f, err := s.api.Import(r.Context(), u.ID, r.PathValue("provider"), r.PostFormValue("day"))
		if err != nil {
			s.fail(w, err)
			return
		}
		flashRedirect(w, r, u.ID, f.Message)
	}))

	// OAuth: connect sends the user to the provider; the provider returns to the fixed callback URL.
	// The backend keeps the state in a cookie, which the frontend only relays.
	mux.HandleFunc("GET /u/{id}/integrations/{provider}/connect", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		target, err := s.api.ConnectIntegration(r.Context(), u.ID, r.PathValue("provider"))
		if err != nil {
			s.fail(w, err)
			return
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}))

	mux.HandleFunc("GET /integrations/{provider}/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f, err := s.api.IntegrationCallback(r.Context(), r.PathValue("provider"), q.Get("state"), q.Get("code"), q.Get("error"))
		if err != nil {
			s.fail(w, err)
			return
		}
		flashRedirect(w, r, f.UserID, f.Message)
	})

	mux.HandleFunc("POST /u/{id}/integrations/{provider}/disconnect", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		f, err := s.api.DisconnectIntegration(r.Context(), u.ID, r.PathValue("provider"))
		if err != nil {
			s.fail(w, err)
			return
		}
		flashRedirect(w, r, u.ID, f.Message)
	}))
}

// label capitalises a provider name for display.
func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
