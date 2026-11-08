package estimate

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func f(v float64) *float64 { return &v }

func d(m time.Month, day int) time.Time { return time.Date(2026, m, day, 0, 0, 0, 0, time.UTC) }

func scaleW(t time.Time, w float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: t.Add(8 * time.Hour), WeightKg: f(w), Method: models.MethodScale}
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
	r := Compute(nil, rs, Options{})
	if len(r.Days) != 1 {
		t.Fatalf("days: %d", len(r.Days))
	}
	v := r.Days[0].Values
	if !near(v["weight_kg"].V, 81) || !near(v["fat_ratio_pct"].V, 20) || v["weight_kg"].Origin != OriginMeasured {
		t.Fatalf("%+v", v)
	}
	if v["weight_kg"].Method != models.MethodScale || v["weight_kg"].Raw == nil || !near(*v["weight_kg"].Raw, 81) {
		t.Fatalf("%+v", v["weight_kg"])
	}
	if _, ok := v["fat_mass_kg"]; ok {
		t.Fatal("unexpected field")
	}
}

func TestInputsAreNotModified(t *testing.T) {
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 4), 83)}
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 2), WeightKg: f(78)}}
	rsCopy := append([]models.ScaleReading(nil), rs...)
	scansCopy := append([]models.DexaScan(nil), scans...)
	Compute(scans, rs, Options{Today: d(1, 10)})
	if !reflect.DeepEqual(rs, rsCopy) || !reflect.DeepEqual(scans, scansCopy) {
		t.Fatal("inputs were modified")
	}
}

func TestMostAccurateMethodWinsPerField(t *testing.T) {
	rs := []models.ScaleReading{
		{MeasuredAt: d(1, 1).Add(8 * time.Hour), Method: models.MethodScale, WeightKg: f(82), FatRatioPct: f(20)},
		{MeasuredAt: d(1, 1).Add(9 * time.Hour), Method: models.MethodCaliper, FatRatioPct: f(15)},
	}
	v := find(t, Compute(nil, rs, Options{}), d(1, 1)).Values
	if got := v["fat_ratio_pct"]; !near(got.V, 15) || got.Method != models.MethodCaliper {
		t.Errorf("fat: %+v", got)
	}
	// caliper has no weight, so the scale still provides it
	if got := v["weight_kg"]; !near(got.V, 82) || got.Method != models.MethodScale {
		t.Errorf("weight: %+v", got)
	}
}

func TestCustomRanking(t *testing.T) {
	rs := []models.ScaleReading{
		{MeasuredAt: d(1, 1).Add(8 * time.Hour), Method: models.MethodScale, FatRatioPct: f(20)},
		{MeasuredAt: d(1, 1).Add(9 * time.Hour), Method: models.MethodVisualAI, FatRatioPct: f(18)},
	}
	got := find(t, Compute(nil, rs, Options{Ranking: []string{models.MethodVisualAI}}), d(1, 1)).Values["fat_ratio_pct"]
	if !near(got.V, 18) || got.Method != models.MethodVisualAI {
		t.Fatalf("%+v", got)
	}
}

func TestSameMethodTieFollowsAthleteDefault(t *testing.T) {
	rs := []models.ScaleReading{
		{MeasuredAt: d(1, 1).Add(8 * time.Hour), Method: models.MethodScale, WeightKg: f(80)},
		{MeasuredAt: d(1, 1).Add(9 * time.Hour), Method: models.MethodScale, AthleteMode: true, WeightKg: f(82)},
	}
	if got := find(t, Compute(nil, rs, Options{AthleteDefault: true}), d(1, 1)).Values["weight_kg"]; !near(got.V, 82) || !got.AthleteMode {
		t.Errorf("athlete default: %+v", got)
	}
	if got := find(t, Compute(nil, rs, Options{}), d(1, 1)).Values["weight_kg"]; !near(got.V, 80) || got.AthleteMode {
		t.Errorf("normal default: %+v", got)
	}
}

func TestFillsGapsAndHoldsLastValueUntilToday(t *testing.T) {
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 5), 84)}
	r := Compute(nil, rs, Options{Today: d(1, 8)})
	if len(r.Days) != 8 {
		t.Fatalf("days: %d", len(r.Days))
	}
	for _, c := range []struct {
		day int
		org Origin
	}{
		{1, OriginMeasured}, {2, OriginInterpolated}, {3, OriginInterpolated}, {4, OriginInterpolated},
		{5, OriginMeasured}, {6, OriginHeld}, {7, OriginHeld}, {8, OriginHeld},
	} {
		got := find(t, r, d(1, c.day)).Values["weight_kg"]
		if got.Origin != c.org {
			t.Errorf("day %d: origin %s want %s", c.day, got.Origin, c.org)
		}
		if (c.org == OriginMeasured) != (got.Raw != nil) {
			t.Errorf("day %d: raw %v", c.day, got.Raw)
		}
	}
	if got := find(t, r, d(1, 8)).Values["weight_kg"]; !near(got.V, 84) {
		t.Errorf("held value: %+v", got)
	}
}

func TestInterpolationIsLinearBeforeSmoothing(t *testing.T) {
	// a long straight ramp is unchanged by a centred average away from the ends
	var rs []models.ScaleReading
	for i := 1; i <= 21; i += 4 {
		rs = append(rs, scaleW(d(1, i), 70+float64(i-1)/4))
	}
	r := Compute(nil, rs, Options{})
	for i := 5; i <= 17; i++ {
		got := find(t, r, d(1, i)).Values["weight_kg"].V
		if want := 70 + float64(i-1)/4; !near(got, want) {
			t.Errorf("day %d: got %v want %v", i, got, want)
		}
	}
}

func TestPeaksAreSmoothedNotCapped(t *testing.T) {
	var rs []models.ScaleReading
	for i := 1; i <= 7; i++ {
		w := 80.0
		if i == 4 {
			w = 90 // a +10 kg spike would have been clamped to +1 kg before
		}
		rs = append(rs, scaleW(d(1, i), w))
	}
	got := find(t, Compute(nil, rs, Options{}), d(1, 4)).Values["weight_kg"]
	if !near(got.V, 570.0/7) {
		t.Fatalf("centred 7-day mean: %+v", got)
	}
	if got.Raw == nil || !near(*got.Raw, 90) {
		t.Fatalf("raw reading must stay visible: %+v", got.Raw)
	}
}

func TestBlendBetweenCalibrations(t *testing.T) {
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)},  // series 82 -> offset -2
		{ID: 2, MeasuredAt: d(1, 11), WeightKg: f(83)}, // series 82 -> offset +1
	}
	var rs []models.ScaleReading
	for t0 := time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC); !t0.After(d(1, 16)); t0 = t0.Add(24 * time.Hour) {
		rs = append(rs, scaleW(t0, 82))
	}
	r := Compute(scans, rs, Options{})
	if len(r.Calibrations) != 2 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	for _, c := range []struct {
		date time.Time
		want float64
		corr bool
	}{
		{time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC), 82, false}, // before the first calibration
		{d(1, 3), 82 + (1-weight(0.2))*-2 + weight(0.2)*1, true},
		{d(1, 6), 82 + 0.5*-2 + 0.5*1, true}, // midpoint: plain average of offsets
		{d(1, 9), 82 + (1-weight(0.8))*-2 + weight(0.8)*1, true},
		{d(1, 16), 82 + 1, true}, // after the last calibration
	} {
		got := find(t, r, c.date).Values["weight_kg"]
		if !near(got.V, c.want) || got.Corrected != c.corr {
			t.Errorf("%v: got %+v want %v corrected=%v", c.date, got, c.want, c.corr)
		}
	}
	for _, c := range []struct {
		date time.Time
		want float64
	}{{d(1, 1), 80}, {d(1, 11), 83}} {
		if got := find(t, r, c.date).Values["weight_kg"]; got.Origin != OriginDexa || !near(got.V, c.want) {
			t.Errorf("%v: %+v", c.date, got)
		}
	}
	// smoothstep: 25% into the gap is closer to the first offset than a linear blend would be
	if !(weight(0.25) < 0.25 && weight(0.75) > 0.75 && near(weight(0.5), 0.5)) {
		t.Fatal("weight curve is not smoothstep")
	}
}

func TestAthleteAndNormalReadingsAreCalibratedSeparately(t *testing.T) {
	var rs []models.ScaleReading
	for i := 1; i <= 9; i++ { // normal mode period
		rs = append(rs, scaleW(d(1, i), 80))
	}
	for i := 12; i <= 20; i++ { // athlete mode period
		r := scaleW(d(1, i), 82)
		r.AthleteMode = true
		rs = append(rs, r)
	}
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 4), WeightKg: f(78)},  // normal group: offset -2
		{ID: 2, MeasuredAt: d(1, 16), WeightKg: f(81)}, // athlete group: offset -1
	}
	r := Compute(scans, rs, Options{})
	if len(r.Calibrations) != 2 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if c := r.Calibrations[0]; c.AthleteMode || !near(c.Offsets["weight_kg"], -2) {
		t.Errorf("normal calibration: %+v", c)
	}
	if c := r.Calibrations[1]; !c.AthleteMode || !near(c.Offsets["weight_kg"], -1) {
		t.Errorf("athlete calibration: %+v", c)
	}
	if got := find(t, r, d(1, 6)).Values["weight_kg"]; !near(got.V, 78) || got.AthleteMode {
		t.Errorf("normal day: %+v", got)
	}
	if got := find(t, r, d(1, 18)).Values["weight_kg"]; !near(got.V, 81) || !got.AthleteMode {
		t.Errorf("athlete day: %+v", got)
	}
}

func TestDexaBeforeFirstReadingMeansNoCalibration(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}
	r := Compute(scans, []models.ScaleReading{scaleW(d(1, 20), 85)}, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 20)).Values["weight_kg"]; got.Corrected || !near(got.V, 85) {
		t.Fatalf("%+v", got)
	}
	if got := find(t, r, d(1, 1)).Values["weight_kg"]; got.Origin != OriginDexa {
		t.Fatalf("%+v", got)
	}
}

func TestDexaDayFillsScaleOnlyFields(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}
	rs := []models.ScaleReading{{MeasuredAt: d(1, 1).Add(8 * time.Hour), WeightKg: f(82), MuscleMassKg: f(30)}}
	v := find(t, Compute(scans, rs, Options{}), d(1, 1)).Values
	if v["weight_kg"].Origin != OriginDexa || !near(v["weight_kg"].V, 80) {
		t.Fatalf("%+v", v["weight_kg"])
	}
	if v["muscle_mass_kg"].Origin != OriginMeasured || !near(v["muscle_mass_kg"].V, 30) || v["muscle_mass_kg"].Corrected {
		t.Fatalf("%+v", v["muscle_mass_kg"])
	}
}
