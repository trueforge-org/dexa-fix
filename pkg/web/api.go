package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	maxBodyBytes  = 4 << 20
	maxBatchItems = 5000
)

// userSettings are the per-user estimation settings.
type userSettings struct {
	AthleteDefault bool     `json:"athlete_default"`
	MethodRanking  []string `json:"method_ranking"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	msg := "internal error"
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, "not found"
	case errors.Is(err, store.ErrExists):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, store.ErrInvalid):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, importer.ErrNotImplemented), errors.Is(err, importer.ErrNotConfigured):
		status, msg = http.StatusNotImplemented, err.Error()
	case errors.Is(err, importer.ErrNotConnected):
		status, msg = http.StatusConflict, err.Error()
	default:
		log.Error().Err(err).Msg("api request failed")
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		badRequest(w, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// apiUser allows a request to act only on the authenticated user's own data.
func (s *Server) apiUser(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			apiUnauthorized(w)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id != u.ID {
			apiError(w, store.ErrNotFound)
			return
		}
		h(w, r, *u)
	}
}

// apiAdmin restricts an endpoint to administrators.
func (s *Server) apiAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			apiUnauthorized(w)
			return
		}
		if !u.IsAdmin() {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator role required"})
			return
		}
		h(w, r)
	}
}

func apiUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="dexa-fix"`)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
}

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/users", s.apiAdmin(func(w http.ResponseWriter, r *http.Request) {
		us, err := s.store.ListUsers(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, orEmpty(us))
	}))
	mux.HandleFunc("POST /api/v1/users", s.apiAdmin(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string `json:"name"`
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if !decode(w, r, &in) {
			return
		}
		email, ok := parseEmail(in.Email)
		if !ok {
			badRequest(w, "a valid email is required")
			return
		}
		var (
			u   models.User
			err error
		)
		if in.Password == "" { // such a user signs in after an admin sets a password
			if u, err = s.store.CreateUser(r.Context(), in.Name); err == nil {
				if err = s.store.UpdateUserProfile(r.Context(), u.ID, u.Name, email, nil); err != nil {
					_ = s.store.DeleteUser(r.Context(), u.ID) // do not leave an account behind without its address
				}
				u.Email = email
			}
		} else {
			if msg := validatePassword(in.Password, in.Password); msg != "" {
				badRequest(w, msg)
				return
			}
			var hash string
			if hash, err = hashPassword(in.Password); err == nil {
				u, err = s.store.CreateUserWithIdentity(r.Context(), in.Name, email, models.Identity{Provider: models.ProviderLocal, Secret: hash})
			}
		}
		if err != nil {
			apiError(w, err)
			return
		}
		s.audit(r, u.ID, auditUserAdded, "api")
		writeJSON(w, http.StatusCreated, u)
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/dexa-scans", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		f, ok := listFilter(w, r)
		if !ok {
			return
		}
		scans, next, err := s.store.ListDexaScansPage(r.Context(), u.ID, f)
		if err != nil {
			apiError(w, err)
			return
		}
		setNextCursor(w, next)
		writeJSON(w, http.StatusOK, orEmpty(scans))
	}))
	mux.HandleFunc("POST /api/v1/users/{id}/dexa-scans", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		var scan models.DexaScan
		if !decode(w, r, &scan) {
			return
		}
		if scan.MeasuredAt.IsZero() {
			badRequest(w, "measured_at is required (RFC 3339)")
			return
		}
		if err := validDexaScan(&scan); err != nil {
			badRequest(w, err.Error())
			return
		}
		scan.ID, scan.UserID = 0, u.ID
		if err := s.store.CreateDexaScan(r.Context(), &scan); err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, scan)
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/scale-readings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		f, ok := listFilter(w, r)
		if !ok {
			return
		}
		rs, next, err := s.store.ListScaleReadingsPage(r.Context(), u.ID, f)
		if err != nil {
			apiError(w, err)
			return
		}
		setNextCursor(w, next)
		writeJSON(w, http.StatusOK, orEmpty(rs))
	}))
	// POST takes a batch. Readings with an external_id are upserted on (source, external_id),
	// which makes repeated imports (e.g. from Withings) idempotent.
	mux.HandleFunc("POST /api/v1/users/{id}/scale-readings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		// athlete_mode is optional: when omitted the user's default applies.
		var in []struct {
			models.ScaleReading
			AthleteMode *bool `json:"athlete_mode"`
		}
		if !decode(w, r, &in) {
			return
		}
		if len(in) > maxBatchItems {
			badRequest(w, "batch too large")
			return
		}
		batch := make([]models.ScaleReading, len(in))
		for i := range in {
			batch[i] = in[i].ScaleReading
			if err := validScaleReading(&batch[i]); err != nil {
				badRequest(w, fmt.Sprintf("item %d: %s", i, err))
				return
			}
			batch[i].AthleteMode = u.AthleteDefault
			if in[i].AthleteMode != nil {
				batch[i].AthleteMode = *in[i].AthleteMode
			}
		}
		n, err := s.store.UpsertScaleReadings(r.Context(), u.ID, batch)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"upserted": n})
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/settings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		writeJSON(w, http.StatusOK, userSettings{AthleteDefault: u.AthleteDefault, MethodRanking: u.MethodRanking})
	}))
	// PUT replaces the settings; a ranking may list only some methods, the rest follow in default order.
	mux.HandleFunc("PUT /api/v1/users/{id}/settings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		var in userSettings
		if !decode(w, r, &in) {
			return
		}
		for _, k := range in.MethodRanking {
			if !models.ValidMethod(k) {
				badRequest(w, "unknown method: "+k)
				return
			}
		}
		in.MethodRanking = models.NormalizeRanking(in.MethodRanking)
		if err := s.store.UpdateUserSettings(r.Context(), u.ID, in.AthleteDefault, in.MethodRanking); err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, in)
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/estimates", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		// The response only changes with the user's data and the day, so a client that kept an
		// earlier copy can ask whether it is still good without the server computing anything.
		etag := s.estimateETag(u.ID, u.DataVersion, time.Now().UTC().Format(dateLayout))
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		res, err := s.computeEstimates(r, u)
		if err != nil {
			apiError(w, err)
			return
		}
		res.Days, res.Calibrations = orEmpty(res.Days), orEmpty(res.Calibrations)
		writeJSON(w, http.StatusOK, res)
	}))
}

const (
	defaultListLimit = 1000
	maxListLimit     = 10000
)

// listFilter reads the paging and window parameters of a list endpoint: limit (default 1000, at
// most 10000), before (the cursor from the previous page's X-Next-Cursor header), and since and
// until (RFC 3339 or YYYY-MM-DD, both inclusive; a plain date as until covers that whole day).
func listFilter(w http.ResponseWriter, r *http.Request) (store.ListFilter, bool) {
	q := r.URL.Query()
	f := store.ListFilter{Limit: defaultListLimit}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxListLimit {
			badRequest(w, "limit must be between 1 and "+strconv.Itoa(maxListLimit))
			return f, false
		}
		f.Limit = n
	}
	if raw := q.Get("before"); raw != "" {
		c, err := store.ParseCursor(raw)
		if err != nil {
			badRequest(w, "before is not a cursor from an earlier response")
			return f, false
		}
		f.After = &c
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
		end  bool
	}{{"since", &f.Since, false}, {"until", &f.Until, true}} {
		raw := q.Get(p.name)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			d, derr := time.Parse(dateLayout, raw)
			if derr != nil {
				badRequest(w, p.name+" must be an RFC 3339 time or a YYYY-MM-DD date")
				return f, false
			}
			t = d
			if p.end {
				t = d.AddDate(0, 0, 1).Add(-time.Nanosecond)
			}
		}
		*p.dst = t
	}
	return f, true
}

// setNextCursor tells the client how to fetch the next, older page. It is absent on the last page.
func setNextCursor(w http.ResponseWriter, next *store.Cursor) {
	if next != nil {
		w.Header().Set("X-Next-Cursor", next.String())
	}
}

// etagMatches reports whether an If-None-Match header lists the tag (or is "*"). The comparison is
// weak, as the spec requires for this header.
func etagMatches(header, etag string) bool {
	weak := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, candidate := range strings.Split(header, ",") {
		if c := strings.TrimSpace(candidate); c == "*" || weak(c) == weak(etag) {
			return true
		}
	}
	return false
}
