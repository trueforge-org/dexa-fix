package web

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// Password reset.
//
// An administrator can always make a one-time link for a user and hand it over. When an SMTP server
// and the base URL are set (on the Admin page) people can also ask for the link by e-mail. A link
// works once and for a limited time, only a hash of it is stored, and using it signs the user out
// everywhere. The reset never skips two-step verification: a user who has it enters a code too, so
// somebody who only controls the mailbox cannot take the account over or sign its owner out.
const (
	resetAdminTTL = 24 * time.Hour
	resetEmailTTL = time.Hour

	maxResetIP   = 10 // reset e-mails per client address within the failure window
	maxResetAddr = 3  // reset e-mails per e-mail address within the failure window
	resetPath    = "/password/reset"
)

// resetView is the data of the reset pages.
type resetView struct {
	Mode      string // "forgot", "sent", "form" or "invalid"
	Error     string
	Token     string
	NeedsCode bool // the account has two-step verification
}

// resetLinkView is the one-time link shown to the administrator who made it.
type resetLinkView struct {
	For     string
	URL     string
	Expires string
	Emailed bool
	Warning bool // made from the request's host, which the request controls
}

func (s *Server) resetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /password/forgot", s.forgotPage)
	mux.HandleFunc("POST /password/forgot", s.forgot)
	mux.HandleFunc("GET "+resetPath, s.resetPage)
	mux.HandleFunc("POST "+resetPath, s.reset)
	mux.HandleFunc("POST /admin/users/{id}/reset-link", s.admin(s.adminResetLink))
}

func (s *Server) renderReset(w http.ResponseWriter, v resetView, status int) {
	w.Header().Set("Referrer-Policy", "no-referrer") // the link carries the secret in its query
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "reset.html", status, pageData{Title: "Reset password · dexa-fix", Reset: &v})
}

// resetURL builds the link for a token. With emailed set it uses the configured base URL only: an
// e-mailed link must never be built from the Host header of the request that asked for it.
func (s *Server) resetURL(r *http.Request, token string, emailed bool) (link string, fromRequest bool) {
	q := "?t=" + url.QueryEscape(token)
	if base := s.cfg().baseURL; base != nil {
		return base.Scheme + "://" + base.Host + resetPath + q, false
	}
	if emailed {
		return "", true
	}
	return externalURL(r, resetPath) + q, true
}

func (s *Server) newResetToken(ctx context.Context, userID int64, createdBy *int64, ttl time.Duration) (string, time.Time, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(ttl)
	return token, expires, s.store.CreatePasswordReset(ctx, hashToken(token), userID, createdBy, expires)
}

func resetMail(name, link string, ttl time.Duration) (subject, body string) {
	return "Reset your dexa-fix password",
		"Hello " + name + ",\n\n" +
			"Someone asked to reset the password of your dexa-fix account. To choose a new password, open this link within " +
			ttl.String() + ":\n\n" + link + "\n\n" +
			"If you did not ask for this, ignore this e-mail: your password stays as it is.\n"
}

// ---- administrator ----

func (s *Server) adminResetLink(w http.ResponseWriter, r *http.Request, admin models.User) {
	ctx := r.Context()
	if !s.passwordEnabled(ctx) {
		adminRedirect(w, r, "Password sign-in is off, so there is nothing to reset.")
		return
	}
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	token, expires, err := s.newResetToken(ctx, t.ID, &admin.ID, resetAdminTTL)
	if err != nil {
		s.fail(w, err)
		return
	}
	emailed := false
	if r.PostFormValue("email") != "" && s.cfg().mailer != nil && s.cfg().baseURL != nil && t.Email != "" {
		if link, _ := s.resetURL(r, token, true); link != "" {
			emailed = s.sendResetMail(t, link, resetAdminTTL)
		}
	}
	link, fromRequest := s.resetURL(r, token, false)
	s.audit(r, t.ID, auditResetLinkCreated, "")
	s.renderAdmin(w, r, admin, func(v *adminView) {
		v.ResetLink = &resetLinkView{For: t.Name, URL: link, Expires: expires.UTC().Format("2006-01-02 15:04 UTC"), Emailed: emailed, Warning: fromRequest}
	})
}

// sendResetMail sends the mail now and reports whether it went out.
func (s *Server) sendResetMail(u models.User, link string, ttl time.Duration) bool {
	m := s.cfg().mailer
	if m == nil || u.Email == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(s.jobCtx, 40*time.Second)
	defer cancel()
	subject, body := resetMail(u.Name, link, ttl)
	if err := m.Send(ctx, u.Email, subject, body); err != nil {
		log.Warn().Err(err).Int64("user", u.ID).Msg("could not send the password reset e-mail")
		return false
	}
	return true
}

// ---- asking for a link by e-mail ----

func (s *Server) resetStep(w http.ResponseWriter, r *http.Request, o apiclient.ResetStep, err error, token string) {
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

func (s *Server) forgotPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.ResetForgotPage(r.Context())
	s.resetStep(w, r, o, err, "")
}

func (s *Server) forgot(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.ResetForgot(r.Context(), r.PostFormValue("email"))
	s.resetStep(w, r, o, err, "")
}

func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	o, err := s.api.ResetFormPage(r.Context(), token)
	s.resetStep(w, r, o, err, token)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	token := r.PostFormValue("t")
	o, err := s.api.Reset(r.Context(), token, r.PostFormValue("password"), r.PostFormValue("confirm"), r.PostFormValue("code"))
	s.resetStep(w, r, o, err, token)
}

// forgetResets drops outstanding links after the password or e-mail address changed by other means.
func (s *Server) forgetResets(ctx context.Context, userID int64) {
	if err := s.store.DeletePasswordResets(ctx, userID); err != nil {
		log.Error().Err(err).Int64("user", userID).Msg("could not remove password reset links")
	}
}
