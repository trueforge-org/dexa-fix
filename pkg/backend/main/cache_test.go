package mainbackend

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Nothing personal may be kept by a shared cache (a CDN such as Cloudflare, or a proxy): every
// response that is not a versioned static file must say so to browsers (Cache-Control) and to CDNs
// directly (CDN-Cache-Control, Surrogate-Control), including errors, redirects and the ones that set
// a cookie. This runs in the combined stack and, with DEXAFIX_TEST_SPLIT=1, over a real TCP split.
func TestPersonalResponsesAreNotCacheable(t *testing.T) {
	ts, alice, _ := newAuthServer(t)
	anon := noRedirectClient()
	signup(t, ts, alice, "alice")

	form := func(v url.Values) io.Reader { return strings.NewReader(v.Encode()) }
	type call struct {
		name, method, path string
		as                 *http.Client
		body               io.Reader
	}
	calls := []call{
		{"sign-in page", "GET", "/", anon, nil},
		{"docs", "GET", "/docs", anon, nil},
		{"terms", "GET", "/terms", anon, nil},
		{"privacy", "GET", "/privacy", anon, nil},
		{"privacy signed in", "GET", "/privacy", alice, nil},
		{"status page", "GET", "/status", anon, nil},
		{"status page signed in", "GET", "/status", alice, nil},
		{"status detail for an administrator", "GET", "/admin/status", alice, nil},
		{"status detail for nobody", "GET", "/admin/status", anon, nil},
		{"wrong password sets or clears cookies", "POST", "/login", anon, form(url.Values{"email": {"alice@example.com"}, "password": {"wrong password!"}})},
		{"signed-in page", "GET", "/u/1/scale", alice, nil},
		{"estimates page", "GET", "/u/1/estimates", alice, nil},
		{"settings page", "GET", "/u/1/settings", alice, nil},
		{"export", "GET", "/u/1/export", alice, nil},
		{"redirect for an anonymous visitor", "GET", "/u/1/scale", anon, nil},
		{"another user's page", "GET", "/u/99/scale", alice, nil},
		{"unknown path", "GET", "/nothing-here", alice, nil},
		{"personal path with a static suffix", "GET", "/u/1/settings/x.css", alice, nil},
		{"api estimates", "GET", "/api/v1/users/1/estimates", alice, nil},
		{"api of another user", "GET", "/api/v1/users/99/estimates", alice, nil},
		{"api without credentials", "GET", "/api/v1/users/1/estimates", anon, nil},
		{"api unknown", "GET", "/api/v1/nothing", alice, nil},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, ts.URL+c.path, c.body)
			if err != nil {
				t.Fatal(err)
			}
			if c.method == "POST" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			resp, err := c.as.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			cc := strings.ToLower(resp.Header.Get("Cache-Control"))
			if strings.Contains(cc, "public") || strings.Contains(cc, "max-age") && !strings.Contains(cc, "no-cache") && !strings.Contains(cc, "no-store") {
				t.Errorf("%d: Cache-Control %q lets a shared cache keep it", resp.StatusCode, cc)
			}
			if !strings.Contains(cc, "no-store") && !(strings.Contains(cc, "private") && strings.Contains(cc, "no-cache")) {
				t.Errorf("%d: Cache-Control %q is neither no-store nor private, no-cache", resp.StatusCode, cc)
			}
			for _, h := range []string{"CDN-Cache-Control", "Surrogate-Control"} {
				if got := resp.Header.Get(h); got != "no-store" {
					t.Errorf("%d: %s = %q, want no-store", resp.StatusCode, h, got)
				}
			}
			if len(resp.Cookies()) > 0 && strings.Contains(cc, "public") {
				t.Errorf("a response that sets a cookie is public: %q", cc)
			}
		})
	}
}

// A versioned static file is the one thing a CDN may keep, and it holds nothing personal. The
// directory listing is not served.
func TestStaticFilesAreTheOnlyCacheable(t *testing.T) {
	ts, _, _ := newAuthServer(t)
	resp, err := http.Get(ts.URL + "/static/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 && strings.Contains(string(b), "<pre>") {
		t.Errorf("the static directory is listed: %.200s", b)
	}
}
