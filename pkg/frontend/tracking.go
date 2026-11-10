package frontend

import (
	"strings"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/edge"
)

// Optional analytics and ads (Scarf, Google Analytics, Google AdSense). An administrator turns them
// on with identifiers on the Admin page. The rules, which the privacy policy repeats:
//
//   - only on the pages a visitor sees before signing in (trackingFor is called by those handlers
//     and by no other), and never for somebody who is signed in;
//   - nothing is loaded until the visitor presses Accept (static/consent.js);
//   - the content security policy is widened for those pages and for what is turned on, and for
//     nothing else; every other page keeps the strict policy of edge.SecureHeaders.

// trackingView is what a public page needs to offer the choice and, after consent, to load the
// services.
type trackingView struct {
	Scarf, GA, AdSense string
	Names              string // "Scarf and Google Analytics", for the notice
}

// trackingFor returns the services to offer on a public page, or nil: for a signed-in person, and
// when none is turned on.
func (s *Frontend) trackingFor(u *models.User) *trackingView {
	if u != nil {
		return nil
	}
	a := s.cfg().analytics
	if !a.Any() {
		return nil
	}
	t := &trackingView{Scarf: a.ScarfPixelID, GA: a.GoogleAnalyticsID, AdSense: a.AdSensePublisher}
	var names []string
	if t.Scarf != "" {
		names = append(names, "Scarf")
	}
	if t.GA != "" {
		names = append(names, "Google Analytics")
	}
	if t.AdSense != "" {
		names = append(names, "Google AdSense")
	}
	switch len(names) {
	case 1:
		t.Names = names[0]
	default:
		t.Names = strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	return t
}

// csp is the content security policy of a page that offers these services: the strict one, plus the
// addresses of the services that are on. Google's own guidance for AdSense also needs inline
// scripts and images from any HTTPS address; that is added only when AdSense is on.
func (t *trackingView) csp() string {
	add := map[string][]string{}
	if t.Scarf != "" {
		add["img-src"] = append(add["img-src"], "https://static.scarf.sh")
	}
	if t.GA != "" {
		add["script-src"] = append(add["script-src"], "https://www.googletagmanager.com")
		add["connect-src"] = append(add["connect-src"], "https://*.google-analytics.com", "https://*.analytics.google.com", "https://*.googletagmanager.com")
		add["img-src"] = append(add["img-src"], "https://*.google-analytics.com", "https://*.googletagmanager.com")
	}
	if t.AdSense != "" {
		add["script-src"] = append(add["script-src"], "'unsafe-inline'", "https://pagead2.googlesyndication.com",
			"https://googleads.g.doubleclick.net", "https://www.googletagservices.com", "https://partner.googleadservices.com", "https://tpc.googlesyndication.com")
		add["connect-src"] = append(add["connect-src"], "https://pagead2.googlesyndication.com", "https://googleads.g.doubleclick.net")
		add["frame-src"] = append(add["frame-src"], "https://googleads.g.doubleclick.net", "https://tpc.googlesyndication.com", "https://www.google.com")
		add["img-src"] = append(add["img-src"], "https:")
	}
	return widenCSP(edge.ContentSecurityPolicy, add)
}

// widenCSP adds sources to the directives of a policy. A directive the policy does not have yet
// falls back to default-src, so it starts from 'self' to keep what it allowed before.
func widenCSP(policy string, add map[string][]string) string {
	parts := strings.Split(policy, ";")
	seen := map[string]bool{}
	for i, p := range parts {
		fields := strings.Fields(p)
		if len(fields) == 0 {
			continue
		}
		seen[fields[0]] = true
		for _, v := range add[fields[0]] {
			fields = appendOnce(fields, v)
		}
		parts[i] = " " + strings.Join(fields, " ")
	}
	for _, name := range []string{"connect-src", "frame-src"} { // fixed order: the header is the same every time
		if seen[name] || len(add[name]) == 0 {
			continue
		}
		fields := []string{name, "'self'"}
		for _, v := range add[name] {
			fields = appendOnce(fields, v)
		}
		parts = append(parts, " "+strings.Join(fields, " "))
	}
	return strings.TrimSpace(strings.Join(parts, ";"))
}

func appendOnce(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}
