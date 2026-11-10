package mainbackend

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/workers/builtin"
)

func TestTheMeasurementPagesWhenTheBodyServiceFailsOrRefuses(t *testing.T) {
	scan := url.Values{"measured_at": {"2026-03-02"}, "weight_kg": {"80"}}
	reading := url.Values{"measured_at": {"2026-03-02T08:00"}, "weight_kg": {"80"}}
	cases := []struct {
		name, method, path string
		form               url.Values
		useBody            func(*testing.T) *bodyapi.Client
		want               int
		wantText           string
	}{
		{"the scans page without the body service", "GET", "/u/1/dexa", nil, bodyDown, 503, "unavailable"},
		{"a new scan without the body service", "POST", "/u/1/dexa", scan, bodyDown, 503, "unavailable"},
		{"a new scan the body service refuses", "POST", "/u/1/dexa", scan, func(t *testing.T) *bodyapi.Client { return bodyAnswering(t, 400, bodyapi.CodeInvalid) }, 400, "the body service says no"},
		{"a new reading without the body service", "POST", "/u/1/scale", reading, bodyDown, 503, "unavailable"},
		{"a new reading the body service refuses", "POST", "/u/1/scale", reading, func(t *testing.T) *bodyapi.Client { return bodyAnswering(t, 400, bodyapi.CodeInvalid) }, 400, "the body service says no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newAdminEnv(t)
			e.useBody(c.useBody(t))
			code, out := e.do(t, c.method, c.path, c.form)
			if code != c.want || !strings.Contains(out, c.wantText) {
				t.Fatalf("%s %s: %d %.150q, want %d with %q", c.method, c.path, code, out, c.want, c.wantText)
			}
		})
	}
	t.Run("a reading id that is not a number", func(t *testing.T) {
		e := newAdminEnv(t)
		if code, _ := e.do(t, "POST", "/u/1/scale/abc/delete", url.Values{}); code != http.StatusNotFound {
			t.Fatalf("status %d, want 404", code)
		}
	})
}

func TestTheAdminPagesWhenTheIdOfAUserIsNotANumber(t *testing.T) {
	e := newAdminEnv(t)
	for _, path := range []string{
		"/admin/users/abc/role", "/admin/users/abc/strengthlevel", "/admin/users/abc/strengthlevel-bulk",
		"/admin/users/abc/password", "/admin/users/abc/delete", "/admin/users/abc/reset-link",
	} {
		if code, _ := e.do(t, "POST", path, url.Values{}); code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, code)
		}
	}
}

func TestTheAdminPageWhenTheSecondStepCannotBeChecked(t *testing.T) {
	e := newAdminEnv(t)
	breakTable(t, e.st, "user_totp")
	if code, _ := e.do(t, "GET", "/admin", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", code)
	}
}

func TestAnAdminMessageThatIsTooLongIsCutForTheRedirect(t *testing.T) {
	e := newAdminEnv(t)
	form := url.Values{
		"kind": {"oauth2"}, "slug": {"gh"}, "label": {strings.Repeat("x", 400)}, "client_id": {"c"}, "client_secret": {"s"},
		"auth_url": {"https://idp.example/auth"}, "token_url": {"https://idp.example/token"}, "userinfo_url": {"https://idp.example/me"},
	}
	code, out := e.do(t, "POST", "/admin/providers", form)
	if code != http.StatusSeeOther || len(adminFlash(t, out)) != 300 {
		t.Fatalf("status %d, message of %d bytes, want a redirect with 300", code, len(adminFlash(t, out)))
	}
}

func TestTheFrontendKeepsGoingWhenItCannotFetchTheNewSettings(t *testing.T) {
	e := newAdminEnv(t)
	// The save reloads the settings once; the frontend's fetch of them afterwards is what fails.
	e.flaky.failAfter("SettingsGeneration", 1)
	code, out := e.do(t, "POST", "/admin/server", url.Values{"audit_retention_days": {"30"}})
	if code != http.StatusSeeOther || !strings.Contains(adminFlash(t, out), "apply now") {
		t.Fatalf("status %d %q, want the save to be reported anyway", code, out)
	}
}

func TestTheStrengthLevelProgressBlockIsServedToTheSettingsPage(t *testing.T) {
	ts, alice, _ := newServerAndStore(t, builtin.Options{})
	signup(t, ts, alice, "alice") // the administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")
	post(t, alice, ts.URL+"/admin/users/2/strengthlevel", url.Values{"enabled": {"1"}})
	post(t, alice, ts.URL+"/admin/users/2/strengthlevel-bulk", url.Values{"enabled": {"1"}})
	if code, _ := body(t, bob, ts.URL+"/u/2/strengthlevel/status"); code != http.StatusOK {
		t.Fatalf("status block: %d", code)
	}
}

func TestTheProviderPagesOfAnUnknownProvider(t *testing.T) {
	e := newAdminEnv(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/u/1/import/nope"}, {"GET", "/u/1/integrations/nope/connect"},
		{"GET", "/integrations/nope/callback?state=s&code=c"}, {"POST", "/u/1/integrations/nope/disconnect"},
	} {
		if code, _ := e.do(t, c.method, c.path, url.Values{}); code != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", c.method, c.path, code)
		}
	}
}
