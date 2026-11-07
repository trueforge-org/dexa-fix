package web

import (
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// ---- DEXA scans ----

func (s *Server) dexaPage(w http.ResponseWriter, r *http.Request, u models.User, errMsg string, status int) {
	scans, err := s.store.ListDexaScans(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := make([]row, 0, len(scans))
	for i := range scans {
		sc := &scans[i]
		rows = append(rows, row{ID: sc.ID, When: sc.MeasuredAt.Format(dateLayout), Cells: cells(dexaTargets(sc)), Notes: sc.Notes})
	}
	s.render(w, "dexa.html", status, pageData{
		Title: u.Name + " · DEXA scans", Tab: "dexa", User: &u, Error: errMsg,
		Fields: dexaFields, Rows: rows, Today: time.Now().UTC().Format(dateLayout),
	})
}

func (s *Server) dexaCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	scan := &models.DexaScan{UserID: u.ID}
	var err error
	if scan.MeasuredAt, err = parseWhen(r.PostFormValue("measured_at"), dateLayout); err == nil {
		if err = parseMeasures(r, dexaFields, dexaTargets(scan)); err == nil {
			scan.Notes, err = parseNotes(r)
		}
	}
	if err != nil {
		s.dexaPage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.CreateDexaScan(r.Context(), scan); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "dexa"), http.StatusSeeOther)
}

func (s *Server) dexaDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDexaScan(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "dexa"), http.StatusSeeOther)
}

// ---- scale readings ----

func (s *Server) scalePage(w http.ResponseWriter, r *http.Request, u models.User, errMsg string, status int) {
	readings, err := s.store.ListScaleReadings(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := make([]row, 0, len(readings))
	for i := range readings {
		rd := &readings[i]
		rows = append(rows, row{ID: rd.ID, When: rd.MeasuredAt.Format("2006-01-02 15:04"), Cells: cells(scaleTargets(rd)), Source: rd.Source, Notes: rd.Notes})
	}
	s.render(w, "scale.html", status, pageData{
		Title: u.Name + " · scale readings", Tab: "scale", User: &u, Error: errMsg,
		Fields: scaleFields, Rows: rows, Now: time.Now().UTC().Format(dateTimeLayout),
	})
}

func (s *Server) scaleCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	rd := models.ScaleReading{UserID: u.ID, Source: models.SourceManual}
	var err error
	if rd.MeasuredAt, err = parseWhen(r.PostFormValue("measured_at"), dateTimeLayout); err == nil {
		if err = parseMeasures(r, scaleFields, scaleTargets(&rd)); err == nil {
			rd.Notes, err = parseNotes(r)
		}
	}
	if err != nil {
		s.scalePage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if _, err := s.store.UpsertScaleReadings(r.Context(), u.ID, []models.ScaleReading{rd}); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "scale"), http.StatusSeeOther)
}

func (s *Server) scaleDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteScaleReading(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "scale"), http.StatusSeeOther)
}
