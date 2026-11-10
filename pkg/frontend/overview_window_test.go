package frontend

import (
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/estimate"
	"github.com/trueforge-org/dexafix/pkg/models"
)

// The estimates page asks the backend for pageDays(range) days instead of the whole history. That
// must change nothing on the page: the overview (tiles and chart) and the table built from the
// trimmed result must equal the ones built from every day.
func TestPageDaysChangeNothingOnThePage(t *testing.T) {
	today := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	start := today.AddDate(0, 0, -900)
	h := 178.0
	var rs []models.ScaleReading
	for i := 0; i < 900; i += 2 {
		w, bf := 80+float64(i%40)*0.05, 20+float64(i%50)*0.04
		r := models.ScaleReading{MeasuredAt: start.AddDate(0, 0, i).Add(8 * time.Hour), WeightKg: &w, FatRatioPct: &bf, Method: models.MethodScale}
		if i < 300 { // a field that was measured early and not since: it is held, not missing
			bone := 3.0 + float64(i%7)*0.01
			r.BoneMassKg = &bone
		}
		rs = append(rs, r)
	}
	sw, sbf := 80.5, 21.0
	scans := []models.DexaScan{{ID: 1, MeasuredAt: start.AddDate(0, 0, 100), WeightKg: &sw, FatRatioPct: &sbf}}
	full := estimate.Compute(scans, rs, estimate.Options{HeightCm: &h, Today: today})
	if len(full.Days) < 800 {
		t.Fatalf("only %d days to test with", len(full.Days))
	}

	for _, rng := range chartRanges {
		n := pageDays(rng)
		trimmed := estimate.Result{Days: estimate.WithinDays(full.Days, n), Calibrations: full.Calibrations}
		if n > 0 && len(trimmed.Days) > n {
			t.Fatalf("range %s: %d days for a window of %d", rng.Key, len(trimmed.Days), n)
		}
		if n > 0 && len(trimmed.Days) >= len(full.Days) {
			t.Fatalf("range %s: nothing was trimmed", rng.Key)
		}
		if got, want := daysInRange(trimmed.Days, rng.Days), daysInRange(full.Days, rng.Days); !reflect.DeepEqual(got, want) {
			t.Errorf("range %s: the table rows differ (%d and %d days)", rng.Key, len(got), len(want))
		}
		for _, f := range estimate.Fields {
			got, want := buildOverview(trimmed, 7, f.Key, rng.Key), buildOverview(full, 7, f.Key, rng.Key)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("range %s, metric %s: the overview differs\n got %+v\nwant %+v", rng.Key, f.Key, got, want)
			}
		}
	}
}
