package web

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
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

// columnsSave remembers the hidden columns of the daily table. The form posts the visible ones, so
// columns added later start out visible.
func (s *Frontend) columnsSave(w http.ResponseWriter, r *http.Request, u models.User) {
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
	if err := s.api.SaveHiddenColumns(r.Context(), u.ID, hidden); err != nil {
		s.fail(w, err)
		return
	}
	target := userPath(u.ID, "estimates")
	if rng := r.PostFormValue("range"); rng != "" {
		target += "?" + url.Values{"range": {rng}}.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (s *Frontend) estimatesPage(w http.ResponseWriter, r *http.Request, u models.User) {
	pg, err := s.api.EstimatesPage(r.Context(), u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	res := pg.Result
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
	data.SLSend = pg.SLReady
	data.Withings = pg.Withings
	data.Resync = data.Withings != nil && data.Withings.Connected
	s.render(w, "estimates.html", http.StatusOK, data)
}

// groupLabel names a reading group, e.g. "Smart scale (Withings, manual), athlete".
func groupLabel(method string, athlete bool) string {
	l := methodLabel(method)
	if athlete {
		l += ", athlete"
	}
	return l
}
