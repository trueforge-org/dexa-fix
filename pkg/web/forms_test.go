package web

import (
	"math"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

func TestCalendarDateAndParseWhenUseTheZone(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no tz data")
	}
	// the API sends the scan at local midnight: its date in Sydney is 1 March, not 28 February (UTC)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, sydney)
	if got := calendarDate(at, sydney); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("calendarDate = %v", got)
	}
	got, err := parseWhen("2026-03-01T07:30", dateTimeLayout, sydney)
	if err != nil || !got.Equal(time.Date(2026, 2, 28, 20, 30, 0, 0, time.UTC)) {
		t.Errorf("parseWhen = %v %v", got, err)
	}
	if got, _ := parseWhen("2026-03-01", dateLayout, time.UTC); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("plain date = %v", got)
	}
}
