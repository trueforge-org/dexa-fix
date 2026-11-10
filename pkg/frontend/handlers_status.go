package frontend

import (
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// The status page: the health of each part of the deployment. /status is the same for everybody
// (a state per part, nothing else), so it is safe to show to visitors and to monitors, and a CDN
// has nothing to tell visitors apart by. /admin/status is a page of its own with the detail for
// administrators: the workers by name and version, and the reason a part is down. The backend
// decides what each may see; this only renders.

type statusRow struct {
	Label, State, StateText, Note string
}

type statusView struct {
	Overall, OverallText string
	CheckedAt            string
	Rows                 []statusRow
	Workers              []apiclient.StatusWorker // /admin/status only
	Detail               bool                     // this is /admin/status
	AdminLink            bool                     // offer /admin/status: the visitor is an administrator
}

var stateText = map[string]string{
	apiclient.StateUp: "Operational", apiclient.StateDegraded: "Degraded", apiclient.StateDown: "Down", apiclient.StateOff: "Not in use",
}

var overallText = map[string]string{
	apiclient.StateUp: "All systems operational", apiclient.StateDegraded: "Some systems are degraded", apiclient.StateDown: "Some systems are down",
}

func (s *Frontend) statusRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", s.statusPage)
	mux.HandleFunc("GET /admin/status", s.admin(s.adminStatusPage))
}

// newStatusView adds the web process itself to what the backend reported. A backend that cannot be
// asked is down, and the parts it reports on are unknown, so they are left out.
func newStatusView(st *apiclient.Status, notes map[string]string) *statusView {
	web := apiclient.StatusComponent{Name: "web", Label: "Web", State: apiclient.StateUp}
	all := apiclient.Status{Components: []apiclient.StatusComponent{web}}
	v := &statusView{}
	if st == nil {
		all.Components = append(all.Components, apiclient.StatusComponent{Name: "api", Label: "API", State: apiclient.StateDown})
		v.CheckedAt = "now"
		notes = map[string]string{"api": "the web process cannot reach the api process"}
	} else {
		all.Components = append(all.Components, st.Components...)
		v.CheckedAt = st.CheckedAt
	}
	for _, c := range all.Components {
		v.Rows = append(v.Rows, statusRow{Label: c.Label, State: c.State, StateText: stateText[c.State], Note: notes[c.Name]})
	}
	v.Overall = all.Overall()
	v.OverallText = overallText[v.Overall]
	return v
}

// statusCode is 503 when something is down, so that a monitor needs no parsing; a degraded or an
// unused part still answers 200.
func statusCode(v *statusView) int {
	if v.Overall == apiclient.StateDown {
		return http.StatusServiceUnavailable
	}
	return http.StatusOK
}

func (s *Frontend) statusPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.api.Status(r.Context())
	if err != nil {
		log.Warn().Err(err).Msg("status: the backend could not be asked")
		st = nil
	}
	v := newStatusView(st, nil)
	u := currentUser(r)
	v.AdminLink = u != nil && u.IsAdmin()
	s.render(w, "status.html", statusCode(v), pageData{Title: "Status · DexaFix", Tab: "status", User: u, Status: v, Track: s.trackingFor(u)})
}

func (s *Frontend) adminStatusPage(w http.ResponseWriter, r *http.Request, u models.User) {
	d, err := s.api.AdminStatus(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	v := newStatusView(&d.Status, d.Notes)
	v.Workers, v.Detail = d.Workers, true
	s.render(w, "status.html", statusCode(v), pageData{Title: "Status · DexaFix", Tab: "admin", User: &u, Status: v})
}
