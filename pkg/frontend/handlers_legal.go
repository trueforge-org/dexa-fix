package frontend

import "net/http"

// The places the footer and the legal pages link to. The source link is also how a visitor of this
// server gets its source code, which the AGPL-3.0 asks of whoever runs a modified copy.
const (
	repoURL    = "https://github.com/trueforge-org/dexafix"
	sponsorURL = "https://opencollective.com/dexafix"
)

// The Terms of Service and the Privacy Policy. Both are public (a visitor reads them before signing
// up) and the same for everybody; the text is in the templates.
func (s *Frontend) legalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /terms", s.legalPage("terms.html", "Terms of Service · DexaFix", "terms"))
	mux.HandleFunc("GET /privacy", s.legalPage("privacy.html", "Privacy Policy · DexaFix", "privacy"))
}

func (s *Frontend) legalPage(tmpl, title, tab string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		s.render(w, tmpl, http.StatusOK, pageData{Title: title, Tab: tab, User: u, Track: s.trackingFor(u)})
	}
}
