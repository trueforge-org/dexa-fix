package web

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	ts, c := newTestServer(t)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for h, want := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestStaticAssetsAreCacheableAndCompressed(t *testing.T) {
	ts, c := newTestServer(t)
	page := string(mustGet(t, c, ts.URL+"/"))
	i := strings.Index(page, "/static/app.css?v=")
	if i < 0 {
		t.Fatalf("no versioned css url in page:\n%s", page)
	}
	href := page[i:]
	href = href[:strings.IndexByte(href, '"')]

	req, _ := http.NewRequest("GET", ts.URL+href, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req) // the default client would decompress for us
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); len(b) == 0 {
		t.Error("empty css")
	}

	req, _ = http.NewRequest("GET", ts.URL+"/static/app.css", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	r2, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: %d", r2.StatusCode)
	}
}

func TestOversizedFormIsRejected(t *testing.T) {
	ts, c := newTestServer(t)
	resp, err := c.Post(ts.URL+"/login", "application/x-www-form-urlencoded", strings.NewReader("email=a&password="+strings.Repeat("x", 2<<20)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestLoginIsThrottledAfterRepeatedFailures(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root")
	form := url.Values{"email": {"root@example.com"}, "password": {"wrong password"}}
	var last int
	for i := 0; i < maxLoginFail+2; i++ {
		last = post(t, noRedirectClient(), ts.URL+"/login", form).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", maxLoginFail+2, last)
	}
	// Even the right password is refused while blocked, so guessing gains nothing.
	good := url.Values{"email": {"root@example.com"}, "password": {testPassword}}
	if r := post(t, noRedirectClient(), ts.URL+"/login", good); r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("right password while blocked: %d", r.StatusCode)
	}
}

func TestStrengthLevelStatusNeedsBulkAccess(t *testing.T) {
	ts, c, _ := newAuthServer(t)
	signup(t, ts, c, "root")
	if code, _ := body(t, c, ts.URL+"/u/1/strengthlevel/status"); code != http.StatusNotFound {
		t.Errorf("status %d", code)
	}
}

func TestLimiterEvictionKeepsNearlyBlockedKeys(t *testing.T) {
	l := newFailLimiter()
	for range maxLoginFail - 1 {
		l.fail("login:victim")
	}
	for i := range maxLimiterKeys + 100 {
		l.fail(fmt.Sprintf("login:flood%d", i))
	}
	if len(l.fails) > maxLimiterKeys {
		t.Fatalf("%d keys, want at most %d", len(l.fails), maxLimiterKeys)
	}
	l.fail("login:victim")
	if !l.blocked("login:victim", maxLoginFail) {
		t.Fatal("a flood of other keys reset the victim's counter")
	}
}

func TestIPv6ClientsShareTheirSlash64(t *testing.T) {
	key := func(addr string) string {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = addr
		return ipKey(r)
	}
	if key("[2001:db8:1:2::1]:1") != key("[2001:db8:1:2:ffff::9]:2") {
		t.Error("addresses in one /64 got different keys")
	}
	if key("[2001:db8:1:2::1]:1") == key("[2001:db8:1:3::1]:1") {
		t.Error("different /64 prefixes share a key")
	}
	if key("192.0.2.7:80") != "ip:192.0.2.7" {
		t.Errorf("IPv4 key: %s", key("192.0.2.7:80"))
	}
}
