package web

import (
	"math"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/models"
)

func TestParseMeasuresRejectsNaNAndInf(t *testing.T) {
	fields := []field{{Key: "weight_kg", Label: "Weight"}}
	for _, in := range []string{"NaN", "nan", "Inf", "-Inf", "+Infinity", "1e999"} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(url.Values{"weight_kg": {in}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var w *float64
		if err := parseMeasures(r, fields, []**float64{&w}); err == nil {
			t.Errorf("%q accepted as %v", in, w)
		}
	}
}

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
