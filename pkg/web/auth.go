package web

import (
	"net/http"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

const (
	sessionCookie = "dexafix_session"
	sessionTTL    = 30 * 24 * time.Hour

	minPasswordLen   = 8
	maxPasswordBytes = 72 // bcrypt's limit

	tokenPrefix       = "dxf_"
	tokenPrefixShown  = 12
	settingCookieKey  = "auth.cookie_key"
	settingPasswordOn = "auth.password_login"
	settingSignup     = "auth.signup"
	valueDisabled     = "disabled"
	valueEnabled      = "enabled"
)

type userKey struct{}

// currentUser returns the authenticated user of the request, or nil.
func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userKey{}).(*models.User)
	return u
}

// ---- passwords ----

// ---- tokens and cookies ----

// ---- request guards ----

// user allows a request to act only on the signed-in user's own pages. Other users' pages answer
// 404 so that they do not reveal which ids exist.
func (s *Frontend) user(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || id != u.ID {
			http.NotFound(w, r)
			return
		}
		h(w, r, *u)
	}
}

// ---- sign-in page ----

type (
	loginProvider = apiclient.LoginProvider
	loginView     = apiclient.LoginPage
)

func (s *Frontend) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /signup", s.signup)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /auth/{provider}/login", s.providerStart)
	mux.HandleFunc("POST /auth/{provider}/login", s.providerStart)
	mux.HandleFunc("GET /auth/{provider}/callback", s.providerCallback)
	mux.HandleFunc("POST /auth/{provider}/callback", s.providerCallback)
	mux.HandleFunc("POST /u/{id}/profile", s.user(s.saveProfile))
	mux.HandleFunc("POST /u/{id}/password", s.user(s.changePassword))
	mux.HandleFunc("POST /u/{id}/tokens", s.user(s.createToken))
	mux.HandleFunc("POST /u/{id}/tokens/{item}/delete", s.user(s.deleteToken))
	mux.HandleFunc("POST /u/{id}/identities/{provider}/delete", s.user(s.unlinkIdentity))
}

func (s *Frontend) index(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		http.Redirect(w, r, userPath(u.ID, ""), http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, "", http.StatusOK)
}

func (s *Frontend) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	view, err := s.api.LoginPage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "index.html", status, pageData{Title: "Sign in · dexa-fix", Error: errMsg, Login: &view})
}

// signInOutcome continues after a step of signing in: to the page the backend named, or back to the
// sign-in page with its message. The backend's cookies are relayed with either.
func (s *Frontend) signInOutcome(w http.ResponseWriter, r *http.Request, o apiclient.SignInOutcome, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	if o.Redirect != "" {
		http.Redirect(w, r, o.Redirect, http.StatusSeeOther)
		return
	}
	s.renderLogin(w, r, o.Error, o.Status)
}

func (s *Frontend) login(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.Login(r.Context(), r.PostFormValue("email"), r.PostFormValue("password"))
	s.signInOutcome(w, r, o, err)
}

func (s *Frontend) signup(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.Signup(r.Context(), r.PostFormValue("name"), r.PostFormValue("email"), r.PostFormValue("password"), r.PostFormValue("confirm"))
	s.signInOutcome(w, r, o, err)
}

func (s *Frontend) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.api.Logout(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// flashParam reads the message passed to a page after a redirect.
func flashParam(r *http.Request) string {
	msg := r.URL.Query().Get("msg")
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return msg
}
