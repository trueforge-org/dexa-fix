package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
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

// mfaState is carried in a signed cookie between the password check and the second step. It is
// not a session: it proves only that the password was right a moment ago.
type mfaState struct {
	UID    int64  `json:"u"`
	Method string `json:"m"`
	Enrol  bool   `json:"e,omitempty"` // the user has to set up two-step verification first
}

// twoFactorView is the data of the sign-in second-step page.
type twoFactorView struct {
	Mode     string // "verify", "setup" or "codes"
	Error    string
	QR       template.HTML
	Secret   string
	Codes    []string
	Continue string // where to go after the recovery codes were seen
}

// twoFactorSetup is a secret waiting to be confirmed.
type twoFactorSetup struct {
	QR     template.HTML
	Secret string
}

// twoFactorSettings is the two-step part of the settings page.
type twoFactorSettings struct {
	Available    bool // the user signs in with a password, so there is something to protect
	Enabled      bool
	Required     bool // an administrator requires it for this user
	RecoveryLeft int
	Setup        *twoFactorSetup
	NewCodes     []string // shown once
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

// ---- policy and state ----

func (s *Server) twoFactorPolicy(ctx context.Context) string {
	v, err := s.store.GetSetting(ctx, settingRequire2FA)
	if err != nil {
		return require2FAOff
	}
	switch v {
	case require2FAAdmins, require2FAEveryone:
		return v
	}
	return require2FAOff
}

// twoFactorRequired reports whether the policy covers the user.
func (s *Server) twoFactorRequired(ctx context.Context, u models.User) bool {
	switch s.twoFactorPolicy(ctx) {
	case require2FAEveryone:
		return true
	case require2FAAdmins:
		return u.IsAdmin()
	}
	return false
}

// hasTwoFactor reports whether the user finished setting up two-step verification.
func (s *Server) hasTwoFactor(ctx context.Context, userID int64) (bool, error) {
	t, err := s.store.GetTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && t.Confirmed, err
}

// secondFactorBlocked reports whether too many wrong codes were tried for the account or from the
// client address.
func (s *Server) secondFactorBlocked(r *http.Request, userID int64) bool {
	return s.limiter.Blocked(mfaKey(userID), maxMFAFail) || s.limiter.Blocked(ipKey(r), maxIPFail)
}

func mfaKey(userID int64) string { return "mfa:" + strconv.FormatInt(userID, 10) }

// verifySecondFactor checks a six-digit code or a recovery code. A code works once: the step is
// recorded atomically, so two requests with the same code cannot both succeed.
func (s *Server) verifySecondFactor(ctx context.Context, userID int64, input string) (ok, recovery bool, err error) {
	if looksLikeTOTP(input) {
		t, err := s.store.GetTOTP(ctx, userID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !t.Confirmed) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		step, good := checkTOTP(t.Secret, input, time.Now())
		if !good || step <= t.LastStep {
			return false, false, nil
		}
		fresh, err := s.store.UseTOTPStep(ctx, userID, step)
		return fresh, false, err
	}
	if strings.TrimSpace(input) == "" {
		return false, false, nil
	}
	ok, err = s.store.UseRecoveryCode(ctx, userID, recoveryHash(input))
	return ok, true, err
}

// pendingTOTPSecret returns the secret of a setup that is not confirmed yet, creating one when
// there is none, so reloading the page keeps showing the same QR code.
func (s *Server) pendingTOTPSecret(ctx context.Context, userID int64) (string, error) {
	t, err := s.store.GetTOTP(ctx, userID)
	switch {
	case err == nil && t.Confirmed:
		return "", store.ErrExists
	case err == nil:
		return t.Secret, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}
	secret, err := newTOTPSecret()
	if err != nil {
		return "", err
	}
	return secret, s.store.PutPendingTOTP(ctx, userID, secret)
}

func (s *Server) totpIssuer() string {
	if base := s.cfg().baseURL; base != nil && base.Hostname() != "" {
		return base.Hostname()
	}
	return defaultTOTPIssuer
}

func (s *Server) twoFactorSetupFor(u models.User, secret string) (*twoFactorSetup, error) {
	account := u.Email
	if account == "" {
		account = u.Name
	}
	svg, err := qrSVG(otpauthURI(s.totpIssuer(), account, secret))
	if err != nil {
		return nil, err
	}
	return &twoFactorSetup{QR: svg, Secret: groupSecret(secret)}, nil
}

// groupSecret writes the secret in groups of four, which is easier to type. Apps ignore spaces.
func groupSecret(secret string) string {
	var b strings.Builder
	for i, c := range secret {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// issueRecoveryCodes replaces the user's recovery codes with a new set and returns it.
func (s *Server) issueRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
	codes, err := newRecoveryCodes(recoveryCodeCount)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = recoveryHash(c)
	}
	return codes, s.store.ReplaceRecoveryCodes(ctx, userID, hashes)
}

// ---- the signed cookie between password and second step ----

func (s *Server) setMFACookie(w http.ResponseWriter, r *http.Request, st mfaState) error {
	sealed, _, err := s.sealValue(sealMFA, st, mfaTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: mfaCookie, Value: sealed, Path: "/login/", MaxAge: int(mfaTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
	return nil
}

func clearMFACookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: mfaCookie, Path: "/login/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r)})
}

func (s *Server) readMFA(r *http.Request) (st mfaState, nonce string, expires time.Time, ok bool) {
	c, err := r.Cookie(mfaCookie)
	if err != nil {
		return st, "", time.Time{}, false
	}
	nonce, expires, ok = s.openValue(sealMFA, c.Value, &st)
	return st, nonce, expires, ok && st.UID != 0
}

// secondStep is called by signInTarget for a password sign-in. It returns the page to continue on
// when a second step (or a required setup) comes first, and "" when the sign-in can finish.
func (s *Server) secondStep(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
	ctx := r.Context()
	has, err := s.hasTwoFactor(ctx, u.ID)
	if err != nil {
		return "", err
	}
	switch {
	case has:
		return "/login/2fa", s.setMFACookie(w, r, mfaState{UID: u.ID, Method: method})
	case s.twoFactorRequired(ctx, u):
		return "/login/2fa/setup", s.setMFACookie(w, r, mfaState{UID: u.ID, Method: method, Enrol: true})
	}
	return "", nil
}

// ---- second step of a sign-in ----

func (s *Server) renderSecondStep(w http.ResponseWriter, v twoFactorView, status int) {
	s.render(w, "twofactor.html", status, pageData{Title: "Two-step verification · dexa-fix", TwoFactor: &v})
}

func (s *Server) loginTwoFactorPage(w http.ResponseWriter, r *http.Request) {
	st, _, _, ok := s.readMFA(r)
	if !ok || st.Enrol {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderSecondStep(w, twoFactorView{Mode: "verify"}, http.StatusOK)
}

func (s *Server) loginTwoFactor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, nonce, expires, ok := s.readMFA(r)
	if !ok || st.Enrol || s.nonces.seen(nonce) { // seen: the cookie already signed someone in
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.secondFactorBlocked(r, st.UID) {
		s.auditAs(r, st.UID, st.UID, auditSecondFactorThrottled, "")
		s.renderSecondStep(w, twoFactorView{Mode: "verify", Error: "Too many wrong codes. Please wait a few minutes and sign in again."}, http.StatusTooManyRequests)
		return
	}
	good, recovery, err := s.verifySecondFactor(ctx, st.UID, r.PostFormValue("code"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !good {
		s.limiter.Fail(mfaKey(st.UID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, st.UID, st.UID, auditSecondFactorFailed, "")
		s.renderSecondStep(w, twoFactorView{Mode: "verify", Error: secondFactorMessage}, http.StatusUnauthorized)
		return
	}
	if !s.nonces.use(nonce, expires) { // the cookie was already used for a sign-in
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.limiter.Reset(mfaKey(st.UID))
	clearMFACookie(w, r)
	if recovery {
		left, _ := s.store.CountRecoveryCodes(ctx, u.ID)
		s.auditAs(r, u.ID, u.ID, auditRecoveryUsed, "")
		if left == 0 {
			s.auditAs(r, u.ID, u.ID, auditRecoveryUsed, "last")
		}
	}
	target, err := s.completeSignIn(w, r, u, st.Method)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.markAdminVerified(w, r, u.ID) // the code just entered counts for the Admin pages too
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// ---- required setup during sign-in ----

func (s *Server) loginSetupPage(w http.ResponseWriter, r *http.Request) {
	st, _, _, ok := s.readMFA(r)
	if !ok || !st.Enrol {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.renderLoginSetup(w, r, st, "", http.StatusOK)
}

func (s *Server) renderLoginSetup(w http.ResponseWriter, r *http.Request, st mfaState, errMsg string, status int) {
	ctx := r.Context()
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		s.fail(w, err)
		return
	}
	secret, err := s.pendingTOTPSecret(ctx, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	setup, err := s.twoFactorSetupFor(u, secret)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSecondStep(w, twoFactorView{Mode: "setup", Error: errMsg, QR: setup.QR, Secret: setup.Secret}, status)
}

func (s *Server) loginSetup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, nonce, expires, ok := s.readMFA(r)
	if !ok || !st.Enrol || s.nonces.seen(nonce) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.secondFactorBlocked(r, st.UID) {
		s.auditAs(r, st.UID, st.UID, auditSecondFactorThrottled, "")
		s.renderLoginSetup(w, r, st, "Too many wrong codes. Please wait a few minutes and sign in again.", http.StatusTooManyRequests)
		return
	}
	t, err := s.store.GetTOTP(ctx, st.UID)
	if err != nil || t.Confirmed {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	step, good := checkTOTP(t.Secret, r.PostFormValue("code"), time.Now())
	if !good {
		s.limiter.Fail(mfaKey(st.UID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, st.UID, st.UID, auditSecondFactorFailed, "setup")
		s.renderLoginSetup(w, r, st, "That code is not right. Check that the app shows the entry for this site and try the next code.", http.StatusUnprocessableEntity)
		return
	}
	if !s.nonces.use(nonce, expires) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.store.ConfirmTOTP(ctx, st.UID, step); err != nil {
		s.fail(w, err)
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, st.UID)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.limiter.Reset(mfaKey(st.UID))
	clearMFACookie(w, r)
	s.auditAs(r, u.ID, u.ID, auditTOTPEnabled, "required")
	target, err := s.completeSignIn(w, r, u, st.Method)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.markAdminVerified(w, r, u.ID)
	// Signed in; the recovery codes are shown once, on this response, never in a redirect.
	s.renderSecondStep(w, twoFactorView{Mode: "codes", Codes: codes, Continue: target}, http.StatusOK)
}

// ---- settings ----

// checkSecondFactor verifies the code in the form field "code" for a change on the settings page.
// When it reports false the response has been written.
func (s *Server) checkSecondFactor(w http.ResponseWriter, r *http.Request, u models.User) bool {
	if s.secondFactorBlocked(r, u.ID) {
		s.auditAs(r, u.ID, u.ID, auditSecondFactorThrottled, "")
		s.settingsPage(w, r, u, "Too many wrong codes. Try again later.", "", http.StatusTooManyRequests)
		return false
	}
	good, _, err := s.verifySecondFactor(r.Context(), u.ID, r.PostFormValue("code"))
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "settings")
		s.settingsPage(w, r, u, secondFactorMessage, "", http.StatusUnprocessableEntity)
		return false
	}
	s.limiter.Reset(mfaKey(u.ID))
	return true
}

// hasLocalPassword reports whether the user can sign in with a password.
func (s *Server) hasLocalPassword(ctx context.Context, u models.User) (bool, error) {
	_, err := s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Server) twoFactorBegin(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	if has, err := s.hasLocalPassword(ctx, u); err != nil {
		s.fail(w, err)
		return
	} else if !has || !s.passwordEnabled(ctx) {
		s.settingsPage(w, r, u, "Two-step verification protects signing in with a password, which this account does not use.", "", http.StatusUnprocessableEntity)
		return
	}
	if !s.confirmPassword(w, r, u) {
		return
	}
	secret, err := s.pendingTOTPSecret(ctx, u.ID)
	if errors.Is(err, store.ErrExists) {
		s.settingsPage(w, r, u, "Two-step verification is already on.", "", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	setup, err := s.twoFactorSetupFor(u, secret)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderSettingsWith(w, r, u, "", "", "", http.StatusOK, func(v *securityView) { v.TwoFactor.Setup = setup })
}

func (s *Server) twoFactorConfirm(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	t, err := s.store.GetTOTP(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Confirmed) {
		s.settingsPage(w, r, u, "Start the setup first.", "", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(msg string, status int) {
		setup, serr := s.twoFactorSetupFor(u, t.Secret)
		if serr != nil {
			s.fail(w, serr)
			return
		}
		s.renderSettingsWith(w, r, u, msg, "", "", status, func(v *securityView) { v.TwoFactor.Setup = setup })
	}
	if s.secondFactorBlocked(r, u.ID) {
		fail("Too many wrong codes. Try again later.", http.StatusTooManyRequests)
		return
	}
	step, good := checkTOTP(t.Secret, r.PostFormValue("code"), time.Now())
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "setup")
		fail("That code is not right. Check that the app shows the entry for this site and try the next code.", http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.ConfirmTOTP(ctx, u.ID, step); err != nil {
		s.fail(w, err)
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.limiter.Reset(mfaKey(u.ID))
	s.audit(r, u.ID, auditTOTPEnabled, "")
	s.renderSettingsWith(w, r, u, "", "Two-step verification is on.", "", http.StatusOK, func(v *securityView) { v.TwoFactor.NewCodes = codes })
}

func (s *Server) twoFactorDisable(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	if s.twoFactorRequired(ctx, u) {
		s.settingsPage(w, r, u, "Your administrator requires two-step verification.", "", http.StatusForbidden)
		return
	}
	if !s.confirmPassword(w, r, u) || !s.checkSecondFactor(w, r, u) {
		return
	}
	if err := s.store.DeleteTOTP(ctx, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditTOTPDisabled, "")
	settingsRedirect(w, r, u.ID, "Two-step verification is off.")
}

func (s *Server) twoFactorRecovery(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	if has, err := s.hasTwoFactor(ctx, u.ID); err != nil {
		s.fail(w, err)
		return
	} else if !has {
		s.settingsPage(w, r, u, "Two-step verification is not on.", "", http.StatusUnprocessableEntity)
		return
	}
	if !s.confirmPassword(w, r, u) || !s.checkSecondFactor(w, r, u) {
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditRecoveryRegenerated, "")
	s.renderSettingsWith(w, r, u, "", "New recovery codes created. The old ones no longer work.", "", http.StatusOK, func(v *securityView) { v.TwoFactor.NewCodes = codes })
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

// ---- the Admin pages ask again ----

// An administrator with two-step verification on is asked for a code again before the Admin pages,
// even though they are signed in: the Admin pages can change everyone's account, so a stolen
// session cookie alone must not be enough. The proof is a signed cookie for 15 minutes, set here
// and after the second step of a sign-in (so it is not asked twice in a row). It covers /admin only.
// The JSON API for administrators takes access tokens, which have no second step.
const (
	adminCookie    = "dexafix_admin"
	sealAdminStep  = "admin-step-up"
	adminStepUpTTL = 15 * time.Minute
)

type adminStepUp struct {
	UID int64 `json:"u"`
}

func (s *Server) markAdminVerified(w http.ResponseWriter, r *http.Request, userID int64) {
	sealed, _, err := s.sealValue(sealAdminStep, adminStepUp{UID: userID}, adminStepUpTTL)
	if err != nil {
		return // the admin is asked again; nothing else depends on it
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: sealed, Path: "/admin", MaxAge: int(adminStepUpTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
}

func (s *Server) adminVerified(r *http.Request, userID int64) bool {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	var st adminStepUp
	_, _, ok := s.openValue(sealAdminStep, c.Value, &st)
	return ok && st.UID == userID
}

// needsAdminStepUp reports whether the administrator has to enter a code before the Admin pages.
func (s *Server) needsAdminStepUp(r *http.Request, u models.User) (bool, error) {
	has, err := s.hasTwoFactor(r.Context(), u.ID)
	if err != nil || !has {
		return false, err
	}
	return !s.adminVerified(r, u.ID), nil
}

func (s *Server) adminVerifyPage(w http.ResponseWriter, r *http.Request, u models.User) {
	if need, err := s.needsAdminStepUp(r, u); err != nil {
		s.fail(w, err)
		return
	} else if !need {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.renderSecondStep(w, twoFactorView{Mode: "admin"}, http.StatusOK)
}

func (s *Server) adminVerify(w http.ResponseWriter, r *http.Request, u models.User) {
	if s.secondFactorBlocked(r, u.ID) {
		s.auditAs(r, u.ID, u.ID, auditSecondFactorThrottled, "admin")
		s.renderSecondStep(w, twoFactorView{Mode: "admin", Error: "Too many wrong codes. Please wait a few minutes."}, http.StatusTooManyRequests)
		return
	}
	good, _, err := s.verifySecondFactor(r.Context(), u.ID, r.PostFormValue("code"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "admin")
		s.renderSecondStep(w, twoFactorView{Mode: "admin", Error: secondFactorMessage}, http.StatusUnauthorized)
		return
	}
	s.limiter.Reset(mfaKey(u.ID))
	s.markAdminVerified(w, r, u.ID)
	s.audit(r, u.ID, auditAdminVerified, "")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
