package web

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// providerWithings is the importer name of the Withings integration.
const providerWithings = "withings"

type estCell struct {
	Text  string
	Class string // "", "dexa" or "corrected"
	Badge string
	Title string
}

type estRow struct {
	Date  string
	Cells []estCell
	// Interpolated is set when a value on this row lies between two readings of that field.
	Interpolated bool
}

// estColumn is one entry of the daily table's column picker.
type estColumn struct {
	Key, Label string
	Visible    bool
}

type calRow struct {
	Date  string
	Group string
	Items []string
}

// computeEstimates loads a user's data and runs the estimator.
// The result comes from the cache when the user's data has not changed since it was computed (see
// estcache.go) and is shared, so callers must not modify it.
func (s *Server) computeEstimates(r *http.Request, u models.User) (estimate.Result, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	key := estimateKey{version: u.DataVersion, day: today.Format(dateLayout)}
	return s.estCache.get(u.ID, key, func() (estimate.Result, error) {
		scans, err := s.store.ListDexaScans(r.Context(), u.ID)
		if err != nil {
			return estimate.Result{}, err
		}
		readings, err := s.store.ListScaleReadings(r.Context(), u.ID)
		if err != nil {
			return estimate.Result{}, err
		}
		return estimate.Compute(scans, readings, estimate.Options{
			Today:          today,
			Ranking:        u.MethodRanking,
			AthleteDefault: u.AthleteDefault,
			HeightCm:       u.HeightCm,
		}), nil
	})
}

// columnsSave remembers the hidden columns of the daily table. The form posts the visible ones, so
// columns added later start out visible.
func (s *Server) columnsSave(w http.ResponseWriter, r *http.Request, u models.User) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	visible := make(map[string]bool, len(r.PostForm["column"]))
	for _, key := range r.PostForm["column"] {
		visible[key] = true
	}
	var hidden []string
	for _, f := range estimate.Fields {
		if !visible[f.Key] {
			hidden = append(hidden, f.Key)
		}
	}
	if err := s.store.UpdateUserHiddenColumns(r.Context(), u.ID, hidden); err != nil {
		s.fail(w, err)
		return
	}
	target := userPath(u.ID, "estimates")
	if rng := r.PostFormValue("range"); rng != "" {
		target += "?" + url.Values{"range": {rng}}.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Server) estimatesPage(w http.ResponseWriter, r *http.Request, u models.User) {
	res, err := s.computeEstimates(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	window := selectedRange(r.URL.Query().Get("range"))
	visible := daysInRange(res.Days, window.Days)
	hidden := make(map[string]bool, len(u.HiddenColumns))
	for _, key := range u.HiddenColumns {
		hidden[key] = true
	}
	var shown []estimate.Field
	columns := make([]estColumn, 0, len(estimate.Fields))
	for _, f := range estimate.Fields {
		columns = append(columns, estColumn{Key: f.Key, Label: f.Label, Visible: !hidden[f.Key]})
		if !hidden[f.Key] {
			shown = append(shown, f)
		}
	}
	rows := make([]estRow, 0, len(visible))
	for _, d := range visible {
		er := estRow{Date: d.Date.Format(dateLayout)}
		for _, f := range shown {
			v, ok := d.Values[f.Key]
			if !ok {
				er.Cells = append(er.Cells, estCell{Text: "–"})
				continue
			}
			c := estCell{Text: fmt.Sprintf("%.2f", v.V), Title: string(v.Origin)}
			if v.Method != "" {
				c.Title += " · " + groupLabel(v.Method, v.AthleteMode)
			}
			if v.Raw != nil {
				c.Title += fmt.Sprintf(" · raw %.2f", *v.Raw)
			}
			switch {
			case v.Origin == estimate.OriginDexa:
				c.Class, c.Badge = "dexa", "DEXA"
			case v.Corrected:
				c.Class, c.Badge = "corrected", "corr."
			}
			if v.Origin == estimate.OriginInterpolated {
				er.Interpolated = true
			}
			if v.Origin == estimate.OriginInterpolated || v.Origin == estimate.OriginHeld {
				c.Class += " filled"
			}
			er.Cells = append(er.Cells, c)
		}
		rows = append(rows, er)
	}
	var cals []calRow
	for i := len(res.Calibrations) - 1; i >= 0; i-- {
		c := res.Calibrations[i]
		cr := calRow{Date: c.Date.Format(dateLayout), Group: groupLabel(c.Method, c.AthleteMode)}
		for _, f := range estimate.Fields {
			if off, ok := c.Offsets[f.Key]; ok {
				cr.Items = append(cr.Items, fmt.Sprintf("%s %+.2f %s", f.Label, off, f.Unit))
			}
		}
		cals = append(cals, cr)
	}
	msg := flashParam(r)
	data := pageData{
		Title: u.Name + " · estimates", Tab: "estimates", User: &u,
		EstFields: shown, EstColumns: columns, EstRows: rows, EstRange: window.Desc, EstRangeKey: window.Key, Cals: cals, Msg: msg,
		Overview: buildOverview(res, u.ID, r.URL.Query().Get("metric"), r.URL.Query().Get("range")),
	}
	data.SLSend = s.strengthLevelReady(r, u)
	data.Withings = s.withingsView(r, u)
	data.Resync = data.Withings != nil && data.Withings.Connected
	s.render(w, "estimates.html", http.StatusOK, data)
}

// withingsView reports the Withings state for a user, or nil if the importer is not registered.
// Unlike oauthProvider it also returns a view when the server lacks credentials, so the pages can
// say so instead of showing nothing.
func (s *Server) withingsView(r *http.Request, u models.User) *integrationView {
	imp, ok := s.importers.Get(providerWithings)
	if !ok {
		return nil
	}
	p, ok := imp.(importer.OAuthProvider)
	if !ok {
		return nil
	}
	v := &integrationView{Configured: p.Configured()}
	v.Connected = v.Configured && p.Connected(r.Context(), s.store, u.ID)
	return v
}

// groupLabel names a reading group, e.g. "Smart scale (Withings, manual), athlete".
func groupLabel(method string, athlete bool) string {
	l := methodLabel(method)
	if athlete {
		l += ", athlete"
	}
	return l
}
