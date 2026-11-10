package frontend

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/estimate"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func TestAnUnknownMethodKeyIsShownAsItIs(t *testing.T) {
	if got := methodLabel("something-new"); got != "something-new" {
		t.Fatalf("got %q", got)
	}
}

func TestGroupLabelNamesAthleteMode(t *testing.T) {
	plain, athlete := groupLabel("scale", false), groupLabel("scale", true)
	if athlete != plain+", athlete" {
		t.Fatalf("%q and %q", plain, athlete)
	}
}

func TestNotesLongerThan500CharactersAreRefused(t *testing.T) {
	form := url.Values{"notes": {strings.Repeat("x", 501)}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := parseNotes(r); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("got %v", err)
	}
}

func TestTheOriginOfAnInterpolatedValueIsNamed(t *testing.T) {
	if got := tileOrigin(estimate.Value{Origin: estimate.OriginInterpolated}); got != "Interpolated" {
		t.Fatalf("got %q", got)
	}
}

func TestNoChangeIsShownWhenTheOldValueIsTooFarBack(t *testing.T) {
	latest := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	far := latest.AddDate(0, 0, -(deltaDays + deltaSlackDays + 5))
	older := []estimate.DayEstimate{{Date: far, Values: map[string]estimate.Value{"weight_kg": {V: 80}}}}
	if _, _, ok := valueAround(older, latest, "weight_kg"); ok {
		t.Fatal("compared against a day that is much older than the comparison period")
	}
}

func TestWidenCSPKeepsWhatWasThereAndSkipsEmptyDirectives(t *testing.T) {
	got := widenCSP("default-src 'self';; script-src 'self'", map[string][]string{
		"script-src":  {"'self'", "https://cdn.example"}, // 'self' is there already: not repeated
		"connect-src": {"https://stats.example"},         // the policy has none yet: starts from 'self'
	})
	if strings.Count(got, "'self'") != 3 || !strings.Contains(got, "script-src 'self' https://cdn.example") ||
		!strings.Contains(got, "connect-src 'self' https://stats.example") {
		t.Fatalf("got %q", got)
	}
}

func TestTheStatusPageCountsAnUnreachableBackendAsDown(t *testing.T) {
	v := newStatusView(nil, nil)
	if v.Overall != apiclient.StateDown || statusCode(v) != http.StatusServiceUnavailable {
		t.Fatalf("%+v", v)
	}
	if len(v.Rows) != 2 || v.Rows[1].Note == "" {
		t.Fatalf("rows %+v", v.Rows)
	}
}
