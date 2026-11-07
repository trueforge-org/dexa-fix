package web

import (
	"fmt"
	"net/http"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

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
	return estimate.Compute(scans, readings), nil
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
			switch v.Origin {
			case estimate.OriginDexa:
				c.Class, c.Badge = "dexa", "DEXA"
			case estimate.OriginScaleCorrected:
				c.Class, c.Badge = "corrected", "corr."
			}
			if v.Limited {
				c.Title += ", daily change limited"
			}
			er.Cells = append(er.Cells, c)
		}
		rows = append(rows, er)
	}
	var cals []calRow
	for i := len(res.Calibrations) - 1; i >= 0; i-- {
		c := res.Calibrations[i]
		cr := calRow{Date: c.Date.Format(dateLayout)}
		for _, f := range estimate.Fields {
			if off, ok := c.Offsets[f.Key]; ok {
				cr.Items = append(cr.Items, fmt.Sprintf("%s %+.2f %s", f.Label, off, f.Unit))
			}
		}
		cals = append(cals, cr)
	}
	s.render(w, "estimates.html", http.StatusOK, pageData{
		Title: u.Name + " · estimates", Tab: "estimates", User: &u,
		EstFields: estimate.Fields, EstRows: rows, Cals: cals,
	})
}
