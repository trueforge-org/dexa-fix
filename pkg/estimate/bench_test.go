package estimate

import (
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// syntheticHistory is n days of scale readings (weight, body fat %, bone mass, fat and fat-free
// mass, mixing two methods) and a DEXA scan about every 90 days.
func syntheticHistory(days int) ([]models.DexaScan, []models.ScaleReading) {
	start := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	var rs []models.ScaleReading
	var scans []models.DexaScan
	for i := range days {
		at := start.AddDate(0, 0, i)
		w, bf, bone := 80+float64(i%30)*0.05, 20+float64(i%45)*0.04, 3.0+float64(i%7)*0.001
		r := models.ScaleReading{MeasuredAt: at.Add(8 * time.Hour), WeightKg: &w, FatRatioPct: &bf, BoneMassKg: &bone, Method: models.MethodScale}
		if i%5 == 0 {
			r.Method = models.MethodCaliper
		}
		rs = append(rs, r)
		if i%90 == 45 {
			sw, sbf := w+0.3, bf+1.5
			scans = append(scans, models.DexaScan{ID: int64(len(scans) + 1), MeasuredAt: at, WeightKg: &sw, FatRatioPct: &sbf})
		}
	}
	return scans, rs
}

func BenchmarkCompute10Years(b *testing.B) {
	scans, rs := syntheticHistory(3650)
	h := 178.0
	o := Options{HeightCm: &h, Today: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	b.ReportAllocs()
	for b.Loop() {
		Compute(scans, rs, o)
	}
}

func TestSyntheticHistoryComputes(t *testing.T) {
	scans, rs := syntheticHistory(400)
	r := Compute(scans, rs, Options{})
	if len(r.Days) < 400 || len(r.Calibrations) == 0 {
		t.Fatalf("days %d, calibrations %d", len(r.Days), len(r.Calibrations))
	}
}
