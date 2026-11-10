package bodybackend

import (
	"context"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// BenchmarkEstimatesCached10Years is what every estimates page view costs the body service and the
// link to it once the estimate cache is warm: ten years of daily readings, encoded, sent, decoded.
func BenchmarkEstimatesCached10Years(b *testing.B) {
	r := newRig(b)
	ctx := context.Background()
	alice := r.sub(0)
	if err := r.client.CreateScan(ctx, alice, &models.DexaScan{MeasuredAt: day(0), WeightKg: fl(80), FatRatioPct: fl(20), FatMassKg: fl(16), FatFreeMassKg: fl(64), LeanMassKg: fl(60)}); err != nil {
		b.Fatal(err)
	}
	const days, batch = 3650, 500
	for from := 0; from < days; from += batch {
		var rs []models.ScaleReading
		for i := from; i < min(from+batch, days); i++ {
			rs = append(rs, models.ScaleReading{MeasuredAt: day(i), WeightKg: fl(80 + float64(i%30)*0.05), FatRatioPct: fl(20 + float64(i%45)*0.04)})
		}
		if _, err := r.client.UpsertReadings(ctx, alice, rs); err != nil {
			b.Fatal(err)
		}
	}
	first, err := r.client.Estimates(ctx, alice, "") // computes and fills the cache
	if err != nil || len(first.Result.Days) < days {
		b.Fatalf("warm-up: %d days, %v", len(first.Result.Days), err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.client.Estimates(ctx, alice, ""); err != nil {
			b.Fatal(err)
		}
	}
}
