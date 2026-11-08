package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// integrationRoutes registers provider imports and the OAuth flow. Everything is per user.
func (s *Server) integrationRoutes(mux *http.ServeMux) {
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

	// POST /u/{id}/import/{provider} with an optional day form value (the estimates page buttons).
	mux.HandleFunc("POST /u/{id}/import/{provider}", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		imp, ok := s.importers.Get(r.PathValue("provider"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		day := r.PostFormValue("day")
		o, err := parseOptions("", day)
		if err != nil {
			flashRedirect(w, r, u.ID, err.Error())
			return
		}
		res, err := imp.Import(r.Context(), s.store, u.ID, o)
		if err != nil {
			flashRedirect(w, r, u.ID, importError(label(imp.Name()), err))
			return
		}
		s.audit(r, u.ID, auditImport, imp.Name())
		flashRedirect(w, r, u.ID, summary(label(imp.Name()), res, day))
	}))

	// OAuth: connect sends the user to the provider; the provider returns to the fixed callback URL,
	// so the user id travels in a cookie next to the anti-CSRF state.
	mux.HandleFunc("GET /u/{id}/integrations/{provider}/connect", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		name := r.PathValue("provider")
		p, ok := s.oauthProvider(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			s.fail(w, err)
			return
		}
		state := hex.EncodeToString(b)
		http.SetCookie(w, &http.Cookie{
			Name: oauthCookiePrefix + name, Value: strconv.FormatInt(u.ID, 10) + "." + state, Path: "/",
			MaxAge: int(oauthStateTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
		})
		http.Redirect(w, r, p.AuthURL(state), http.StatusSeeOther)
	}))

	mux.HandleFunc("GET /integrations/{provider}/callback", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("provider")
		p, ok := s.oauthProvider(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		cookieName := oauthCookiePrefix + name
		http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true})
		c, err := r.Cookie(cookieName)
		idStr, state, found := "", "", false
		if err == nil {
			idStr, state, found = strings.Cut(c.Value, ".")
		}
		cu := currentUser(r)
		got := r.URL.Query().Get("state")
		if !found || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(got)) != 1 {
			http.Error(w, "invalid or expired state, start the connection again", http.StatusBadRequest)
			return
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		if cu == nil || cu.ID != id { // the account must be linked by the user who started the flow
			http.Error(w, "sign in as the user who started the connection", http.StatusForbidden)
			return
		}
		u := *cu
		if e := r.URL.Query().Get("error"); e != "" {
			flashRedirect(w, r, u.ID, label(name)+" connection was not authorised.")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		if err := p.Connect(r.Context(), s.store, u.ID, code); err != nil {
			log.Error().Err(err).Str("provider", name).Msg("connect failed")
			flashRedirect(w, r, u.ID, "Could not connect "+label(name)+". The details are in the server log.")
			return
		}
		s.audit(r, u.ID, auditConnect, name)
		flashRedirect(w, r, u.ID, label(name)+" connected. Use “Import” to fetch your measurements.")
	})

	mux.HandleFunc("POST /u/{id}/integrations/{provider}/disconnect", s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
		name := r.PathValue("provider")
		p, ok := s.oauthProvider(name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := p.Disconnect(r.Context(), s.store, u.ID); err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, u.ID, auditDisconnect, name)
		flashRedirect(w, r, u.ID, label(name)+" disconnected. Imported readings were kept.")
	}))
}

// label capitalises a provider name for display.
func label(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
