package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	maxBodyBytes  = 4 << 20
	maxBatchItems = 5000
)

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
	case errors.Is(err, errNotImplemented):
		status, msg = http.StatusNotImplemented, err.Error()
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

// apiUser resolves {id} to a user, answering with a JSON error otherwise.
func (s *Server) apiUser(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			apiError(w, store.ErrNotFound)
			return
		}
		u, err := s.store.GetUser(r.Context(), id)
		if err != nil {
			apiError(w, err)
			return
		}
		h(w, r, u)
	}
}

var errNotImplemented = errors.New("not implemented")

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		us, err := s.store.ListUsers(r.Context())
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, orEmpty(us))
	})
	mux.HandleFunc("POST /api/v1/users", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if !decode(w, r, &in) {
			return
		}
		u, err := s.store.CreateUser(r.Context(), in.Name)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, u)
	})

	mux.HandleFunc("GET /api/v1/users/{id}/dexa-scans", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		scans, err := s.store.ListDexaScans(r.Context(), u.ID)
		if err != nil {
			apiError(w, err)
			return
		}
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
		scan.ID, scan.UserID = 0, u.ID
		if err := s.store.CreateDexaScan(r.Context(), &scan); err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, scan)
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/scale-readings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		rs, err := s.store.ListScaleReadings(r.Context(), u.ID)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, orEmpty(rs))
	}))
	// POST takes a batch. Readings with an external_id are upserted on (source, external_id),
	// which makes repeated imports (e.g. from Withings) idempotent.
	mux.HandleFunc("POST /api/v1/users/{id}/scale-readings", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		var batch []models.ScaleReading
		if !decode(w, r, &batch) {
			return
		}
		if len(batch) > maxBatchItems {
			badRequest(w, "batch too large")
			return
		}
		n, err := s.store.UpsertScaleReadings(r.Context(), u.ID, batch)
		if err != nil {
			apiError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"upserted": n})
	}))

	mux.HandleFunc("GET /api/v1/users/{id}/estimates", s.apiUser(func(w http.ResponseWriter, r *http.Request, u models.User) {
		res, err := s.computeEstimates(r, u)
		if err != nil {
			apiError(w, err)
			return
		}
		res.Days, res.Calibrations = orEmpty(res.Days), orEmpty(res.Calibrations)
		writeJSON(w, http.StatusOK, res)
	}))
}
