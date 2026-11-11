package frontend

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

func directive(policy, name string) []string {
	for _, d := range strings.Split(policy, ";") {
		if f := strings.Fields(d); len(f) > 0 && f[0] == name {
			return f[1:]
		}
	}
	return nil
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func TestTrackingPolicyIsWidenedOnlyForWhatIsOn(t *testing.T) {
	strict := edge.ContentSecurityPolicy

	// Nothing turned on: the policy is the strict one (trackingFor returns nil, render never calls csp).
	empty := (&trackingView{}).csp()
	if empty != strict {
		t.Errorf("an empty tracking view changes the policy:\n%s\n%s", empty, strict)
	}

	scarf := (&trackingView{Scarf: "id"}).csp()
	if !has(directive(scarf, "img-src"), "https://static.scarf.sh") {
		t.Errorf("Scarf needs its image host: %s", scarf)
	}
	if has(directive(scarf, "script-src"), "https://www.googletagmanager.com") || directive(scarf, "connect-src") != nil {
		t.Errorf("Scarf alone must not open anything for Google: %s", scarf)
	}
	if got := directive(scarf, "script-src"); len(got) != 1 || got[0] != "'self'" {
		t.Errorf("Scarf alone must leave script-src alone: %v", got)
	}

	ga := (&trackingView{GA: "G-ABC123XYZ9"}).csp()
	if !has(directive(ga, "script-src"), "https://www.googletagmanager.com") || !has(directive(ga, "connect-src"), "https://*.google-analytics.com") {
		t.Errorf("Google Analytics: %s", ga)
	}
	if !has(directive(ga, "connect-src"), "'self'") {
		t.Errorf("a new connect-src must keep 'self', or the page's own requests stop: %s", ga)
	}
	if has(directive(ga, "script-src"), "'unsafe-inline'") {
		t.Errorf("Google Analytics does not need inline scripts: %s", ga)
	}

	ads := (&trackingView{AdSense: "ca-pub-1234567890123456"}).csp()
	if !has(directive(ads, "script-src"), "https://pagead2.googlesyndication.com") || !has(directive(ads, "frame-src"), "https://tpc.googlesyndication.com") ||
		!has(directive(ads, "img-src"), "https:") || !has(directive(ads, "script-src"), "'unsafe-inline'") {
		t.Errorf("AdSense: %s", ads)
	}

	// Whatever is on, what makes the strict policy strict stays.
	for _, p := range []string{scarf, ga, ads, (&trackingView{Scarf: "a", GA: "b", AdSense: "c"}).csp()} {
		for _, must := range []string{"frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'", "object-src 'none'", "default-src 'self'"} {
			if !strings.Contains(p, must) {
				t.Errorf("%q lost from %s", must, p)
			}
		}
		if has(directive(p, "script-src"), "*") || has(directive(p, "script-src"), "https:") || has(directive(p, "script-src"), "'unsafe-eval'") {
			t.Errorf("script-src is wider than the services need: %s", p)
		}
	}
}

func TestTrackingIsOnlyOfferedToVisitorsWhoAreNotSignedIn(t *testing.T) {
	s := &Frontend{}
	s.swapSettings(&frontendSettings{tlsMode: apiclient.TLSOff})
	if s.trackingFor(nil) != nil {
		t.Error("offered although nothing is turned on")
	}

	s.swapSettings(&frontendSettings{tlsMode: apiclient.TLSOff, analytics: apiclient.Analytics{
		ScarfPixelID: "123e4567-e89b-12d3-a456-426614174000", GoogleAnalyticsID: "G-ABC123XYZ9", AdSensePublisher: "ca-pub-1234567890123456",
	}})
	tr := s.trackingFor(nil)
	if tr == nil || tr.Names != "Scarf, Google Analytics and Google AdSense" {
		t.Fatalf("visitor: %+v", tr)
	}
	if s.trackingFor(&models.User{ID: 1, Username: "alice"}) != nil {
		t.Error("offered to somebody who is signed in")
	}

	for _, c := range []struct {
		a    apiclient.Analytics
		want string
	}{
		{apiclient.Analytics{ScarfPixelID: "x"}, "Scarf"},
		{apiclient.Analytics{GoogleAnalyticsID: "x", AdSensePublisher: "y"}, "Google Analytics and Google AdSense"},
	} {
		s.swapSettings(&frontendSettings{tlsMode: apiclient.TLSOff, analytics: c.a})
		if got := s.trackingFor(nil).Names; got != c.want {
			t.Errorf("names of %+v = %q, want %q", c.a, got, c.want)
		}
	}
}

// consent.js is what stands between a visitor and the third parties. The harness runs it against a
// small fake page (testdata/consent_harness.js) where Node is installed.
func TestConsentScriptKeepsThePrivacyPromises(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/consent_harness.js", "static/consent.js").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "all checks passed") {
		t.Errorf("unexpected output: %s", out)
	}
}
