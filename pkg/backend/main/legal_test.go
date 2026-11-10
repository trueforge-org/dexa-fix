package mainbackend

import (
	"strings"
	"testing"
)

func TestLegalPagesArePublicAndLinkedFromTheFooter(t *testing.T) {
	ts, c := newTestServer(t)
	for path, want := range map[string][]string{
		"/terms":   {"<h1>Terms of Service</h1>", "Not medical advice", "GNU Affero General Public License"},
		"/privacy": {"<h1>Privacy Policy</h1>", "Analytics and ads (optional)", "scarf.sh", "Google AdSense", "off unless the operator turns it on"},
	} {
		code, page := body(t, c, ts.URL+path)
		if code != 200 {
			t.Fatalf("GET %s signed out: %d", path, code)
		}
		for _, w := range want {
			if !strings.Contains(page, w) {
				t.Errorf("%s lacks %q", path, w)
			}
		}
		if strings.Contains(page, `class="user-menu"`) {
			t.Errorf("%s shows the user menu to a visitor", path)
		}
	}

	signup(t, ts, c, "alice")
	code, page := body(t, c, ts.URL+"/privacy")
	if code != 200 || !strings.Contains(page, `class="user-menu"`) {
		t.Errorf("signed in, /privacy: %d, user menu %v", code, strings.Contains(page, `class="user-menu"`))
	}
}

// Every page has the footer: the way to the terms, the privacy policy, the status page, the
// sponsor page and the source code, which the AGPL asks a server to offer.
func TestEveryPageHasTheFooter(t *testing.T) {
	ts, c := newTestServer(t)
	paths := []string{"/", "/docs", "/status", "/terms", "/privacy"}
	check := func(label string) {
		for _, p := range paths {
			_, page := body(t, c, ts.URL+p)
			for _, want := range []string{
				`<footer class="site-footer">`,
				`href="/terms"`, `href="/privacy"`, `href="/status"`, `href="/docs"`,
				`href="https://opencollective.com/dexafix"`,
				`href="https://github.com/trueforge-org/dexafix"`, `aria-label="DexaFix on GitHub"`,
			} {
				if !strings.Contains(page, want) {
					t.Errorf("%s %s: footer lacks %s", label, p, want)
				}
			}
			// Links that leave the site must not hand it over to the other page.
			if strings.Count(page, `target="_blank"`) > strings.Count(page, `rel="noopener`) {
				t.Errorf("%s %s: a link opens a new tab without rel=noopener", label, p)
			}
		}
	}
	check("signed out")
	signup(t, ts, c, "alice")
	paths = []string{"/docs", "/status", "/terms", "/privacy", "/u/1/estimates", "/u/1/scale", "/u/1/dexa", "/u/1/settings"} // "/" sends a signed-in person on
	check("signed in")
}

func TestSponsorNoteIsOnlyOnTheLandingPageOfSignedInPeople(t *testing.T) {
	ts, c := newTestServer(t)
	const note = `data-dismiss-cookie="dexafix_sponsor"`
	if _, page := body(t, c, ts.URL+"/"); strings.Contains(page, note) {
		t.Error("a visitor who is not signed in gets the sponsor note")
	}
	signup(t, ts, c, "alice")
	_, page := body(t, c, ts.URL+"/u/1/estimates")
	for _, want := range []string{note, "data-dismiss", `href="https://opencollective.com/dexafix"`, "Not now"} {
		if !strings.Contains(page, want) {
			t.Errorf("estimates page lacks %q", want)
		}
	}
	for _, p := range []string{"/u/1/scale", "/u/1/dexa", "/u/1/settings", "/docs"} {
		if _, other := body(t, c, ts.URL+p); strings.Contains(other, note) {
			t.Errorf("%s has the sponsor note", p)
		}
	}
	// The script that dismisses it is a file of ours, never inline (the CSP forbids inline scripts).
	if !strings.Contains(page, `src="/static/site.js?v=`) {
		t.Error("site.js is not loaded")
	}
	if strings.Contains(page, "<script>") {
		t.Error("an inline script")
	}
	if code, js := body(t, c, ts.URL+"/static/site.js"); code != 200 || !strings.Contains(js, "dexafix_sponsor") && !strings.Contains(js, "data-dismiss-cookie") {
		t.Errorf("site.js: %d", code)
	}
}

func TestStatusPageShowsTheOverallStateAndEachPart(t *testing.T) {
	ts, c := newTestServer(t)
	code, page := body(t, c, ts.URL+"/status")
	if code != 200 {
		t.Fatalf("status: %d", code)
	}
	for _, want := range []string{
		`class="status-hero status-hero-up"`, "All systems operational", `class="hero-mark"`,
		`class="status-grid"`, `class="comp-up"`, `class="state state-up"`, "Operational", "Checked ",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("status page lacks %q", want)
		}
	}
	// Not by colour alone: the state is in words, and the hero and each pill have a mark.
	if strings.Contains(page, "status-hero-down") {
		t.Error("the test server is not down")
	}
}
