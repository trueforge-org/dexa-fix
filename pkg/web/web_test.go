package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func newTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ts, client
}

func post(t *testing.T, c *http.Client, u string, form url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func body(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestUserAndMeasurementFlow(t *testing.T) {
	ts, c := newTestServer(t)

	if r := post(t, c, ts.URL+"/users", url.Values{"name": {"alice"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("create user: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	// entering an existing name opens that user
	if r := post(t, c, ts.URL+"/users", url.Values{"name": {"alice"}}); r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("existing user: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if r := post(t, c, ts.URL+"/users", url.Values{"name": {" "}}); r.StatusCode != 422 {
		t.Fatalf("empty name: %d", r.StatusCode)
	}
	if code, b := body(t, c, ts.URL+"/"); code != 200 || !strings.Contains(b, "alice") {
		t.Fatalf("index: %d", code)
	}

	r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"81,5"}, "muscle_mass_kg": {"30.2"}})
	if r.StatusCode != 303 {
		t.Fatalf("scale create: %d", r.StatusCode)
	}
	if code, b := body(t, c, ts.URL+"/u/1/scale"); code != 200 || !strings.Contains(b, "81.50") || !strings.Contains(b, "2026-03-01 07:30") {
		t.Fatalf("scale page: %d\n%s", code, b)
	}
	r = post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-02"}, "weight_kg": {"80"}, "bmd_g_cm2": {"1.25"}})
	if r.StatusCode != 303 {
		t.Fatalf("dexa create: %d", r.StatusCode)
	}
	if code, b := body(t, c, ts.URL+"/u/1/dexa"); code != 200 || !strings.Contains(b, "1.25") || !strings.Contains(b, "2026-03-02") {
		t.Fatalf("dexa page: %d", code)
	}

	// validation
	if r := post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"2026-03-02"}}); r.StatusCode != 422 {
		t.Fatalf("no values: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/dexa", url.Values{"measured_at": {"nope"}, "weight_kg": {"1"}}); r.StatusCode != 422 {
		t.Fatalf("bad date: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/scale", url.Values{"measured_at": {"2026-03-01T07:30"}, "weight_kg": {"abc"}}); r.StatusCode != 422 {
		t.Fatalf("bad number: %d", r.StatusCode)
	}

	// delete
	if r := post(t, c, ts.URL+"/u/1/scale/1/delete", nil); r.StatusCode != 303 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if r := post(t, c, ts.URL+"/u/1/scale/1/delete", nil); r.StatusCode != 404 {
		t.Fatalf("delete again: %d", r.StatusCode)
	}
}

func TestUnknownUser(t *testing.T) {
	ts, c := newTestServer(t)
	for _, p := range []string{"/u/99/dexa", "/u/99/scale", "/u/x/scale"} {
		if code, _ := body(t, c, ts.URL+p); code != 404 {
			t.Errorf("%s: %d", p, code)
		}
	}
	if code, _ := body(t, c, ts.URL+"/static/app.css"); code != 200 {
		t.Errorf("static: %d", code)
	}
}
