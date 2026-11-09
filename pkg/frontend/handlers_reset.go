package frontend

import (
	"net/http"

	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// Password reset.
//
// An administrator can always make a one-time link for a user and hand it over. When an SMTP server
// and the base URL are set (on the Admin page) people can also ask for the link by e-mail. A link
// works once and for a limited time, only a hash of it is stored, and using it signs the user out
// everywhere. The reset never skips two-step verification: a user who has it enters a code too, so
// somebody who only controls the mailbox cannot take the account over or sign its owner out.
const (
	resetPath = "/password/reset"
)

// resetView is the data of the reset pages.
type resetView struct {
	Mode      string // "forgot", "sent", "form" or "invalid"
	Error     string
	Token     string
	NeedsCode bool // the account has two-step verification
}

func (s *Frontend) resetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /password/forgot", s.forgotPage)
	mux.HandleFunc("POST /password/forgot", s.forgot)
	mux.HandleFunc("GET "+resetPath, s.resetPage)
	mux.HandleFunc("POST "+resetPath, s.reset)
	mux.HandleFunc("POST /admin/users/{id}/reset-link", s.admin(s.adminResetLink))
}

func (s *Frontend) renderReset(w http.ResponseWriter, v resetView, status int) {
	w.Header().Set("Referrer-Policy", "no-referrer") // the link carries the secret in its query
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "reset.html", status, pageData{Title: "Reset password · dexa-fix", Reset: &v})
}

// ---- administrator ----

// The one-time link of the Admin page is made by internalAdminResetLink; adminResetLink shows it.

// ---- asking for a link by e-mail ----

func (s *Frontend) resetStep(w http.ResponseWriter, r *http.Request, o apiclient.ResetStep, err error, token string) {
	if err != nil {
		s.fail(w, err)
		return
	}
	if o.Redirect != "" {
		http.Redirect(w, r, o.Redirect, http.StatusSeeOther)
		return
	}
	status := o.Status
	if status == 0 {
		status = http.StatusOK
	}
	v := resetView{Mode: o.Mode, Error: o.Error, NeedsCode: o.NeedsCode}
	if o.Mode == "form" {
		v.Token = token
	}
	s.renderReset(w, v, status)
}

func (s *Frontend) forgotPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.ResetForgotPage(r.Context())
	s.resetStep(w, r, o, err, "")
}

func (s *Frontend) forgot(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.ResetForgot(r.Context(), r.PostFormValue("email"))
	s.resetStep(w, r, o, err, "")
}

func (s *Frontend) resetPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	o, err := s.api.ResetFormPage(r.Context(), token)
	s.resetStep(w, r, o, err, token)
}

func (s *Frontend) reset(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("t")
	o, err := s.api.Reset(r.Context(), token, r.PostFormValue("password"), r.PostFormValue("confirm"), r.PostFormValue("code"))
	s.resetStep(w, r, o, err, token)
}
