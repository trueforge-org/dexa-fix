package mainbackend

import (
	"net/http"
	"strconv"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Internal endpoints for the DEXA scan and scale reading pages.

func (s *Backend) internalMeasureRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/users/{id}"
	mux.HandleFunc("GET "+p+"/pages/dexa", s.internalUser(s.internalDexaList))
	mux.HandleFunc("POST "+p+"/dexa", s.internalUser(s.internalDexaCreate))
	mux.HandleFunc("DELETE "+p+"/dexa/{item}", s.internalUser(s.internalDexaDelete))
	mux.HandleFunc("GET "+p+"/pages/scale", s.internalUser(s.internalScaleList))
	mux.HandleFunc("POST "+p+"/scale", s.internalUser(s.internalScaleCreate))
	mux.HandleFunc("DELETE "+p+"/scale/{item}", s.internalUser(s.internalScaleDelete))
}

func internalItemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return 0, false
	}
	return id, true
}

func (s *Backend) internalDexaList(w http.ResponseWriter, r *http.Request, u models.User) {
	scans, err := s.store.ListDexaScans(r.Context(), u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, orEmpty(scans))
}

func (s *Backend) internalDexaCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	var sc models.DexaScan
	if !internalDecode(w, r, &sc) {
		return
	}
	sc.ID, sc.UserID = 0, u.ID
	if err := validDexaScan(&sc); err != nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, err.Error())
		return
	}
	if err := s.store.CreateDexaScan(r.Context(), &sc); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalDexaDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := internalItemID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteDexaScan(r.Context(), u.ID, id); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditScanDeleted, "")
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalScaleList(w http.ResponseWriter, r *http.Request, u models.User) {
	f := store.ListFilter{Limit: scalePageSize}
	if raw := r.URL.Query().Get("before"); raw != "" {
		c, err := store.ParseCursor(raw)
		if err != nil {
			internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid page")
			return
		}
		f.After = &c
	}
	readings, next, err := s.store.ListScaleReadingsPage(r.Context(), u.ID, f)
	if err != nil {
		internalError(w, err)
		return
	}
	page := apiclient.ScalePage{Readings: orEmpty(readings)}
	if next != nil {
		page.Next = next.String()
	}
	internalJSON(w, http.StatusOK, page)
}

func (s *Backend) internalScaleCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	var rd models.ScaleReading
	if !internalDecode(w, r, &rd) {
		return
	}
	rd.ID, rd.UserID, rd.Source, rd.ExternalID = 0, u.ID, models.SourceManual, nil
	if err := validScaleReading(&rd); err != nil {
		internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, err.Error())
		return
	}
	if _, err := s.store.UpsertScaleReadings(r.Context(), u.ID, []models.ScaleReading{rd}); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalScaleDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := internalItemID(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteScaleReading(r.Context(), u.ID, id); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditReadingDeleted, "")
	internalJSON(w, http.StatusNoContent, nil)
}
