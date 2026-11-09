package frontend

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
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

func TestParseWhenUsesTheZone(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Skip("no tz data")
	}
	got, err := parseWhen("2026-03-01T07:30", dateTimeLayout, sydney)
	if err != nil || !got.Equal(time.Date(2026, 2, 28, 20, 30, 0, 0, time.UTC)) {
		t.Errorf("parseWhen = %v %v", got, err)
	}
	if got, _ := parseWhen("2026-03-01", dateLayout, time.UTC); !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("plain date = %v", got)
	}
}
