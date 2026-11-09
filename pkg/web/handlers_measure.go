package web

import (
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// ---- DEXA scans ----

func (s *Frontend) dexaPage(w http.ResponseWriter, r *http.Request, u models.User, errMsg string, status int) {
	scans, err := s.api.DexaScans(r.Context(), u.ID)
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
		Fields: dexaFields, Rows: rows, Today: time.Now().In(u.Location()).Format(dateLayout),
	})
}

func (s *Frontend) dexaCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	scan := &models.DexaScan{UserID: u.ID}
	var err error
	if scan.MeasuredAt, err = parseWhen(r.PostFormValue("measured_at"), dateLayout, time.UTC); err == nil {
		if err = parseMeasures(r, dexaFields, dexaTargets(scan)); err == nil {
			scan.Notes, err = parseNotes(r)
		}
	}
	if err != nil {
		s.dexaPage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.api.CreateDexaScan(r.Context(), u.ID, *scan); err != nil {
		if apiclient.IsCode(err, apiclient.CodeInvalid) {
			s.dexaPage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "dexa"), http.StatusSeeOther)
}

func (s *Frontend) dexaDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.api.DeleteDexaScan(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "dexa"), http.StatusSeeOther)
}

// ---- scale readings ----

func (s *Frontend) scalePage(w http.ResponseWriter, r *http.Request, u models.User, errMsg string, status int) {
	before := r.URL.Query().Get("before")
	page, err := s.api.ScalePage(r.Context(), u.ID, before)
	if err != nil {
		s.fail(w, err)
		return
	}
	pager := &pager{Paged: before != "", Next: page.Next}
	rows := make([]row, 0, len(page.Readings))
	for i := range page.Readings {
		rd := &page.Readings[i]
		rows = append(rows, row{ID: rd.ID, When: rd.MeasuredAt.In(u.Location()).Format("2006-01-02 15:04"), Cells: cells(scaleTargets(rd)), Source: rd.Source, Method: methodLabel(rd.Method), Athlete: rd.AthleteMode, Notes: rd.Notes})
	}
	s.render(w, "scale.html", status, pageData{
		Title: u.Name + " · scale readings", Tab: "scale", User: &u, Error: errMsg,
		Fields: scaleFields, Rows: rows, Now: time.Now().In(u.Location()).Format(dateTimeLayout), Timezone: u.Location().String(),
		Methods: models.Methods, AthleteDefault: u.AthleteDefault, Pager: pager,
	})
}

// scalePageSize is how many readings the scale page shows at a time.
const scalePageSize = 100

// pager links the pages of a long list.
type pager struct {
	Next  string // cursor of the next (older) page; empty on the last page
	Paged bool   // this is not the first page
}

func (s *Frontend) scaleCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	rd := models.ScaleReading{UserID: u.ID, Source: models.SourceManual, AthleteMode: r.PostFormValue("athlete_mode") != ""}
	var err error
	if rd.MeasuredAt, err = parseWhen(r.PostFormValue("measured_at"), dateTimeLayout, u.Location()); err == nil {
		if rd.Method, err = parseMethod(r); err == nil {
			if err = parseMeasures(r, scaleFields, scaleTargets(&rd)); err == nil {
				rd.Notes, err = parseNotes(r)
			}
		}
	}
	if err != nil {
		s.scalePage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := s.api.CreateScaleReading(r.Context(), u.ID, rd); err != nil {
		if apiclient.IsCode(err, apiclient.CodeInvalid) {
			s.scalePage(w, r, u, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "scale"), http.StatusSeeOther)
}

func (s *Frontend) scaleDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.api.DeleteScaleReading(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, userPath(u.ID, "scale"), http.StatusSeeOther)
}
