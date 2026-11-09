package web

import (
	"net/http"
	"net/url"
	"runtime"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// The Admin pages. The frontend parses the forms and renders the page; the backend (internal_admin.go)
// does every check and change. The page's data comes from one call, and the frontend adds only what
// it alone knows: its own version and whether its listener could bind.

type (
	resetLinkView = apiclient.ResetLink
)

// adminView is the data of the Admin page: the backend's page plus the frontend's own parts.
type adminView struct {
	apiclient.AdminPage
	Version   string
	GoVersion string
	TLS       tlsView
	ResetLink *resetLinkView
}

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.admin(s.adminHome))
	mux.HandleFunc("POST /admin/users", s.admin(s.adminAddUser))
	mux.HandleFunc("POST /admin/users/{id}/role", s.admin(s.adminSetRole))
	mux.HandleFunc("POST /admin/users/{id}/strengthlevel", s.admin(s.adminSetStrengthLevel))
	mux.HandleFunc("POST /admin/users/{id}/strengthlevel-bulk", s.admin(s.adminSetStrengthLevelBulk))
	mux.HandleFunc("POST /admin/users/{id}/password", s.admin(s.adminSetPassword))
	mux.HandleFunc("POST /admin/users/{id}/delete", s.admin(s.adminDeleteUser))
	mux.HandleFunc("POST /admin/password-login", s.admin(s.adminPasswordLogin))
	s.adminSettingRoutes(mux)
	s.adminTLSRoutes(mux)
	mux.HandleFunc("POST /admin/signup", s.admin(s.adminSignup))
	mux.HandleFunc("POST /admin/providers", s.admin(s.adminSaveProvider))
	mux.HandleFunc("POST /admin/providers/{slug}/delete", s.admin(s.adminDeleteProvider))
	mux.HandleFunc("POST /admin/withings", s.admin(s.adminSaveWithings))
}

// admin restricts a page to administrators. It answers 404 to everyone else so that the page does
// not advertise itself. It is adminPage under the name the routes use.
func (s *Server) admin(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return s.adminPage(h)
}

// adminPage is the frontend guard of the Admin pages. An administrator with two-step verification
// on is sent to /admin/verify for a code first; the backend decides (GET /admin/step-up) and
// enforces the same on every admin endpoint (internalAdmin), so this redirect is a convenience.
func (s *Server) adminPage(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return s.adminBasic(func(w http.ResponseWriter, r *http.Request, u models.User) {
		need, err := s.api.AdminStepUp(r.Context())
		if err != nil {
			s.fail(w, err)
			return
		}
		if need {
			http.Redirect(w, r, "/admin/verify", http.StatusSeeOther)
			return
		}
		h(w, r, u)
	})
}

// adminBasic is admin without the second step: for the page that asks for it.
func (s *Server) adminBasic(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !u.IsAdmin() {
			http.NotFound(w, r)
			return
		}
		h(w, r, *u)
	}
}

func adminRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// adminDone answers a change: it shows the backend's message on the Admin page. A change of the
// settings this process acts on (base URL, proxies, TLS) is fetched first, so it applies before
// the redirect.
func (s *Server) adminDone(w http.ResponseWriter, r *http.Request, res apiclient.AdminResult, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	s.afterAdminChange(r, res)
	adminRedirect(w, r, res.Message)
}

func (s *Server) afterAdminChange(r *http.Request, res apiclient.AdminResult) {
	if !res.ConfigChanged {
		return
	}
	if err := s.RefreshConfig(r.Context()); err != nil {
		log.Warn().Err(err).Msg("could not fetch the new settings from the backend; the poll will retry")
	}
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request, u models.User) {
	s.renderAdmin(w, r, u, nil)
}

// renderAdmin shows the Admin page; link is a reset link that is shown once.
func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, u models.User, link *resetLinkView) {
	page, err := s.api.AdminPage(r.Context(), r.URL.Query().Get("edit"))
	if err != nil {
		s.fail(w, err)
		return
	}
	view := &adminView{AdminPage: page, Version: s.Version, GoVersion: runtime.Version(), ResetLink: link,
		TLS: tlsView{TLSView: page.TLS, ListenError: s.listenError()}}
	if view.Version == "" {
		view.Version = "dev"
	}
	s.render(w, "admin.html", http.StatusOK, pageData{Title: "Admin · dexa-fix", Tab: "admin", User: &u, Msg: flashParam(r), Admin: view})
}

// targetID reads the user id of the path; it answers 404 when it is not a number.
func targetID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func (s *Server) adminAddUser(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminAddUser(r.Context(), r.PostFormValue("name"), r.PostFormValue("email"), r.PostFormValue("password"))
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSignup(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminSignup(r.Context(), r.PostFormValue("enabled") == "1")
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSetRole(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminSetRole(r.Context(), id, r.PostFormValue("role"))
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSetStrengthLevel(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminSetStrengthLevel(r.Context(), id, r.PostFormValue("enabled") == "1")
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSetStrengthLevelBulk(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminSetStrengthLevelBulk(r.Context(), id, r.PostFormValue("enabled") == "1")
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSetPassword(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminSetPassword(r.Context(), id, r.PostFormValue("password"))
	s.adminDone(w, r, res, err)
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request, _ models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminDeleteUser(r.Context(), id)
	s.adminDone(w, r, res, err)
}

func (s *Server) adminPasswordLogin(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminPasswordLogin(r.Context(), r.PostFormValue("enabled") == "1")
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSaveProvider(w http.ResponseWriter, r *http.Request, _ models.User) {
	f := r.PostFormValue
	res, err := s.api.AdminSaveProvider(r.Context(), apiclient.ProviderForm{
		Slug: f("slug"), Label: f("label"), Kind: f("kind"), Issuer: f("issuer"),
		AuthURL: f("auth_url"), TokenURL: f("token_url"), UserinfoURL: f("userinfo_url"),
		ClientID: f("client_id"), ClientSecret: f("client_secret"),
		AppleTeamID: f("apple_team_id"), AppleKeyID: f("apple_key_id"),
		Scopes: f("scopes"), SubjectClaim: f("subject_claim"), NameClaim: f("name_claim"),
		AllowSignup: f("allow_signup") != "",
	})
	s.adminDone(w, r, res, err)
}

func (s *Server) adminDeleteProvider(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminDeleteProvider(r.Context(), r.PathValue("slug"))
	s.adminDone(w, r, res, err)
}

func (s *Server) adminSaveWithings(w http.ResponseWriter, r *http.Request, _ models.User) {
	res, err := s.api.AdminSaveWithings(r.Context(), apiclient.WithingsForm{
		ClientID: r.PostFormValue("client_id"), RedirectURL: r.PostFormValue("redirect_url"),
		ClientSecret: r.PostFormValue("client_secret"), ClearSecret: r.PostFormValue("clear_secret") != "",
	})
	s.adminDone(w, r, res, err)
}

// adminResetLink makes a one-time reset link and shows it on the Admin page.
func (s *Server) adminResetLink(w http.ResponseWriter, r *http.Request, admin models.User) {
	id, ok := targetID(w, r)
	if !ok {
		return
	}
	res, err := s.api.AdminResetLink(r.Context(), id, r.PostFormValue("email") != "")
	if err != nil {
		s.fail(w, err)
		return
	}
	if res.ResetLink == nil {
		adminRedirect(w, r, res.Message)
		return
	}
	s.renderAdmin(w, r, admin, res.ResetLink)
}
