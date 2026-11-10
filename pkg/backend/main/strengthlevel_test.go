package mainbackend

import (
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/estimate"
)

func estDay(s string) time.Time { t, _ := time.Parse(dateLayout, s); return t }

func TestEntriesFromEstimates(t *testing.T) {
	v := func(x float64, o estimate.Origin) estimate.Value { return estimate.Value{V: x, Origin: o} }
	got := entriesFromEstimates([]estimate.DayEstimate{
		{Date: estDay("2026-11-06"), Values: map[string]estimate.Value{"weight_kg": v(81, estimate.OriginMeasured), "fat_ratio_pct": v(18.2, estimate.OriginMeasured)}},
		{Date: estDay("2026-11-08"), Values: map[string]estimate.Value{"weight_kg": v(82, estimate.OriginHeld)}},
		{Date: estDay("2026-11-07"), Values: map[string]estimate.Value{"weight_kg": v(81.5, estimate.OriginInterpolated), "fat_ratio_pct": v(18.1, estimate.OriginInterpolated)}},
		{Date: estDay("2026-11-05"), Values: map[string]estimate.Value{"fat_ratio_pct": v(18, estimate.OriginMeasured)}}, // no weight: left out
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
