package mainbackend

import (
	"net/http"
	"strconv"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/bodyapi"
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
	scans, _, err := s.body.Scans(r.Context(), u, bodyapi.Page{})
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
	if err := s.body.CreateScan(r.Context(), u, &sc); err != nil {
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
	if err := s.body.DeleteScan(r.Context(), u, id); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditScanDeleted, "")
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalScaleList(w http.ResponseWriter, r *http.Request, u models.User) {
	p := bodyapi.Page{Limit: scalePageSize}
	if raw := r.URL.Query().Get("before"); raw != "" {
		if _, err := store.ParseCursor(raw); err != nil {
			internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid page")
			return
		}
		p.Before = raw
	}
	readings, next, err := s.body.Readings(r.Context(), u, p)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, apiclient.ScalePage{Readings: orEmpty(readings), Next: next})
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
	if _, err := s.body.UpsertReadings(r.Context(), u, []models.ScaleReading{rd}); err != nil {
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
	if err := s.body.DeleteReading(r.Context(), u, id); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditReadingDeleted, "")
	internalJSON(w, http.StatusNoContent, nil)
}
