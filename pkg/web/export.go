package web

import (
	"io"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// The two exports answer a data subject's right of access and portability (GDPR articles 15 and
// 20): one holds the account, its settings and the other personal details kept about the person,
// the other holds everything they measured. Both are machine-readable JSON.
//
// Credentials are deliberately left out: the password hash, session and token hashes, and the
// tokens and password used to reach Withings and Strength Level are secrets that give access to
// other systems, not data about the person. The export says whether those links exist.

func (s *Frontend) exportPageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /u/{id}/export/profile", s.user(s.exportPage("profile")))
	mux.HandleFunc("GET /u/{id}/export/data", s.user(s.exportPage("data")))
}

// exportPage streams an export from the backend, with the headers that make it a download.
func (s *Frontend) exportPage(kind string) func(http.ResponseWriter, *http.Request, models.User) {
	return func(w http.ResponseWriter, r *http.Request, u models.User) {
		resp, err := s.api.Raw(r.Context(), http.MethodGet, "/users/"+strconv.FormatInt(u.ID, 10)+"/export/"+kind, nil, nil)
		if err != nil {
			s.fail(w, err)
			return
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
		case resp.StatusCode >= 500:
			s.fail(w, apiclient.ErrUnavailable)
			return
		case resp.StatusCode == http.StatusNotFound:
			http.NotFound(w, r)
			return
		default:
			http.Error(w, "export not available", resp.StatusCode)
			return
		}
		h := w.Header()
		for _, k := range []string{"Content-Type", "Content-Disposition"} {
			if v := resp.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Error().Err(err).Str("export", kind).Msg("export failed")
		}
	}
}
