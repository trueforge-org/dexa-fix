package web

import (
	"html/template"
	"net/http"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// Two-step verification with a one-time password from an authenticator app.
//
// It protects signing in with a password. Providers (OIDC, Apple, OAuth2) are not asked for a
// second step: they are trusted to apply their own, and a code that only exists for the local
// password would confuse people who sign in with a provider. An administrator who needs more
// enforces it at the provider. A passkey sign-in already is two factors (the device and its
// unlock), so it never asks either.
const (
	settingRequire2FA   = "auth.require_2fa"
	require2FAOff       = "off"
	require2FAAdmins    = "admins"
	require2FAEveryone  = "everyone"
	mfaCookie           = "dexafix_mfa"
	sealMFA             = "mfa"
	mfaTTL              = 5 * time.Minute
	maxMFAFail          = 5 // wrong codes per account within the failure window
	defaultTOTPIssuer   = "dexa-fix"
	secondFactorMessage = "That code is not right. Try the next code, or use a recovery code."
)

// twoFactorView is the data of the sign-in second-step page.
type twoFactorView struct {
	Mode     string // "verify", "setup" or "codes"
	Error    string
	QR       template.HTML
	Secret   string
	Codes    []string
	Continue string // where to go after the recovery codes were seen
}

func (s *Server) twoFactorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login/2fa", s.loginTwoFactorPage)
	mux.HandleFunc("POST /login/2fa", s.loginTwoFactor)
	mux.HandleFunc("GET /login/2fa/setup", s.loginSetupPage)
	mux.HandleFunc("POST /login/2fa/setup", s.loginSetup)
	mux.HandleFunc("POST /u/{id}/2fa/begin", s.user(s.twoFactorBegin))
	mux.HandleFunc("POST /u/{id}/2fa/confirm", s.user(s.twoFactorConfirm))
	mux.HandleFunc("POST /u/{id}/2fa/disable", s.user(s.twoFactorDisable))
	mux.HandleFunc("POST /u/{id}/2fa/recovery", s.user(s.twoFactorRecovery))
	mux.HandleFunc("GET /admin/verify", s.adminBasic(s.adminVerifyPage))
	mux.HandleFunc("POST /admin/verify", s.adminBasic(s.adminVerify))
	mux.HandleFunc("POST /admin/two-factor", s.admin(s.adminTwoFactorPolicy))
	mux.HandleFunc("POST /admin/users/{id}/2fa/delete", s.admin(s.adminRemoveTwoFactor))
}

// ---- second step of a sign-in ----

func (s *Server) renderSecondStep(w http.ResponseWriter, v twoFactorView, status int) {
	s.render(w, "twofactor.html", status, pageData{Title: "Two-step verification · dexa-fix", TwoFactor: &v})
}

// stepPage renders what the backend answered for a step of the second step of signing in, or
// redirects. The backend's cookies are relayed with either.
func (s *Server) stepPage(w http.ResponseWriter, r *http.Request, o apiclient.SecondStep, err error) {
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
	s.renderSecondStep(w, twoFactorView{Mode: o.Mode, Error: o.Error, QR: template.HTML(o.QR), Secret: o.Secret, Codes: o.Codes, Continue: o.Continue}, status)
}

func (s *Server) loginTwoFactorPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.LoginSecondStepPage(r.Context())
	s.stepPage(w, r, o, err)
}

func (s *Server) loginTwoFactor(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.LoginSecondStep(r.Context(), r.PostFormValue("code"))
	s.stepPage(w, r, o, err)
}

// ---- required setup during sign-in ----

func (s *Server) loginSetupPage(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.LoginSetupPage(r.Context())
	s.stepPage(w, r, o, err)
}

func (s *Server) loginSetup(w http.ResponseWriter, r *http.Request) {
	o, err := s.api.LoginSetup(r.Context(), r.PostFormValue("code"))
	s.stepPage(w, r, o, err)
}

// ---- administration ----

func (s *Server) adminTwoFactorPolicy(w http.ResponseWriter, r *http.Request, _ models.User) {
	v := r.PostFormValue("require")
	switch v {
	case require2FAOff, require2FAAdmins, require2FAEveryone:
	default:
		adminRedirect(w, r, "Choose who must use two-step verification.")
		return
	}
	if err := s.store.PutSetting(r.Context(), settingRequire2FA, v); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "require_2fa="+v)
	switch v {
	case require2FAAdmins:
		adminRedirect(w, r, "Administrators must now use two-step verification when they sign in with a password. They set it up at their next sign-in.")
	case require2FAEveryone:
		adminRedirect(w, r, "Everyone must now use two-step verification when they sign in with a password. They set it up at their next sign-in.")
	default:
		adminRedirect(w, r, "Two-step verification is optional.")
	}
}

// adminRemoveTwoFactor lets an administrator remove the second step of a user who lost their
// device and their recovery codes. The user is asked to set it up again if the policy requires it.
func (s *Server) adminRemoveTwoFactor(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTOTP(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, t.ID, auditTOTPRemovedByAdmin, "")
	adminRedirect(w, r, "Two-step verification was removed for "+t.Name+".")
}

// ---- settings ----

// twoFactorChange shows the outcome of a change on the settings page. A refusal that carries the setup
// (the QR code is shown again) comes as a TwoFactorChange.
func (s *Server) twoFactorChange(w http.ResponseWriter, r *http.Request, u models.User, ch apiclient.TwoFactorChange, err error, saved string) {
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	if ch.Error != "" {
		s.renderSettingsWith(w, r, u, ch.Error, "", "", ch.Status, func(v *securityView) { v.TwoFactor.Setup = ch.Setup })
		return
	}
	s.renderSettingsWith(w, r, u, "", saved, "", http.StatusOK, func(v *securityView) {
		v.TwoFactor.Setup = ch.Setup
		v.TwoFactor.NewCodes = ch.Codes // shown once, never in a URL
	})
}

func (s *Server) twoFactorBegin(w http.ResponseWriter, r *http.Request, u models.User) {
	ch, err := s.api.TwoFactorBegin(r.Context(), u.ID, r.PostFormValue("current"))
	s.twoFactorChange(w, r, u, ch, err, "")
}

func (s *Server) twoFactorConfirm(w http.ResponseWriter, r *http.Request, u models.User) {
	ch, err := s.api.TwoFactorConfirm(r.Context(), u.ID, r.PostFormValue("code"))
	s.twoFactorChange(w, r, u, ch, err, "Two-step verification is on.")
}

func (s *Server) twoFactorDisable(w http.ResponseWriter, r *http.Request, u models.User) {
	if err := s.api.TwoFactorDisable(r.Context(), u.ID, r.PostFormValue("current"), r.PostFormValue("code")); err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	settingsRedirect(w, r, u.ID, "Two-step verification is off.")
}

func (s *Server) twoFactorRecovery(w http.ResponseWriter, r *http.Request, u models.User) {
	ch, err := s.api.TwoFactorRecovery(r.Context(), u.ID, r.PostFormValue("current"), r.PostFormValue("code"))
	s.twoFactorChange(w, r, u, ch, err, "New recovery codes created. The old ones no longer work.")
}

// ---- the Admin pages ask again ----

// adminVerifyPage asks for a code when the administrator has to prove the second step again.
func (s *Server) adminVerifyPage(w http.ResponseWriter, r *http.Request, _ models.User) {
	need, err := s.api.AdminStepUp(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if !need {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.renderSecondStep(w, twoFactorView{Mode: "admin"}, http.StatusOK)
}

func (s *Server) adminVerify(w http.ResponseWriter, r *http.Request, _ models.User) {
	o, err := s.api.AdminVerify(r.Context(), r.PostFormValue("code"))
	s.stepPage(w, r, o, err)
}
