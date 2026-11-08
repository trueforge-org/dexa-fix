package web

import (
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
)

var overviewStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// weightDays builds n consecutive days, newest first, the way estimate.Compute returns them.
func weightDays(n int, value func(i int) estimate.Value) []estimate.DayEstimate {
	days := make([]estimate.DayEstimate, 0, n)
	for i := n - 1; i >= 0; i-- {
		days = append(days, estimate.DayEstimate{
			Date:   overviewStart.AddDate(0, 0, i),
			Values: map[string]estimate.Value{"weight_kg": value(i)},
		})
	}
	return days
}

func measured(v float64) estimate.Value {
	return estimate.Value{V: v, Origin: estimate.OriginMeasured}
}

func rising(i int) estimate.Value { return measured(80 + float64(i)*0.1) }

func TestOverviewEmpty(t *testing.T) {
	if ov := buildOverview(estimate.Result{}, 1, "", ""); ov != nil {
		t.Fatalf("want nil, got %+v", ov)
	}
}

func TestTileLatestAndDelta(t *testing.T) {
	ov := buildOverview(estimate.Result{Days: weightDays(40, rising)}, 1, "", "")
	if len(ov.Tiles) != 1 {
		t.Fatalf("tiles: %+v", ov.Tiles)
	}
	got := ov.Tiles[0]
	if got.Label != "Weight" || got.Value != "83.9" || got.Unit != "kg" || got.Origin != "Measured" {
		t.Errorf("tile: %+v", got)
	}
	if got.Arrow != "↑" || got.Delta != "3.0 kg in 30 days" {
		t.Errorf("delta: %q %q", got.Arrow, got.Delta)
	}
}

func TestTileWithoutComparison(t *testing.T) {
	ov := buildOverview(estimate.Result{Days: weightDays(10, rising)}, 1, "", "")
	if got := ov.Tiles[0]; got.Arrow != "" || got.Delta != "" {
		t.Errorf("want no delta, got %+v", got)
	}
}

func TestTileOrigin(t *testing.T) {
	tests := []struct {
		value estimate.Value
		want  string
	}{
		{estimate.Value{Origin: estimate.OriginDexa}, "DEXA scan"},
		{estimate.Value{Origin: estimate.OriginMeasured, Corrected: true}, "Measured, corrected to DEXA"},
		{estimate.Value{Origin: estimate.OriginHeld}, "Held since last reading"},
		{estimate.Value{Origin: estimate.OriginHeld, Corrected: true}, "Held, corrected to DEXA"},
	}
	for _, tt := range tests {
		if got := tileOrigin(tt.value); got != tt.want {
			t.Errorf("tileOrigin(%+v) = %q, want %q", tt.value, got, tt.want)
		}
	}
}

func TestDescribeDelta(t *testing.T) {
	if a, d := describeDelta(-1.26, "kg", 30); a != "↓" || d != "1.3 kg in 30 days" {
		t.Errorf("down: %q %q", a, d)
	}
	if a, d := describeDelta(0.01, "kg", 31); a != "→" || d != "No change in 31 days" {
		t.Errorf("flat: %q %q", a, d)
	}
}

func TestLinePathsSplitsByOriginAndGaps(t *testing.T) {
	interpolated := estimate.Value{V: 80, Origin: estimate.OriginInterpolated}
	var pts []chartPoint
	for _, p := range []struct {
		day   int
		value estimate.Value
	}{{0, measured(80)}, {1, measured(80)}, {2, interpolated}, {3, measured(80)}, {5, measured(80)}, {6, measured(80)}} {
		pts = append(pts, chartPoint{overviewStart.AddDate(0, 0, p.day), p.value})
	}
	x := func(d time.Time) float64 { return float64(daysBetween(overviewStart, d)) }
	y := func(float64) float64 { return 1 }

	solid, dashed := linePaths(pts, x, y)
	if want := "M0.0 1.0L1.0 1.0M5.0 1.0L6.0 1.0"; solid != want {
		t.Errorf("solid = %q, want %q", solid, want)
	}
	if want := "M1.0 1.0L2.0 1.0L3.0 1.0"; dashed != want {
		t.Errorf("dashed = %q, want %q", dashed, want)
	}
}

func TestFieldSeriesRange(t *testing.T) {
	days := weightDays(200, rising)
	if got := fieldSeries(days, "weight_kg", 0); len(got) != 200 {
		t.Errorf("all: %d", len(got))
	}
	got := fieldSeries(days, "weight_kg", 90)
	if len(got) != 90 {
		t.Fatalf("90 days: %d", len(got))
	}
	if want := overviewStart.AddDate(0, 0, 199); !got[89].date.Equal(want) || got[0].date.After(got[89].date) {
		t.Errorf("order or end: first %s, last %s", got[0].date, got[89].date)
	}
	if got := fieldSeries(days, "hydration_kg", 90); len(got) != 0 {
		t.Errorf("missing field: %d", len(got))
	}
}

func TestOverviewSelectionFallsBack(t *testing.T) {
	ov := buildOverview(estimate.Result{Days: weightDays(10, rising)}, 7, "nope", "nope")
	active := func(opts []chartOption) string {
		for _, o := range opts {
			if o.Active {
				return o.Label
			}
		}
		return ""
	}
	if got := active(ov.Metrics); got != "Weight" {
		t.Errorf("metric = %q", got)
	}
	if got := active(ov.Ranges); got != "90d" {
		t.Errorf("range = %q", got)
	}
	if !strings.HasPrefix(ov.Ranges[0].Href, "/u/7/estimates?") || !strings.Contains(ov.Ranges[0].Href, "metric=weight_kg") {
		t.Errorf("href = %q", ov.Ranges[0].Href)
	}
}

func TestOverviewTooFewPoints(t *testing.T) {
	ov := buildOverview(estimate.Result{Days: weightDays(1, rising)}, 1, "", "")
	if ov.Chart != nil || ov.Note == "" || len(ov.Tiles) != 1 {
		t.Errorf("overview: %+v", ov)
	}
}

func TestChartCapsHitColumns(t *testing.T) {
	ov := buildOverview(estimate.Result{Days: weightDays(1000, rising)}, 1, "", "all")
	hits := ov.Chart.Hits
	if len(hits) > maxHitColumns {
		t.Fatalf("hits: %d", len(hits))
	}
	if want := overviewStart.AddDate(0, 0, 999).Format(dateLayout); hits[len(hits)-1].Date != want {
		t.Errorf("last hit %s, want %s", hits[len(hits)-1].Date, want)
	}
	for _, h := range hits {
		if h.Left < chartLeft || h.Left+h.Width > chartRight+0.2 {
			t.Fatalf("hit outside plot: %+v", h)
		}
	}
}

func TestNiceTicksCoverRange(t *testing.T) {
	for _, r := range [][2]float64{{78.2, 83.9}, {80, 80}, {0.4, 0.9}, {0, 120}} {
		ticks, _ := niceTicks(r[0], r[1], chartYTickCount)
		if ticks[0] > r[0] || ticks[len(ticks)-1] < r[1] || len(ticks) < 2 {
			t.Errorf("niceTicks(%v) = %v", r, ticks)
		}
		if ticks[0] < 0 {
			t.Errorf("niceTicks(%v) goes negative: %v", r, ticks)
		}
	}
}

func TestInitial(t *testing.T) {
	for in, want := range map[string]string{"bob": "B", "  ann": "A", "élan": "É", "": "?"} {
		if got := initial(in); got != want {
			t.Errorf("initial(%q) = %q, want %q", in, got, want)
		}
	}
}
