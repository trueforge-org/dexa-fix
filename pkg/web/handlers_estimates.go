package web

import (
	"fmt"
	"net/http"
	"time"

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
}

type calRow struct {
	Date  string
	Group string
	Items []string
}

// computeEstimates loads a user's data and runs the estimator.
func (s *Server) computeEstimates(r *http.Request, u models.User) (estimate.Result, error) {
	scans, err := s.store.ListDexaScans(r.Context(), u.ID)
	if err != nil {
		return estimate.Result{}, err
	}
	readings, err := s.store.ListScaleReadings(r.Context(), u.ID)
	if err != nil {
		return estimate.Result{}, err
	}
	return estimate.Compute(scans, readings, estimate.Options{
		Today:          time.Now().UTC(),
		Ranking:        u.MethodRanking,
		AthleteDefault: u.AthleteDefault,
	}), nil
}

func (s *Server) estimatesPage(w http.ResponseWriter, r *http.Request, u models.User) {
	res, err := s.computeEstimates(r, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows := make([]estRow, 0, len(res.Days))
	for _, d := range res.Days {
		er := estRow{Date: d.Date.Format(dateLayout)}
		for _, f := range estimate.Fields {
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
	msg := r.URL.Query().Get("msg")
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	data := pageData{
		Title: u.Name + " · estimates", Tab: "estimates", User: &u,
		EstFields: estimate.Fields, EstRows: rows, Cals: cals, Msg: msg,
	}
	if p, ok := s.oauthProvider(providerWithings); ok {
		data.Withings = &integrationView{Connected: p.Connected(r.Context(), s.store, u.ID)}
		data.Resync = data.Withings.Connected
	}
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
