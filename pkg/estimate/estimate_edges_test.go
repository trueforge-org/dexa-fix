package estimate

import (
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func TestMethodsOutsideTheKnownListRankLastAndTieByName(t *testing.T) {
	at := d(1, 1).Add(8 * time.Hour)
	reading := func(method string, kg float64) models.ScaleReading {
		return models.ScaleReading{MeasuredAt: at, WeightKg: f(kg), Method: method}
	}
	for _, tc := range []struct {
		name string
		rs   []models.ScaleReading
		want float64
	}{
		{"a known method beats an unknown one", []models.ScaleReading{reading("legacy-a", 70), reading(models.MethodScale, 80)}, 80},
		{"of two unknown methods the first by name wins", []models.ScaleReading{reading("legacy-b", 90), reading("legacy-a", 70)}, 70},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := find(t, Compute(nil, tc.rs, Options{}), d(1, 1)).Values["weight_kg"]
			if !near(got.V, tc.want) {
				t.Fatalf("weight %v, want %v", got.V, tc.want)
			}
		})
	}
}

func TestRankerBetterIsAStrictOrder(t *testing.T) {
	rk := newRanker(Options{Ranking: []string{models.MethodVisualAI}, AthleteDefault: true})
	scale, ai := group{method: models.MethodScale}, group{method: models.MethodVisualAI}
	scaleAthlete := group{method: models.MethodScale, athlete: true}
	for _, tc := range []struct {
		name string
		a, b group
		want bool
	}{
		{"the better ranked method wins", ai, scale, true},
		{"and not the other way round", scale, ai, false},
		{"the user's default athlete mode wins within a method", scaleAthlete, scale, true},
		{"and not the other way round with it set", scale, scaleAthlete, false},
		{"a group is not better than itself", scale, scale, false},
	} {
		if got := rk.better(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: better(%+v, %+v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTwoScansOnOneDayAgreeByIDWithTheLaterOneWinningPerField(t *testing.T) {
	scans := []models.DexaScan{
		{ID: 2, MeasuredAt: d(1, 1), FatRatioPct: f(20)},
		{ID: 1, MeasuredAt: d(1, 1), FatRatioPct: f(25), WeightKg: f(80)},
	}
	r := Compute(scans, []models.ScaleReading{scaleF(d(1, 1), 22), scaleF(d(1, 2), 22)}, Options{})
	if len(r.Calibrations) != 1 || !near(r.Calibrations[0].Offsets["fat_ratio_pct"], -2) {
		t.Fatalf("the scan with the higher id must win for body fat: %+v", r.Calibrations)
	}
}

func TestAScanWithFatMassAloneCannotGiveABodyFatPercentage(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 1), FatMassKg: f(18)}}
	r := Compute(scans, []models.ScaleReading{scaleF(d(1, 1), 22), scaleF(d(1, 2), 22)}, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("no weight and no fat-free mass: nothing to calibrate with, got %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 2)).Values["fat_ratio_pct"]; got.Corrected || !near(got.V, 22) {
		t.Fatalf("%+v", got)
	}
}

func TestAFieldHasNoValueAfterItsLastReadingUnlessTodayCarriesItOn(t *testing.T) {
	rs := []models.ScaleReading{scaleF(d(1, 1), 22), scaleW(d(1, 1), 80), scaleW(d(1, 5), 81)}
	r := Compute(nil, rs, Options{})
	est := find(t, r, d(1, 5))
	if _, ok := est.Values["weight_kg"]; !ok {
		t.Fatal("the weight has a reading on the fifth")
	}
	if v, ok := est.Values["fat_ratio_pct"]; ok {
		t.Fatalf("body fat was last read on the first, got %+v", v)
	}
}

func TestAScanAfterTheLastReadingCalibratesNothing(t *testing.T) {
	scans := []models.DexaScan{{ID: 1, MeasuredAt: d(1, 10), FatRatioPct: f(18)}}
	r := Compute(scans, []models.ScaleReading{scaleF(d(1, 1), 22)}, Options{})
	if len(r.Calibrations) != 0 {
		t.Fatalf("calibrations: %+v", r.Calibrations)
	}
	if got := find(t, r, d(1, 10)).Values["fat_ratio_pct"]; got.Origin != OriginDexa || !near(got.V, 18) {
		t.Fatalf("%+v", got)
	}
}
