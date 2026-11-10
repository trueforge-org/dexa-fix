package frontend

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// pageCall is one request a visitor makes.
type pageCall struct {
	name   string
	method string
	path   string
	form   url.Values
}

func v(kv ...string) url.Values {
	out := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		out.Set(kv[i], kv[i+1])
	}
	return out
}

// callsThatNeedTheBackend are the requests of a signed-in administrator, each with the form that
// gets past the frontend's own checks to the one call it makes to the backend.
var callsThatNeedTheBackend = []pageCall{
	{"the DEXA page", "GET", "/u/1/dexa", nil},
	{"saving a DEXA scan", "POST", "/u/1/dexa", v("measured_at", "2026-01-02", "weight_kg", "80")},
	{"the scale page", "GET", "/u/1/scale", nil},
	{"saving a reading", "POST", "/u/1/scale", v("measured_at", "2026-01-02T08:00", "weight_kg", "80")},
	{"deleting a reading", "POST", "/u/1/scale/5/delete", nil},
	{"the estimates page", "GET", "/u/1/estimates", nil},
	{"saving the hidden columns", "POST", "/u/1/estimates/columns", v("column", "weight_kg")},
	{"the settings page", "GET", "/u/1/settings", nil},
	{"saving the settings", "POST", "/u/1/settings", v("athlete_default", "1")},
	{"saving the profile", "POST", "/u/1/profile", v("name", "Root")},
	{"changing the password", "POST", "/u/1/password", v("current", testPassword, "password", "another one", "confirm", "another one")},
	{"creating a token", "POST", "/u/1/tokens", v("name", "script")},
	{"revoking a token", "POST", "/u/1/tokens/3/delete", nil},
	{"unlinking an identity", "POST", "/u/1/identities/github/delete", nil},
	{"the first export", "GET", "/u/1/export/profile", nil},
	{"the second export", "GET", "/u/1/export/data", nil},
	{"an import", "POST", "/u/1/import/withings", v("day", "2026-01-02")},
	{"connecting an integration", "GET", "/u/1/integrations/withings/connect", nil},
	{"the integration callback", "GET", "/integrations/withings/callback?state=a&code=b", nil},
	{"disconnecting an integration", "POST", "/u/1/integrations/withings/disconnect", nil},
	{"saving the Strength Level login", "POST", "/u/1/strengthlevel/login", v("email", "a@b.c", "password", "x")},
	{"starting the Strength Level sync", "POST", "/u/1/strengthlevel/sync", v("days", "30")},
	{"the Strength Level status", "GET", "/u/1/strengthlevel/status", nil},
	{"sending a day to Strength Level", "POST", "/u/1/strengthlevel/day", v("day", "2026-01-02")},
	{"the Admin page", "GET", "/admin", nil},
	{"adding a user", "POST", "/admin/users", v("name", "Bob", "email", "bob@example.com", "password", "secret words")},
	{"changing a role", "POST", "/admin/users/2/role", v("role", "admin")},
	{"switching Strength Level for a user", "POST", "/admin/users/2/strengthlevel", v("enabled", "1")},
	{"switching Strength Level for everyone", "POST", "/admin/users/2/strengthlevel-bulk", v("enabled", "1")},
	{"setting a user's password", "POST", "/admin/users/2/password", v("password", "secret words")},
	{"deleting a user", "POST", "/admin/users/2/delete", nil},
	{"making a reset link", "POST", "/admin/users/2/reset-link", nil},
	{"removing a user's second step", "POST", "/admin/users/2/2fa/delete", nil},
	{"setting a user's priority", "POST", "/admin/users/2/priority", v("priority", "5")},
	{"switching password sign-in", "POST", "/admin/password-login", v("enabled", "1")},
	{"switching sign-up", "POST", "/admin/signup", v("enabled", "1")},
	{"saving a provider", "POST", "/admin/providers", v("slug", "x", "kind", "oidc")},
	{"deleting a provider", "POST", "/admin/providers/x/delete", nil},
	{"saving the Withings app", "POST", "/admin/withings", v("client_id", "a")},
	{"saving the server settings", "POST", "/admin/server", v("base_url", "https://dexa.example.com")},
	{"saving the analytics", "POST", "/admin/analytics", v("scarf_pixel_id", "")},
	{"saving the SMTP settings", "POST", "/admin/smtp", v("host", "smtp.example.com", "port", "25")},
	{"saving the TLS settings", "POST", "/admin/tls", v("mode", "off")},
	{"the two-step policy", "POST", "/admin/two-factor", v("policy", "off")},
	{"the workers page", "GET", "/admin/workers", nil},
	{"saving a kind of job", "POST", "/admin/workers/kinds/withings", workerKindForm()},
	{"resuming a kind of job", "POST", "/admin/workers/kinds/withings/resume", nil},
	{"the admin status page", "GET", "/admin/status", nil},
	{"the two-step setup", "POST", "/u/1/2fa/begin", v("current", testPassword)},
	{"confirming the two-step setup", "POST", "/u/1/2fa/confirm", v("code", "123456")},
	{"disabling the two step", "POST", "/u/1/2fa/disable", v("current", testPassword)},
	{"new recovery codes", "POST", "/u/1/2fa/recovery", v("current", testPassword)},
	{"signing out", "POST", "/logout", nil},
}

func workerKindForm() url.Values {
	f := url.Values{}
	for _, k := range []string{"max_running", "max_attempts", "retry_base_seconds", "timeout_seconds", "schedule_hours", "pause_after", "breaker"} {
		f.Set(k, "1")
	}
	f.Set("opt_pause_ms", "5")
	return f
}

// guards keep working while the handlers' own calls fail: they decide who is signed in.
var guards = []string{"GET /session", "GET /admin/step-up"}

func TestEveryPageSaysTheBackendIsUnavailableWhenItFails(t *testing.T) {
	for name, fault := range map[string]faultFn{"answers 500": serverError, "cuts the connection": cutOff} {
		t.Run("a backend that "+name, func(t *testing.T) {
			fs := newFaultStack(t)
			fs.failAll(fault, guards...)
			for _, c := range callsThatNeedTheBackend {
				var res result
				if c.method == "GET" {
					res = fs.get(c.path)
				} else {
					res = fs.post(c.path, c.form)
				}
				if res.code != http.StatusServiceUnavailable || res.header.Get("Retry-After") == "" ||
					!strings.Contains(res.body, "temporarily unavailable") {
					t.Errorf("%s: %d %q, Retry-After %q", c.name, res.code, res.body, res.header.Get("Retry-After"))
				}
			}
		})
	}
}
