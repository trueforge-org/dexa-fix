package bodybackend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	// maxPage is the most a list call returns; a caller that wants more asks for the next page.
	maxPage = 10000
	// maxBatch is the most readings one call may carry. An import of a whole Withings history is
	// the largest user.
	maxBatch = 200000
	// maxSmallBody is the size limit of calls that carry one scan.
	maxSmallBody = 1 << 20
	// maxBulkBody is the size limit of calls that carry readings.
	maxBulkBody = 128 << 20
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func fail(w http.ResponseWriter, code, msg string) {
	writeJSON(w, bodyapi.StatusFor(code), bodyapi.Error{Code: code, Message: msg})
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		fail(w, bodyapi.CodeInvalid, "invalid request: "+err.Error())
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

func itemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		fail(w, bodyapi.CodeNotFound, "not found")
		return 0, false
	}
	return id, true
}

// listFilter reads the paging parameters of a list call.
func listFilter(w http.ResponseWriter, r *http.Request) (store.ListFilter, bool) {
	q := r.URL.Query()
	var f store.ListFilter
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			fail(w, bodyapi.CodeInvalid, "invalid limit")
			return f, false
		}
		f.Limit = min(n, maxPage)
	}
	if raw := q.Get("before"); raw != "" {
		c, err := store.ParseCursor(raw)
		if err != nil {
			fail(w, bodyapi.CodeInvalid, "invalid page")
			return f, false
		}
		f.After = &c
	}
	for key, dst := range map[string]*time.Time{"since": &f.Since, "until": &f.Until} {
		if raw := q.Get(key); raw != "" {
			t, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				fail(w, bodyapi.CodeInvalid, "invalid "+key)
				return f, false
			}
			*dst = t
		}
	}
	return f, true
}

func nextString(c *store.Cursor) string {
	if c == nil {
		return ""
	}
	return c.String()
}

// ---- scans ----

func (s *Service) listScans(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	f, ok := listFilter(w, r)
	if !ok {
		return
	}
	scans, next, err := s.store.ListDexaScansPage(r.Context(), c.Subject, f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(scans), "next": nextString(next)})
}

func (s *Service) createScan(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	var sc models.DexaScan
	if !decode(w, r, maxSmallBody, &sc) {
		return
	}
	sc.ID, sc.UserID = 0, c.Subject // whose scan it is comes from the token only
	if err := validDexaScan(&sc); err != nil {
		fail(w, bodyapi.CodeInvalid, err.Error())
		return
	}
	if err := s.store.CreateDexaScan(r.Context(), &sc); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Service) deleteScan(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	id, ok := itemID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteDexaScan(r.Context(), c.Subject, id); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- readings ----

func (s *Service) listReadings(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	f, ok := listFilter(w, r)
	if !ok {
		return
	}
	rs, next, err := s.store.ListScaleReadingsPage(r.Context(), c.Subject, f)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": orEmpty(rs), "next": nextString(next)})
}

func (s *Service) upsertReadings(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	var in []models.ScaleReading
	if !decode(w, r, maxBulkBody, &in) {
		return
	}
	if len(in) > maxBatch {
		fail(w, bodyapi.CodeInvalid, "batch too large")
		return
	}
	for i := range in {
		in[i].ID, in[i].UserID = 0, c.Subject
		if err := validScaleReading(&in[i]); err != nil {
			fail(w, bodyapi.CodeInvalid, fmt.Sprintf("item %d: %s", i, err))
			return
		}
	}
	release, ok := acquire(w, r, s.bulkSlots)
	if !ok {
		return
	}
	defer release()
	n, err := s.store.UpsertScaleReadings(r.Context(), c.Subject, in)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"upserted": n})
}

func (s *Service) deleteReading(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	id, ok := itemID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteScaleReading(r.Context(), c.Subject, id); err != nil {
		storeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- import ----

func (s *Service) reconcileHandler(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	if c.Params == nil {
		fail(w, bodyapi.CodeInvalid, "the token carries no user settings")
		return
	}
	var req bodyapi.ReconcileRequest
	if !decode(w, r, maxBulkBody, &req) {
		return
	}
	if req.Source == "" || len(req.Fetched) > maxBatch {
		fail(w, bodyapi.CodeInvalid, "invalid import")
		return
	}
	for i := range req.Fetched {
		f := &req.Fetched[i]
		if f.Source != req.Source || f.ExternalID == nil || *f.ExternalID == "" || f.MeasuredAt.IsZero() {
			fail(w, bodyapi.CodeInvalid, "invalid import")
			return
		}
		f.ID, f.UserID = 0, c.Subject
		if err := validScaleReading(f); err != nil {
			fail(w, bodyapi.CodeInvalid, fmt.Sprintf("item %d: %s", i, err))
			return
		}
	}
	var day time.Time
	if req.DayMode {
		var err error
		if day, err = time.Parse("2006-01-02", req.Day); err != nil {
			fail(w, bodyapi.CodeInvalid, "invalid import")
			return
		}
	}
	release, ok := acquire(w, r, s.bulkSlots)
	if !ok {
		return
	}
	defer release()
	out, err := s.reconcile(r.Context(), c.Subject, *c.Params, req, day)
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- estimates ----

func (s *Service) estimatesHandler(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	if c.Params == nil {
		fail(w, bodyapi.CodeInvalid, "the token carries no user settings")
		return
	}
	key, err := s.estimateKeyFor(r, c.Subject, *c.Params)
	if err != nil {
		storeError(w, err)
		return
	}
	etag := s.estimateETag(c.Subject, key)
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	res, err := s.estimates(r, c.Subject, *c.Params, key)
	if err != nil {
		storeError(w, err)
		return
	}
	res.Days, res.Calibrations = orEmpty(res.Days), orEmpty(res.Calibrations)
	writeJSON(w, http.StatusOK, res)
}

// ---- export, deletion, totals ----

func (s *Service) exportHandler(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	release, ok := acquire(w, r, s.bulkSlots)
	if !ok {
		return
	}
	defer release()
	var e bodyapi.Export
	var err error
	if e.DexaScans, err = s.store.ListDexaScans(r.Context(), c.Subject); err != nil {
		storeError(w, err)
		return
	}
	if e.ScaleReadings, err = s.store.ListScaleReadings(r.Context(), c.Subject); err != nil {
		storeError(w, err)
		return
	}
	e.DexaScans, e.ScaleReadings = orEmpty(e.DexaScans), orEmpty(e.ScaleReadings)
	writeJSON(w, http.StatusOK, e)
}

func (s *Service) deleteUserHandler(w http.ResponseWriter, r *http.Request, c bodyapi.Claims) {
	if err := s.store.DeleteUserBodyData(r.Context(), c.Subject); err != nil {
		storeError(w, err)
		return
	}
	s.est.drop(c.Subject)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) statsHandler(w http.ResponseWriter, r *http.Request, _ bodyapi.Claims) {
	st, err := s.store.BodyStats(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bodyapi.Stats{DexaScans: st.DexaScans, ScaleReadings: st.ScaleReadings})
}

func (s *Service) userIDsHandler(w http.ResponseWriter, r *http.Request, _ bodyapi.Claims) {
	ids, err := s.store.BodyUserIDs(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(ids))
}
