package backend

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func TestAPIRangeChecks(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	if err := validScaleReading(&models.ScaleReading{WeightKg: f(80), FatRatioPct: f(20)}); err != nil {
		t.Errorf("valid reading rejected: %v", err)
	}
	for name, r := range map[string]models.ScaleReading{
		"nan":         {WeightKg: f(math.NaN())},
		"zero weight": {WeightKg: f(0)},
		"negative":    {WeightKg: f(-5)},
		"bf over 100": {FatRatioPct: f(101)},
		"long notes":  {Notes: strings.Repeat("x", 501)},
	} {
		if validScaleReading(&r) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if validDexaScan(&models.DexaScan{AndroidFatPct: f(-1)}) == nil {
		t.Error("negative android fat accepted")
	}
	if err := validDexaScan(&models.DexaScan{TScore: f(-2.5), BMDGCm2: f(1.2)}); err != nil {
		t.Errorf("valid scan rejected: %v", err)
	}
}
func TestCalendarDateUsesTheZone(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no tz data")
	}
	// the API sends the scan at local midnight: its date in Sydney is 1 March, not 28 February (UTC)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, sydney)
	if got := calendarDate(at, sydney); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("calendarDate = %v", got)
	}
}
