package mainbackend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/store"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

func newProxyServer(t *testing.T, proxies, base string) (*httptest.Server, *sqlstore.Store) {
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
	t.Cleanup(srv.Close)
	trusted, err := ParseTrustedProxies([]string{proxies})
	if err != nil {
		t.Fatal(err)
	}
	baseURL, err := ParseBaseURL(base)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetTrustedProxies(trusted)
	srv.SetBaseURL(baseURL)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

// signupVia signs up through the proxy-facing server with the given extra request headers.
func signupVia(t *testing.T, ts *httptest.Server, hdr map[string]string) *http.Response {
	t.Helper()
	form := url.Values{"name": {"root"}, "email": {"root@example.com"}, "password": {testPassword}, "confirm": {testPassword}}
	req, _ := http.NewRequest("POST", ts.URL+"/signup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 303 {
		t.Fatalf("signup: %d", resp.StatusCode)
	}
	return resp
}

func sessionCookieOf(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func loginIP(t *testing.T, st *sqlstore.Store) string {
	t.Helper()
	events, err := st.ListAuditEvents(context.Background(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == auditLogin {
			return e.IP
		}
	}
	t.Fatal("no login event")
	return ""
}

func TestForwardedHeadersAreIgnoredFromUntrustedPeers(t *testing.T) {
	ts, st := newProxyServer(t, "", "")
	resp := signupVia(t, ts, map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-For": "203.0.113.9"})
	if sessionCookieOf(t, resp).Secure {
		t.Error("a visitor made the session cookie Secure by sending X-Forwarded-Proto")
	}
	if resp.Header.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS sent over plain HTTP")
	}
	if ip := loginIP(t, st); ip != "127.0.0.1" {
		t.Errorf("recorded address %q: a forged X-Forwarded-For was believed", ip)
	}
}

func TestTrustedProxyDescribesTheVisitor(t *testing.T) {
	ts, st := newProxyServer(t, "loopback", "")
	resp := signupVia(t, ts, map[string]string{
		"X-Forwarded-Proto": "https",
		// The left entry is whatever the visitor claimed; the proxy appended the real address.
		"X-Forwarded-For": "198.51.100.7, 203.0.113.9",
	})
	if !sessionCookieOf(t, resp).Secure {
		t.Error("cookie not Secure behind an HTTPS proxy")
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Error("no HSTS behind an HTTPS proxy")
	}
	if ip := loginIP(t, st); ip != "203.0.113.9" {
		t.Errorf("recorded address %q, want the one the proxy saw", ip)
	}
}

func TestChainOfTrustedProxies(t *testing.T) {
	ts, st := newProxyServer(t, "private", "")
	signupVia(t, ts, map[string]string{"X-Forwarded-For": "203.0.113.9, 10.1.2.3"})
	if ip := loginIP(t, st); ip != "203.0.113.9" {
		t.Errorf("recorded address %q, want the first address outside the trusted ranges", ip)
	}
}

func TestOriginCheckUsesTheForwardedHost(t *testing.T) {
	post := func(ts *httptest.Server, hdr map[string]string) int {
		req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader("email=a%40b.c&password=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://dexa.example.com")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	fwd := map[string]string{"X-Forwarded-Host": "dexa.example.com", "X-Forwarded-Proto": "https"}

	untrusted, _ := newProxyServer(t, "", "")
	if code := post(untrusted, fwd); code != http.StatusForbidden {
		t.Errorf("cross-origin post without a trusted proxy: %d", code)
	}
	trusted, _ := newProxyServer(t, "loopback", "")
	if code := post(trusted, fwd); code == http.StatusForbidden {
		t.Error("the proxy's own site was treated as cross-origin")
	}
	if code := post(trusted, map[string]string{"Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("a different site was allowed: %d", code)
	}
	based, _ := newProxyServer(t, "", "https://dexa.example.com")
	if code := post(based, nil); code == http.StatusForbidden {
		t.Error("the base URL's origin was treated as cross-origin")
	}
}

func TestBaseURL(t *testing.T) {
	ts, _ := newProxyServer(t, "", "https://dexa.example.com")
	// Cookies are Secure because the public address is https, whatever the proxy forwards.
	if !sessionCookieOf(t, signupVia(t, ts, nil)).Secure {
		t.Error("cookie not Secure with an https base URL")
	}
}

func TestParseProxySettings(t *testing.T) {
	for _, bad := range []string{"nonsense", "10.0.0.0/33", "1.2.3"} {
		if _, err := ParseTrustedProxies([]string{bad}); err == nil {
			t.Errorf("trusted proxy %q accepted", bad)
		}
	}
	if p, err := ParseTrustedProxies([]string{"10.0.0.1, 192.168.0.0/16", "loopback"}); err != nil || len(p) != 4 {
		t.Errorf("valid list: %v %v", p, err)
	}
	for _, bad := range []string{"dexa.example.com", "ftp://x", "https://x.example/app", "https://u:p@x.example", "https://x.example/?a=1"} {
		if _, err := ParseBaseURL(bad); err == nil {
			t.Errorf("base URL %q accepted", bad)
		}
	}
	if u, err := ParseBaseURL(""); err != nil || u != nil {
		t.Errorf("empty base URL: %v %v", u, err)
	}
}
