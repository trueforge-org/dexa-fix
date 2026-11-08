package strengthlevel

import (
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
)

func day(s string) time.Time { t, _ := time.Parse(DayLayout, s); return t }

func TestFromEstimates(t *testing.T) {
	v := func(x float64, o estimate.Origin) estimate.Value { return estimate.Value{V: x, Origin: o} }
	got := FromEstimates([]estimate.DayEstimate{
		{Date: day("2026-11-06"), Values: map[string]estimate.Value{"weight_kg": v(81, estimate.OriginMeasured), "fat_ratio_pct": v(18.2, estimate.OriginMeasured)}},
		{Date: day("2026-11-08"), Values: map[string]estimate.Value{"weight_kg": v(82, estimate.OriginHeld)}},
		{Date: day("2026-11-07"), Values: map[string]estimate.Value{"weight_kg": v(81.5, estimate.OriginInterpolated), "fat_ratio_pct": v(18.1, estimate.OriginInterpolated)}},
		{Date: day("2026-11-05"), Values: map[string]estimate.Value{"fat_ratio_pct": v(18, estimate.OriginMeasured)}}, // no weight: left out
	})
	if len(got) != 3 {
		t.Fatalf("want 3 days, got %+v", got)
	}
	if got[0].Day != "2026-11-08" || got[0].WeightKg != 82 || got[0].FatPct != nil {
		t.Fatalf("held day without fat: %+v", got[0])
	}
	if got[1].Day != "2026-11-07" || got[1].FatPct == nil || *got[1].FatPct != 18.1 {
		t.Fatalf("interpolated day is included: %+v", got[1])
	}
	if got[2].Day != "2026-11-06" {
		t.Fatalf("newest first: %+v", got)
	}
}
