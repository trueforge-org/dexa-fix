package mainbackend

import (
	"net/http"
	"testing"
)

func TestEstimatesAndSettingsEndpointsRefuseAndFailCleanly(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		breakIt                  func(*testing.T, *jobRig)
		want                     int
	}{
		{"estimates for a number of days that is not one", "GET", "/users/2/pages/estimates?days=many", "", nil, http.StatusBadRequest},
		{"estimates for no days at all", "GET", "/users/2/pages/estimates?days=0", "", nil, http.StatusBadRequest},
		{"estimates without the body service", "GET", "/users/2/pages/estimates", "", func(t *testing.T, r *jobRig) { r.b.body = bodyService{c: bodyDown(t)} }, http.StatusServiceUnavailable},
		{"hidden columns that are not JSON", "POST", "/users/2/estimates/columns", "{nope", nil, http.StatusBadRequest},
		{"hidden columns the database refuses", "POST", "/users/2/estimates/columns", `{"hidden":["weight_kg","nonsense"]}`, func(t *testing.T, r *jobRig) { refuseWrites(t, r.st, "users", "id", "2") }, http.StatusInternalServerError},
		{"hidden columns", "POST", "/users/2/estimates/columns", `{"hidden":["weight_kg","nonsense"]}`, nil, http.StatusNoContent},
		{"the settings page without the access tokens", "GET", "/users/2/pages/settings", "", func(t *testing.T, r *jobRig) { r.flaky.failOn("ListAccessTokens") }, http.StatusInternalServerError},
		{"the settings page without the audit log", "GET", "/users/2/pages/settings", "", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "audit_log") }, http.StatusInternalServerError},
		{"settings that are not JSON", "POST", "/users/2/settings", "{nope", nil, http.StatusBadRequest},
		{"settings the database refuses", "POST", "/users/2/settings", `{"athlete_default":true}`, func(t *testing.T, r *jobRig) { refuseWrites(t, r.st, "users", "id", "2") }, http.StatusInternalServerError},
		{"the profile export without the identities table", "GET", "/users/2/export/profile", "", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "user_identities") }, http.StatusInternalServerError},
		{"the data export without the body service", "GET", "/users/2/export/data", "", func(t *testing.T, r *jobRig) { r.b.body = bodyService{c: bodyDown(t)} }, http.StatusServiceUnavailable},
		{"the profile export", "GET", "/users/2/export/profile", "", nil, http.StatusOK},
		{"the data export", "GET", "/users/2/export/data", "", nil, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newJobRig(t)
			if c.breakIt != nil {
				c.breakIt(t, r)
			}
			if got := r.raw(c.method, c.path, c.body); got != c.want {
				t.Fatalf("%s %s: status %d, want %d", c.method, c.path, got, c.want)
			}
		})
	}
}
