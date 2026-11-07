package web

import (
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// integrationRoutes registers the provider import endpoint and the OAuth placeholders a future
// Withings integration will use. All of them are per user.
func (s *Server) integrationRoutes(mux *http.ServeMux) {
	// POST /api/v1/users/{id}/import/{provider}?since=<RFC 3339>
	mux.HandleFunc("POST /api/v1/users/{id}/import/{provider}", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		imp, ok := s.importers.Get(r.PathValue("provider"))
		if !ok {
			apiError(w, store.ErrNotFound)
			return
		}
		var since time.Time
		if raw := r.URL.Query().Get("since"); raw != "" {
			var err error
			if since, err = time.Parse(time.RFC3339, raw); err != nil {
				badRequest(w, "since must be an RFC 3339 timestamp")
				return
			}
		}
		n, err := imp.Import(r.Context(), s.store, u.ID, since)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"provider": imp.Name(), "imported": n})
	}))

	// OAuth flow placeholders: connect starts authorization, callback receives the code.
	for _, step := range []string{"connect", "callback"} {
		mux.HandleFunc("GET /u/{id}/integrations/{provider}/"+step, s.user(func(w http.ResponseWriter, r *http.Request, u models.User) {
			if _, ok := s.importers.Get(r.PathValue("provider")); !ok {
				http.NotFound(w, r)
				return
			}
			http.Error(w, importer.ErrNotImplemented.Error(), http.StatusNotImplemented)
		}))
	}
}
