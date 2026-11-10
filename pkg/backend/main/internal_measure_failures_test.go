package mainbackend

import (
	"net/http"
	"testing"
)

func TestScanAndReadingEndpointsRefuseAndFailCleanly(t *testing.T) {
	const scan = `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":80}`
	const reading = `{"measured_at":"2026-03-01T08:00:00Z","weight_kg":80}`
	cases := []struct {
		name, method, path, body string
		bodyDown                 bool // the body service does not run
		want                     int
	}{
		{"a scan that is not JSON", "POST", "/users/2/dexa", "{nope", false, http.StatusBadRequest},
		{"a scan with a weight that cannot be", "POST", "/users/2/dexa", `{"measured_at":"2026-03-01T00:00:00Z","weight_kg":-3}`, false, http.StatusBadRequest},
		{"a scan without the body service", "POST", "/users/2/dexa", scan, true, http.StatusServiceUnavailable},
		{"the scans without the body service", "GET", "/users/2/pages/dexa", "", true, http.StatusServiceUnavailable},
		{"a scan id that is not a number", "DELETE", "/users/2/dexa/abc", "", false, http.StatusNotFound},
		{"a scan delete without the body service", "DELETE", "/users/2/dexa/1", "", true, http.StatusServiceUnavailable},
		{"a reading that is not JSON", "POST", "/users/2/scale", "{nope", false, http.StatusBadRequest},
		{"a reading with a weight that cannot be", "POST", "/users/2/scale", `{"measured_at":"2026-03-01T08:00:00Z","weight_kg":-3}`, false, http.StatusBadRequest},
		{"a reading without the body service", "POST", "/users/2/scale", reading, true, http.StatusServiceUnavailable},
		{"the readings without the body service", "GET", "/users/2/pages/scale", "", true, http.StatusServiceUnavailable},
		{"a page cursor that is not one", "GET", "/users/2/pages/scale?before=nonsense", "", false, http.StatusBadRequest},
		{"a reading id that is not a number", "DELETE", "/users/2/scale/abc", "", false, http.StatusNotFound},
		{"a reading delete without the body service", "DELETE", "/users/2/scale/1", "", true, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newJobRig(t)
			if c.bodyDown {
				r.b.body = bodyService{c: bodyDown(t)}
			}
			if got := r.raw(c.method, c.path, c.body); got != c.want {
				t.Fatalf("%s %s: status %d, want %d", c.method, c.path, got, c.want)
			}
		})
	}
}
