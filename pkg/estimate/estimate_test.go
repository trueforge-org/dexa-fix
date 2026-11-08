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

// scaleF is a scale reading of body fat %, a field that DEXA scans correct.
func scaleF(t time.Time, v float64) models.ScaleReading {
	return models.ScaleReading{MeasuredAt: t.Add(8 * time.Hour), FatRatioPct: f(v), Method: models.MethodScale}
}

func TestWeightIsNotCorrected(t *testing.T) {
	rs := []models.ScaleReading{scaleW(d(1, 1), 82), scaleW(d(1, 2), 82)}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}, rs, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 1)).Values["weight_kg"]; got.Origin != OriginDexa || !near(got.V, 80) {
		t.Errorf("DEXA day: %+v", got)
	}
	if got := find(t, r, d(1, 2)).Values["weight_kg"]; got.Corrected || !near(got.V, 82) {
		t.Errorf("next day: %+v", got)
	}
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
	if _, ok := v["hydration_kg"]; ok {
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

func TestAverageUsesOnlyRealReadings(t *testing.T) {
	// days 4..6 are interpolated towards 100 and must not pull the window of day 3 upwards
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 2), 82), scaleW(d(1, 3), 84), scaleW(d(1, 20), 100)}
	if got := find(t, Compute(nil, rs, Options{}), d(1, 3)).Values["weight_kg"]; !near(got.V, 82) {
		t.Fatalf("%+v", got)
	}
}

func TestTooFewReadingsKeepTheReading(t *testing.T) {
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 3), 90), scaleW(d(1, 20), 70)}
	r := Compute(nil, rs, Options{})
	if got := find(t, r, d(1, 3)).Values["weight_kg"]; !near(got.V, 90) {
		t.Fatalf("two readings in the window must not be averaged: %+v", got)
	}
	if got := find(t, r, d(1, 2)).Values["weight_kg"]; !near(got.V, 85) || got.Origin != OriginInterpolated {
		t.Fatalf("%+v", got)
	}
}

func TestInterpolatesBetweenAverages(t *testing.T) {
	var rs []models.ScaleReading
	for _, i := range []int{1, 2, 3} {
		rs = append(rs, scaleW(d(1, i), 80))
	}
	for _, i := range []int{15, 16, 17} {
		rs = append(rs, scaleW(d(1, i), 90))
	}
	r := Compute(nil, rs, Options{})
	// day 4 still has three readings in its window and day 14 too; the days between are a line
	for _, c := range []struct {
		day  int
		want float64
	}{{4, 80}, {9, 85}, {14, 90}} {
		if got := find(t, r, d(1, c.day)).Values["weight_kg"]; !near(got.V, c.want) || got.Origin != OriginInterpolated {
			t.Errorf("day %d: got %+v want %v", c.day, got, c.want)
		}
	}
}

func TestHoldsTheLastAverage(t *testing.T) {
	rs := []models.ScaleReading{scaleW(d(1, 1), 80), scaleW(d(1, 2), 81), scaleW(d(1, 3), 82)}
	got := find(t, Compute(nil, rs, Options{Today: d(1, 10)}), d(1, 10)).Values["weight_kg"]
	if !near(got.V, 81) || got.Origin != OriginHeld {
		t.Fatalf("%+v", got)
	}
}

func TestSparseReadingsInterpolateLinearly(t *testing.T) {
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
		{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(80)},  // series 82 -> offset -2
		{ID: 2, MeasuredAt: d(1, 11), FatRatioPct: f(83)}, // series 82 -> offset +1
	}
	var rs []models.ScaleReading
	for t0 := time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC); !t0.After(d(1, 16)); t0 = t0.Add(24 * time.Hour) {
		rs = append(rs, scaleF(t0, 82))
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
		{time.Date(2025, 12, 28, 0, 0, 0, 0, time.UTC), 82 - 2, true}, // before the first calibration: its offset
		{d(1, 3), 82 + (1-weight(0.2))*-2 + weight(0.2)*1, true},
		{d(1, 6), 82 + 0.5*-2 + 0.5*1, true}, // midpoint: plain average of offsets
		{d(1, 9), 82 + (1-weight(0.8))*-2 + weight(0.8)*1, true},
		{d(1, 16), 82 + 1, true}, // after the last calibration
	} {
		got := find(t, r, c.date).Values["fat_ratio_pct"]
		if !near(got.V, c.want) || got.Corrected != c.corr {
			t.Errorf("%v: got %+v want %v corrected=%v", c.date, got, c.want, c.corr)
		}
	}
	for _, c := range []struct {
		date time.Time
		want float64
	}{{d(1, 1), 80}, {d(1, 11), 83}} {
		if got := find(t, r, c.date).Values["fat_ratio_pct"]; got.Origin != OriginDexa || !near(got.V, c.want) {
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
		rs = append(rs, scaleF(d(1, i), 80))
	}
	for i := 12; i <= 20; i++ { // athlete mode period
		r := scaleF(d(1, i), 82)
		r.AthleteMode = true
		rs = append(rs, r)
	}
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 4), FatRatioPct: f(78)},  // normal group: offset -2
		{ID: 2, MeasuredAt: d(1, 16), FatRatioPct: f(81)}, // athlete group: offset -1
	}
	r := Compute(scans, rs, Options{})
	if len(r.Calibrations) != 2 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if c := r.Calibrations[0]; c.AthleteMode || !near(c.Offsets["fat_ratio_pct"], -2) {
		t.Errorf("normal calibration: %+v", c)
	}
	if c := r.Calibrations[1]; !c.AthleteMode || !near(c.Offsets["fat_ratio_pct"], -1) {
		t.Errorf("athlete calibration: %+v", c)
	}
	if got := find(t, r, d(1, 6)).Values["fat_ratio_pct"]; !near(got.V, 78) || got.AthleteMode {
		t.Errorf("normal day: %+v", got)
	}
	if got := find(t, r, d(1, 18)).Values["fat_ratio_pct"]; !near(got.V, 81) || !got.AthleteMode {
		t.Errorf("athlete day: %+v", got)
	}
}

func TestOffsetUsesTheActualReadingNotTheSmoothedSeries(t *testing.T) {
	var rs []models.ScaleReading
	for i := 1; i <= 7; i++ {
		w := 80.0
		if i == 4 {
			w = 90
		}
		rs = append(rs, scaleF(d(1, i), w))
	}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 4), FatRatioPct: f(85)}}, rs, Options{})
	if len(r.Calibrations) != 1 || !near(r.Calibrations[0].Offsets["fat_ratio_pct"], -5) {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	// the offset is still applied to the smoothed series: days 2..7 average 490/6
	if got := find(t, r, d(1, 5)).Values["fat_ratio_pct"]; !got.Corrected || !near(got.V, 490.0/6-5) {
		t.Fatalf("%+v", got)
	}
}

func TestNoCalibrationWithoutReadingOnScanDay(t *testing.T) {
	rs := []models.ScaleReading{scaleF(d(1, 1), 80), scaleF(d(1, 3), 80)}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 2), FatRatioPct: f(78)}}, rs, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 3)).Values["fat_ratio_pct"]; got.Corrected || !near(got.V, 80) {
		t.Fatalf("%+v", got)
	}
}

func TestEveryGroupMeasuredOnScanDayGetsItsOwnOffset(t *testing.T) {
	athlete := func(day time.Time, w float64) models.ScaleReading {
		r := scaleF(day, w)
		r.AthleteMode = true
		return r
	}
	rs := []models.ScaleReading{
		scaleF(d(1, 1), 80), athlete(d(1, 1), 83),
		scaleF(d(1, 11), 80), // the athlete group has no reading on the second scan day
		athlete(d(1, 21), 83),
	}
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(78)},
		{ID: 2, MeasuredAt: d(1, 11), FatRatioPct: f(79)},
	}
	r := Compute(scans, rs, Options{})
	if len(r.Calibrations) != 3 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	for i, c := range []struct {
		date    time.Time
		athlete bool
		want    float64
	}{{d(1, 1), false, -2}, {d(1, 1), true, -5}, {d(1, 11), false, -1}} {
		got := r.Calibrations[i]
		if !got.Date.Equal(c.date) || got.AthleteMode != c.athlete || !near(got.Offsets["fat_ratio_pct"], c.want) {
			t.Errorf("calibration %d: %+v", i, got)
		}
	}
}

func TestDexaBeforeFirstReadingMeansNoCalibration(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(80)}}
	r := Compute(scans, []models.ScaleReading{scaleF(d(1, 20), 85)}, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 20)).Values["fat_ratio_pct"]; got.Corrected || !near(got.V, 85) {
		t.Fatalf("%+v", got)
	}
	if got := find(t, r, d(1, 1)).Values["fat_ratio_pct"]; got.Origin != OriginDexa {
		t.Fatalf("%+v", got)
	}
}

func TestDexaDayFillsScaleOnlyFields(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(80)}}
	rs := []models.ScaleReading{{MeasuredAt: d(1, 1).Add(8 * time.Hour), WeightKg: f(82), HydrationKg: f(30)}}
	v := find(t, Compute(scans, rs, Options{}), d(1, 1)).Values
	if v["weight_kg"].Origin != OriginDexa || !near(v["weight_kg"].V, 80) {
		t.Fatalf("%+v", v["weight_kg"])
	}
	if v["hydration_kg"].Origin != OriginMeasured || !near(v["hydration_kg"].V, 30) || v["hydration_kg"].Corrected {
		t.Fatalf("%+v", v["hydration_kg"])
	}
}

func bodyComp(day time.Time, weightKg, fatPct float64) models.ScaleReading {
	return models.ScaleReading{
		MeasuredAt: day.Add(8 * time.Hour), Method: models.MethodScale, WeightKg: f(weightKg), FatRatioPct: f(fatPct),
		FatMassKg: f(weightKg * fatPct / 100), FatFreeMassKg: f(weightKg * (100 - fatPct) / 100),
	}
}

func TestMassesFollowCorrectedBodyFat(t *testing.T) {
	rs := []models.ScaleReading{bodyComp(d(1, 1), 100, 20), bodyComp(d(1, 2), 100, 20)}
	rs[0].BoneMassKg, rs[1].BoneMassKg = f(3), f(3)
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18), BoneMassKg: f(2.5)}}, rs, Options{})
	if len(r.Calibrations) != 1 || len(r.Calibrations[0].Offsets) != 2 ||
		!near(r.Calibrations[0].Offsets["fat_ratio_pct"], -2) || !near(r.Calibrations[0].Offsets["bone_mass_kg"], -0.5) {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	next := find(t, r, d(1, 2)).Values
	for _, c := range []struct {
		key  string
		want float64
		corr bool
	}{
		{"fat_ratio_pct", 18, true}, {"fat_mass_kg", 18, true}, {"fat_free_mass_kg", 82, true},
		{"bone_mass_kg", 2.5, true}, {"weight_kg", 100, false},
	} {
		if got := next[c.key]; !near(got.V, c.want) || got.Corrected != c.corr {
			t.Errorf("%s: %+v", c.key, got)
		}
	}
	// the scan only gave body fat %, so on its own day the masses are calculated from it
	scanDay := find(t, r, d(1, 1)).Values
	if got := scanDay["fat_mass_kg"]; !near(got.V, 18) || !got.Corrected || got.Origin != OriginMeasured {
		t.Errorf("scan day fat mass: %+v", got)
	}
}

func TestDexaMassWinsOnItsOwnDay(t *testing.T) {
	rs := []models.ScaleReading{bodyComp(d(1, 1), 100, 20)}
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18), FatMassKg: f(17)}}
	if got := find(t, Compute(scans, rs, Options{}), d(1, 1)).Values["fat_mass_kg"]; !near(got.V, 17) || got.Origin != OriginDexa || got.Corrected {
		t.Fatalf("%+v", got)
	}
}

func TestMassesAreNotCorrectedWithoutCalibration(t *testing.T) {
	rs := []models.ScaleReading{bodyComp(d(1, 1), 100, 20)}
	if got := find(t, Compute(nil, rs, Options{}), d(1, 1)).Values["fat_mass_kg"]; !near(got.V, 20) || got.Corrected {
		t.Fatalf("%+v", got)
	}
}

func TestScanWithoutBodyFatUsesFatMassAndWeight(t *testing.T) {
	rs := []models.ScaleReading{bodyComp(d(1, 1), 100, 20), bodyComp(d(1, 2), 100, 20)}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), WeightKg: f(100), FatMassKg: f(18)}}, rs, Options{})
	if len(r.Calibrations) != 1 || !near(r.Calibrations[0].Offsets["fat_ratio_pct"], -2) {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 2)).Values["fat_ratio_pct"]; !near(got.V, 18) || !got.Corrected {
		t.Fatalf("%+v", got)
	}
}

func TestMassIsCalculatedWithoutItsOwnReading(t *testing.T) {
	rs := []models.ScaleReading{
		{MeasuredAt: d(1, 1).Add(8 * time.Hour), Method: models.MethodScale, WeightKg: f(100), FatRatioPct: f(20)},
		{MeasuredAt: d(1, 2).Add(8 * time.Hour), Method: models.MethodScale, WeightKg: f(100), FatRatioPct: f(20)},
	}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18)}}, rs, Options{})
	got := find(t, r, d(1, 2)).Values["fat_free_mass_kg"]
	if !near(got.V, 82) || !got.Corrected || got.Origin != OriginMeasured || got.Method != models.MethodScale {
		t.Fatalf("%+v", got)
	}
}

func TestFatAndFatFreeMassAddUpToWeight(t *testing.T) {
	reading := func(fatPct *float64) models.ScaleReading {
		return models.ScaleReading{
			MeasuredAt: d(1, 1).Add(8 * time.Hour), Method: models.MethodScale,
			WeightKg: f(100), FatRatioPct: fatPct, FatMassKg: f(19), FatFreeMassKg: f(85), // 19 + 85 != 100
		}
	}
	for _, c := range []struct {
		name          string
		reading       models.ScaleReading
		scans         []models.DexaScan
		fatMass, free float64
		corrected     bool
	}{
		{"body fat known, no calibration", reading(f(20)), nil, 20, 80, false},
		{"no body fat %: the fat mass is kept", reading(nil), nil, 19, 81, false},
		{"corrected body fat", reading(f(20)), []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18)}}, 18, 82, true},
		{"DEXA fat mass on its own day", reading(f(20)), []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18), FatMassKg: f(17)}}, 17, 83, true},
	} {
		v := find(t, Compute(c.scans, []models.ScaleReading{c.reading}, Options{}), d(1, 1)).Values
		if got := v["fat_mass_kg"]; !near(got.V, c.fatMass) {
			t.Errorf("%s: fat mass %+v", c.name, got)
		}
		if got := v["fat_free_mass_kg"]; !near(got.V, c.free) || got.Corrected != c.corrected {
			t.Errorf("%s: fat-free mass %+v", c.name, got)
		}
	}
}

func TestFirstOffsetAppliesBackToFirstReading(t *testing.T) {
	athlete := scaleF(d(1, 1), 30)
	athlete.AthleteMode = true
	rs := []models.ScaleReading{scaleF(d(1, 2), 20), scaleF(d(1, 10), 20), athlete}
	r := Compute([]models.DexaScan{{ID: 1, MeasuredAt: d(1, 10), FatRatioPct: f(18)}}, rs, Options{})
	if got := find(t, r, d(1, 2)).Values["fat_ratio_pct"]; !got.Corrected || !near(got.V, 18) {
		t.Errorf("before the first scan: %+v", got)
	}
	// the athlete group has no calibration of its own, so it stays as measured
	if got := find(t, r, d(1, 1)).Values["fat_ratio_pct"]; got.Corrected || !near(got.V, 30) {
		t.Errorf("group without calibration: %+v", got)
	}
}

func TestLeanMassIsFatFreeMinusBone(t *testing.T) {
	reading := func(day int) models.ScaleReading {
		r := bodyComp(d(1, day), 100, 20)
		r.BoneMassKg, r.MuscleMassKg = f(3), f(70) // the scale's muscle mass is not used
		return r
	}
	rs := []models.ScaleReading{reading(1), reading(2)}
	for _, c := range []struct {
		name      string
		scans     []models.DexaScan
		day       int
		want      float64
		corrected bool
		origin    Origin
	}{
		{"no calibration", nil, 2, 77, false, OriginMeasured},
		{"corrected body fat and bone", []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18), BoneMassKg: f(2.5)}}, 2, 79.5, true, OriginMeasured},
		{"only bone corrected", []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), BoneMassKg: f(2.5)}}, 2, 77.5, true, OriginMeasured},
		{"DEXA lean mass on its own day", []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), LeanMassKg: f(75)}}, 1, 75, false, OriginDexa},
	} {
		got := find(t, Compute(c.scans, rs, Options{}), d(1, c.day)).Values["lean_mass_kg"]
		if !near(got.V, c.want) || got.Corrected != c.corrected || got.Origin != c.origin {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestLeanMassNeedsBoneMass(t *testing.T) {
	v := find(t, Compute(nil, []models.ScaleReading{bodyComp(d(1, 1), 100, 20)}, Options{}), d(1, 1)).Values
	if got, ok := v["lean_mass_kg"]; ok {
		t.Fatalf("%+v", got)
	}
}

func TestBodyIndicesUseHeight(t *testing.T) {
	r := bodyComp(d(1, 1), 100, 20)
	r.BoneMassKg = f(3)
	rs := []models.ScaleReading{r}
	v := find(t, Compute(nil, rs, Options{HeightCm: f(200)}), d(1, 1)).Values
	for key, want := range map[string]float64{"bmi": 25, "fmi": 5, "lmi": 19.25} {
		if got := v[key]; !near(got.V, want) || got.Corrected {
			t.Errorf("%s: %+v", key, got)
		}
	}
	v = find(t, Compute(nil, rs, Options{}), d(1, 1)).Values
	for _, key := range []string{"bmi", "fmi", "lmi"} {
		if got, ok := v[key]; ok {
			t.Errorf("%s without height: %+v", key, got)
		}
	}
}

func TestAndroidGynoidRatioComesFromDexaAndIsHeld(t *testing.T) {
	var rs []models.ScaleReading
	for i := 1; i <= 8; i++ {
		rs = append(rs, scaleW(d(1, i), 80))
	}
	scans := []models.DexaScan{
		{ID: 1, MeasuredAt: d(1, 3), AndroidFatPct: f(30), GynoidFatPct: f(40)},
		{ID: 2, MeasuredAt: d(1, 6), AndroidFatPct: f(30)}, // no gynoid value: no ratio for this scan
	}
	r := Compute(scans, rs, Options{})
	if got, ok := find(t, r, d(1, 2)).Values["ag_ratio"]; ok {
		t.Errorf("before the first scan: %+v", got)
	}
	if got := find(t, r, d(1, 3)).Values["ag_ratio"]; !near(got.V, 0.75) || got.Origin != OriginDexa {
		t.Errorf("scan day: %+v", got)
	}
	for _, day := range []int{4, 6, 8} {
		if got := find(t, r, d(1, day)).Values["ag_ratio"]; !near(got.V, 0.75) || got.Origin != OriginHeld {
			t.Errorf("day %d: %+v", day, got)
		}
	}
}

func TestBodyIndicesUseEstimatedValues(t *testing.T) {
	reading := func(day int) models.ScaleReading {
		r := bodyComp(d(1, day), 100, 20)
		r.BoneMassKg = f(3)
		return r
	}
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(18), BoneMassKg: f(2.5)}}
	v := find(t, Compute(scans, []models.ScaleReading{reading(1), reading(2)}, Options{HeightCm: f(200)}), d(1, 2)).Values
	for _, c := range []struct {
		key  string
		want float64
		corr bool
	}{{"bmi", 25, false}, {"fmi", 4.5, true}, {"lmi", 19.875, true}} {
		if got := v[c.key]; !near(got.V, c.want) || got.Corrected != c.corr {
			t.Errorf("%s: %+v", c.key, got)
		}
	}
}
