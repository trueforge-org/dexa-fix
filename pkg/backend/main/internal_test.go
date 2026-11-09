package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

func newInternalServer(t *testing.T) (*httptest.Server, *Server, *http.Cookie) {
	t.Helper()
	ts, _, st := newAuthServer(t)
	form := url.Values{"name": {"root"}, "email": {"root@example.com"}, "password": {testPassword}, "confirm": {testPassword}}
	req, _ := http.NewRequest("POST", ts.URL+"/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ck := sessionCookieOf(t, resp)

	srv, err := New(st, importer.NewRegistry(withings.New(withings.Config{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.SetServiceSecrets("current-secret-0123456789abcdef", "previous-secret-0123456789abcdef")
	bs := httptest.NewServer(srv.InternalHandler())
	t.Cleanup(bs.Close)
	return bs, srv, ck
}

func internalGet(t *testing.T, bs *httptest.Server, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", bs.URL+apiclient.Prefix+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestInternalAPIRequiresServiceSecret(t *testing.T) {
	bs, _, ck := newInternalServer(t)
	cookie := sessionCookie + "=" + ck.Value
	for name, hdr := range map[string]map[string]string{
		"missing": {apiclient.HeaderCookie: cookie},
		"wrong":   {apiclient.HeaderService: "nope", apiclient.HeaderCookie: cookie},
		"prefix":  {apiclient.HeaderService: "current-secret", apiclient.HeaderCookie: cookie},
	} {
		if got := internalGet(t, bs, "/session", hdr).StatusCode; got != http.StatusUnauthorized {
			t.Errorf("%s secret: status %d", name, got)
		}
	}
	for _, sec := range []string{"current-secret-0123456789abcdef", "previous-secret-0123456789abcdef"} {
		hdr := map[string]string{apiclient.HeaderService: sec, apiclient.HeaderCookie: cookie}
		if got := internalGet(t, bs, "/session", hdr).StatusCode; got != http.StatusOK {
			t.Errorf("secret %q: status %d", sec, got)
		}
	}
	// A valid secret without a session is "not signed in", not an error of the service.
	if got := internalGet(t, bs, "/session", map[string]string{apiclient.HeaderService: "current-secret-0123456789abcdef"}).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("no session: %d", got)
	}
}

func TestInternalAPIIgnoresAuthorization(t *testing.T) {
	bs, srv, _ := newInternalServer(t)
	// Basic credentials work on /api/ only, never on the internal API.
	req, _ := http.NewRequest("GET", bs.URL+apiclient.Prefix+"/session", nil)
	req.SetBasicAuth("root@example.com", testPassword)
	req.Header.Set(apiclient.HeaderService, "current-secret-0123456789abcdef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Basic credentials accepted by the internal API: %d", resp.StatusCode)
	}
	_ = srv
}

func TestInternalAPIUsesForwardedVisitor(t *testing.T) {
	bs, srv, ck := newInternalServer(t)
	var gotIP, gotHost string
	var gotSecure bool
	mux := http.NewServeMux()
	mux.HandleFunc(apiclient.Prefix+"/probe", func(w http.ResponseWriter, r *http.Request) {
		gotIP, gotHost, gotSecure = clientIP(r), r.Host, isSecure(r)
		if c, err := r.Cookie(sessionCookie); err != nil || c.Value != ck.Value {
			t.Errorf("session cookie not restored: %v", err)
		}
		if r.Header.Get(apiclient.HeaderCookie) != "" {
			t.Error("the transport header was left on the request")
		}
	})
	probe := httptest.NewServer(srv.serviceAuth(srv.visitorContext(mux)))
	t.Cleanup(probe.Close)
	_ = bs
	req, _ := http.NewRequest("GET", probe.URL+apiclient.Prefix+"/probe", nil)
	req.Header.Set(apiclient.HeaderService, "current-secret-0123456789abcdef")
	req.Header.Set(apiclient.HeaderCookie, sessionCookie+"="+ck.Value)
	req.Header.Set(apiclient.HeaderClientIP, "203.0.113.9")
	req.Header.Set(apiclient.HeaderHost, "dexa.example.com")
	req.Header.Set(apiclient.HeaderSecure, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotIP != "203.0.113.9" || gotHost != "dexa.example.com" || !gotSecure {
		t.Errorf("ip %q host %q secure %v", gotIP, gotHost, gotSecure)
	}

	// Without the secret none of it is believed.
	gotIP = ""
	req.Header.Del(apiclient.HeaderService)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if gotIP != "" {
		t.Error("forwarded headers were used without the service secret")
	}
}

func TestPublicAPIIgnoresInternalHeaders(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root") // creates the user
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/users/1/settings", nil)
	req.Header.Set(apiclient.HeaderCookie, sessionCookie+"=whatever")
	req.Header.Set(apiclient.HeaderService, "x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("public API: %d", resp.StatusCode)
	}
}

func TestFrontendResolvesSessionThroughBackend(t *testing.T) {
	if splitTests() {
		t.Skip("tests the combined server")
	}
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root")
	resp, err := c.Get(ts.URL + "/u/1/scale")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed-in page: %d", resp.StatusCode)
	}
	srv := serverOf(ts)
	u, err := srv.api.Session(context.Background())
	if err != nil || u != nil {
		t.Errorf("session without cookies: %v %v", u, err)
	}
}
