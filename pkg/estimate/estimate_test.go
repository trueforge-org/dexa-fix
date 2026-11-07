package estimate

import (
	"math"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func f(v float64) *float64 { return &v }

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

func scaleW(t time.Time, w float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: t.Add(8 * time.Hour), WeightKg: f(w)}
}

func find(t *testing.T, r Result, date time.Time) DayEstimate {
	t.Helper()
	for _, e := range r.Days {
		if e.Date.Equal(date) {
			return e
		}
	}
	t.Fatalf("no estimate for %v", date)
	return DayEstimate{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAveragesSameDayAndIgnoresNil(t *testing.T) {
	rs := []models.ScaleReading{
		{MeasuredAt: d(1, 5).Add(7 * time.Hour), WeightKg: f(80), FatRatioPct: f(20)},
		{MeasuredAt: d(1, 5).Add(20 * time.Hour), WeightKg: f(82)},
	}
	r := Compute(nil, rs)
	if len(r.Days) != 1 {
		t.Fatalf("days: %d", len(r.Days))
	}
	v := r.Days[0].Values
	if !near(v["weight_kg"].V, 81) || !near(v["fat_ratio_pct"].V, 20) || v["weight_kg"].Origin != OriginScale {
		t.Fatalf("%+v", v)
	}
	if _, ok := v["fat_mass_kg"]; ok {
		t.Fatal("unexpected field")
	}
}

func TestBlendBetweenCalibrations(t *testing.T) {
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)},  // scale 82 -> offset -2
		{ID: 2, MeasuredAt: d(1, 11), WeightKg: f(78)}, // scale 77 -> offset +1
	}
	rs := []models.ScaleReading{
		scaleW(time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC), 83), // before first calibration
		scaleW(d(1, 1), 82),
		scaleW(d(1, 3), 81), // t=0.2
		scaleW(d(1, 6), 80), // t=0.5
		scaleW(d(1, 9), 79), // t=0.8
		scaleW(d(1, 11), 77),
		scaleW(d(1, 16), 76), // after last calibration
	}
	r := Compute(scans, rs)
	if len(r.Calibrations) != 2 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	for _, c := range []struct {
		date time.Time
		want float64
		org  Origin
	}{
		{d(1, 1), 80, OriginDexa},
		{d(1, 11), 78, OriginDexa},
		{time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC), 83, OriginScale},
		{d(1, 3), 81 + (1-weight(0.2))*-2 + weight(0.2)*1, OriginScaleCorrected},
		{d(1, 6), 80 + 0.5*-2 + 0.5*1, OriginScaleCorrected}, // midpoint: plain average of offsets
		{d(1, 9), 79 + (1-weight(0.8))*-2 + weight(0.8)*1, OriginScaleCorrected},
		{d(1, 16), 76 + 1, OriginScaleCorrected},
	} {
		got := find(t, r, c.date).Values["weight_kg"]
		if !near(got.V, c.want) || got.Origin != c.org {
			t.Errorf("%v: got %+v want %v %s", c.date, got, c.want, c.org)
		}
	}
	// smoothstep: 25% into the gap is closer to the first offset than a linear blend would be
	if !(weight(0.25) < 0.25 && weight(0.75) > 0.75 && near(weight(0.5), 0.5)) {
		t.Fatal("weight curve is not smoothstep")
	}
}

func TestNoNearbyScaleMeansNoCalibration(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}
	r := Compute(scans, []models.ScaleReading{scaleW(d(1, 20), 85)})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 20)).Values["weight_kg"]; got.Origin != OriginScale || !near(got.V, 85) {
		t.Fatalf("%+v", got)
	}
}

func TestDexaDayFillsScaleOnlyFields(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}
	rs := []models.ScaleReading{{MeasuredAt: d(1, 1).Add(8 * time.Hour), WeightKg: f(82), MuscleMassKg: f(30)}}
	v := find(t, Compute(scans, rs), d(1, 1)).Values
	if v["weight_kg"].Origin != OriginDexa || !near(v["weight_kg"].V, 80) {
		t.Fatalf("%+v", v["weight_kg"])
	}
	if v["muscle_mass_kg"].Origin != OriginScale || !near(v["muscle_mass_kg"].V, 30) {
		t.Fatalf("%+v", v["muscle_mass_kg"])
	}
}

func TestDailyChangeIsLimited(t *testing.T) {
	mk := func(day int, muscle float64) models.ScaleReading {
		return models.ScaleReading{MeasuredAt: d(1, day).Add(8 * time.Hour), MuscleMassKg: f(muscle)}
	}
	r := Compute(nil, []models.ScaleReading{mk(1, 30.0), mk(2, 31.0), mk(4, 31.0), mk(5, 30.0)})
	for _, c := range []struct {
		day     int
		want    float64
		limited bool
	}{
		{1, 30.0, false},
		{2, 30.05, true}, // +1.0 clamped to 0.05/day
		{4, 30.15, true}, // two days elapsed -> 0.10 allowed from 30.05
		{5, 30.10, true}, // downward moves are limited too
	} {
		got := find(t, r, d(1, c.day)).Values["muscle_mass_kg"]
		if !near(got.V, c.want) || got.Limited != c.limited {
			t.Errorf("day %d: got %+v want %v limited=%v", c.day, got, c.want, c.limited)
		}
	}
}

func TestLimitedValueFeedsCalibration(t *testing.T) {
	// scale jumps 80 -> 85 (limit 1 kg/day), so the DEXA day pairs with the limited 81 and the
	// offset is computed against it; the day itself still reports the DEXA value.
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 2), WeightKg: f(80)}}
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 2), 85), scaleW(d(1, 3), 81.5)}
	r := Compute(scans, rs)
	if off := r.Calibrations[0].Offsets["weight_kg"]; !near(off, -1) {
		t.Fatalf("offset: %v", off)
	}
	if got := find(t, r, d(1, 3)).Values["weight_kg"]; !near(got.V, 81.5-1) {
		t.Fatalf("%+v", got)
	}
}
