package mainbackend

import (
	"net/http"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// withingsView reports the Withings state for a user. Unlike oauthProvider it also returns a view
// when the server lacks credentials, so the pages can say so instead of showing nothing.
func (s *Backend) withingsView(r *http.Request, u models.User) *integrationView {
	p, ok := s.providerNamed(providerWithings)
	if !ok || p.OAuth == nil {
		return nil
	}
	configured, _ := p.OAuth.Configured(r.Context(), s)
	v := &integrationView{Configured: configured}
	v.Connected = configured && s.connected(r.Context(), p, u.ID)
	if v.Connected {
		v.AutoPaused = s.pausedReason(r.Context(), u.ID, kindWithingsImport)
	}
	return v
}
