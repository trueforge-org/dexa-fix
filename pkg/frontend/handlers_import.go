package frontend

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/models"
)

const (
	maxFlashLen = 300
)

func flashRedirect(w http.ResponseWriter, r *http.Request, userID int64, msg string) {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, userPath(userID, "estimates")+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// integrationPageRoutes registers the pages of imports and the OAuth flow. Everything is per user.
func (s *Frontend) integrationPageRoutes(mux *http.ServeMux) {
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
