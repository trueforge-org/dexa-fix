package strengthlevel

import (
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func f(v float64) *float64 { return &v }

func at(day string, hour int) time.Time {
	t, _ := time.Parse(DayLayout, day)
	return t.Add(time.Duration(hour) * time.Hour)
}

func TestDailyEntries(t *testing.T) {
	got := DailyEntries([]models.ScaleReading{
		{MeasuredAt: at("2026-11-07", 8), WeightKg: f(82.0), FatRatioPct: f(18.0)},
		{MeasuredAt: at("2026-11-07", 20), WeightKg: f(83.0)}, // later weight, no fat
		{MeasuredAt: at("2026-11-08", 7), FatRatioPct: f(17)}, // no weight: day dropped
		{MeasuredAt: at("2026-11-06", 7), WeightKg: f(81.44)},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 days, got %+v", got)
	}
	if got[0].Day != "2026-11-07" || got[0].WeightKg != 83.0 || got[0].FatPct == nil || *got[0].FatPct != 18.0 {
		t.Fatalf("latest weight with fat of the day: %+v", got[0])
	}
	if got[1].Day != "2026-11-06" || got[1].FatPct != nil || got[1].Weight() != "81.4" || got[1].Fat() != "" {
		t.Fatalf("second day: %+v", got[1])
	}
}

func TestValidDay(t *testing.T) {
	for s, want := range map[string]bool{"2026-11-08": true, "2026-13-01": false, "2026-1-8": false, "x": false, "": false} {
		if ValidDay(s) != want {
			t.Errorf("%q", s)
		}
	}
}
