package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func newApp(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(st, importer.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCombinedServesPagesAndTheAPI(t *testing.T) {
	srv, ts := newApp(t)
	t.Cleanup(srv.Close)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	if code, page := get(t, c, ts.URL+"/"); code != 200 || !strings.Contains(page, "Sign in") {
		t.Fatalf("sign-in page: %d", code)
	}
	if code, body := get(t, c, ts.URL+"/healthz"); code != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", code, body)
	}
	if code, _ := get(t, c, ts.URL+"/readyz"); code != 200 {
		t.Fatalf("readyz: %d", code)
	}
	// The API is served on the same handler, by the backend, and wants credentials.
	if code, _ := get(t, c, ts.URL+"/api/v1/users"); code != http.StatusUnauthorized {
		t.Fatalf("API without credentials: %d", code)
	}
	// The first account can sign up through the pages, and then reaches its page.
	resp, err := c.PostForm(ts.URL+"/signup", url.Values{"name": {"root"}, "email": {"root@example.com"}, "password": {"correct horse"}, "confirm": {"correct horse"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("signup: %d %v", resp.StatusCode, resp.Cookies())
	}
	req, _ := http.NewRequest("GET", ts.URL+"/u/1/estimates", nil)
	req.AddCookie(resp.Cookies()[0])
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("signed-in page: %d", r2.StatusCode)
	}
}

func TestSetVersionAndCloseReachBothHalves(t *testing.T) {
	srv, _ := newApp(t)
	srv.SetVersion("v1.2.3")
	if srv.Frontend.Version != "v1.2.3" || srv.Backend.Version != "v1.2.3" {
		t.Errorf("versions %q %q", srv.Frontend.Version, srv.Backend.Version)
	}
	srv.Close() // stops the jobs of both halves; a second Close must not hang or panic
	srv.Close()
}
