package backend

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The backend half of two-step verification: the state of an account, the checks of codes, the
// signed cookies, and the step-up proof for the Admin pages.

// mfaState is carried in a signed cookie between the password check and the second step. It is
// not a session: it proves only that the password was right a moment ago.
type mfaState struct {
	UID    int64  `json:"u"`
	Method string `json:"m"`
	Enrol  bool   `json:"e,omitempty"` // the user has to set up two-step verification first
}

// ---- policy and state ----

func (s *Backend) twoFactorPolicy(ctx context.Context) string {
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
func (s *Backend) twoFactorRequired(ctx context.Context, u models.User) bool {
	switch s.twoFactorPolicy(ctx) {
	case require2FAEveryone:
		return true
	case require2FAAdmins:
		return u.IsAdmin()
	}
	return false
}

// hasTwoFactor reports whether the user finished setting up two-step verification.
func (s *Backend) hasTwoFactor(ctx context.Context, userID int64) (bool, error) {
	t, err := s.store.GetTOTP(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil && t.Confirmed, err
}

// secondFactorBlocked reports whether too many wrong codes were tried for the account or from the
// client address.
func (s *Backend) secondFactorBlocked(r *http.Request, userID int64) bool {
	return s.limiter.Blocked(mfaKey(userID), maxMFAFail) || s.limiter.Blocked(ipKey(r), maxIPFail)
}

func mfaKey(userID int64) string { return "mfa:" + strconv.FormatInt(userID, 10) }

// verifySecondFactor checks a six-digit code or a recovery code. A code works once: the step is
// recorded atomically, so two requests with the same code cannot both succeed.
func (s *Backend) verifySecondFactor(ctx context.Context, userID int64, input string) (ok, recovery bool, err error) {
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
func (s *Backend) pendingTOTPSecret(ctx context.Context, userID int64) (string, error) {
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

func (s *Backend) totpIssuer() string {
	if base := s.cfg().baseURL; base != nil && base.Hostname() != "" {
		return base.Hostname()
	}
	return defaultTOTPIssuer
}

func (s *Backend) twoFactorSetupFor(u models.User, secret string) (*twoFactorSetup, error) {
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
func (s *Backend) issueRecoveryCodes(ctx context.Context, userID int64) ([]string, error) {
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

func (s *Backend) setMFACookie(w http.ResponseWriter, r *http.Request, st mfaState) error {
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

func (s *Backend) readMFA(r *http.Request) (st mfaState, nonce string, expires time.Time, ok bool) {
	c, err := r.Cookie(mfaCookie)
	if err != nil {
		return st, "", time.Time{}, false
	}
	nonce, expires, ok = s.openValue(sealMFA, c.Value, &st)
	return st, nonce, expires, ok && st.UID != 0
}

// secondStep is called by signInTarget for a password sign-in. It returns the page to continue on
// when a second step (or a required setup) comes first, and "" when the sign-in can finish.
func (s *Backend) secondStep(w http.ResponseWriter, r *http.Request, u models.User, method string) (string, error) {
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

// hasLocalPassword reports whether the user can sign in with a password.
func (s *Backend) hasLocalPassword(ctx context.Context, u models.User) (bool, error) {
	_, err := s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
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

func (s *Backend) markAdminVerified(w http.ResponseWriter, r *http.Request, userID int64) {
	sealed, _, err := s.sealValue(sealAdminStep, adminStepUp{UID: userID}, adminStepUpTTL)
	if err != nil {
		return // the admin is asked again; nothing else depends on it
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminCookie, Value: sealed, Path: "/admin", MaxAge: int(adminStepUpTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isSecure(r),
	})
}

func (s *Backend) adminVerified(r *http.Request, userID int64) bool {
	c, err := r.Cookie(adminCookie)
	if err != nil {
		return false
	}
	var st adminStepUp
	_, _, ok := s.openValue(sealAdminStep, c.Value, &st)
	return ok && st.UID == userID
}

// needsAdminStepUp reports whether the administrator has to enter a code before the Admin pages.
func (s *Backend) needsAdminStepUp(r *http.Request, u models.User) (bool, error) {
	has, err := s.hasTwoFactor(r.Context(), u.ID)
	if err != nil || !has {
		return false, err
	}
	return !s.adminVerified(r, u.ID), nil
}

// ---- the internal endpoints ----

func (s *Backend) internalTwoFactorRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/users/{id}/2fa"
	mux.HandleFunc("POST "+p+"/begin", s.internalUser(s.internalTwoFactorBegin))
	mux.HandleFunc("POST "+p+"/confirm", s.internalUser(s.internalTwoFactorConfirm))
	mux.HandleFunc("POST "+p+"/disable", s.internalUser(s.internalTwoFactorDisable))
	mux.HandleFunc("POST "+p+"/recovery", s.internalUser(s.internalTwoFactorRecovery))
	mux.HandleFunc("GET "+apiclient.Prefix+"/admin/step-up", s.internalAdminBasic(s.internalAdminStepUp))
	mux.HandleFunc("POST "+apiclient.Prefix+"/admin/step-up", s.internalAdminBasic(s.internalAdminVerify))
}

// internalAdminBasic allows only administrators, without the step-up proof: for the endpoints that
// ask for it. It answers 401 with CodeUnauthorized when nobody is signed in and 403 with
// CodeForbidden when the user is not an administrator.
func (s *Backend) internalAdminBasic(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			internalFail(w, http.StatusUnauthorized, apiclient.CodeUnauthorized, "sign in first")
			return
		}
		if !u.IsAdmin() {
			internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "not found")
			return
		}
		h(w, r, *u)
	}
}

// internalAdmin is the guard of every administrator endpoint of the internal API: the user must be
// signed in and an administrator, and an administrator with two-step verification on must have
// proved it recently (the step-up cookie from the visitor's request). It answers CodeUnauthorized,
// CodeForbidden or, when the proof is missing, 403 with CodeStepUp. The frontend's own checks are
// a convenience; this is the one that counts.
func (s *Backend) internalAdmin(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return s.internalAdminBasic(func(w http.ResponseWriter, r *http.Request, u models.User) {
		need, err := s.needsAdminStepUp(r, u)
		if err != nil {
			internalError(w, err)
			return
		}
		if need {
			internalFail(w, http.StatusForbidden, apiclient.CodeStepUp, "enter a verification code first")
			return
		}
		h(w, r, u)
	})
}

func (s *Backend) internalAdminStepUp(w http.ResponseWriter, r *http.Request, u models.User) {
	need, err := s.needsAdminStepUp(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, map[string]bool{"needed": need})
}

func (s *Backend) internalAdminVerify(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	if s.secondFactorBlocked(r, u.ID) {
		s.auditAs(r, u.ID, u.ID, auditSecondFactorThrottled, "admin")
		secondStepPage(w, apiclient.SecondStep{Mode: "admin", Error: "Too many wrong codes. Please wait a few minutes."}, http.StatusTooManyRequests)
		return
	}
	good, _, err := s.verifySecondFactor(r.Context(), u.ID, in.Code)
	if err != nil {
		internalError(w, err)
		return
	}
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "admin")
		secondStepPage(w, apiclient.SecondStep{Mode: "admin", Error: secondFactorMessage}, http.StatusUnauthorized)
		return
	}
	s.limiter.Reset(mfaKey(u.ID))
	s.markAdminVerified(w, r, u.ID)
	s.audit(r, u.ID, auditAdminVerified, "")
	secondStepRedirect(w, "/admin")
}

// ---- settings ----

// internalCheckSecondFactor verifies the code for a change on the settings page. It reports whether
// to go on; if not, the answer has been written.
func (s *Backend) internalCheckSecondFactor(w http.ResponseWriter, r *http.Request, u models.User, code string) bool {
	if s.secondFactorBlocked(r, u.ID) {
		s.auditAs(r, u.ID, u.ID, auditSecondFactorThrottled, "")
		internalFail(w, http.StatusTooManyRequests, apiclient.CodeThrottled, "Too many wrong codes. Try again later.")
		return false
	}
	good, _, err := s.verifySecondFactor(r.Context(), u.ID, code)
	if err != nil {
		internalError(w, err)
		return false
	}
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "settings")
		internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, secondFactorMessage)
		return false
	}
	s.limiter.Reset(mfaKey(u.ID))
	return true
}

func invalid2FA(w http.ResponseWriter, msg string) {
	internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, msg)
}

func (s *Backend) internalTwoFactorBegin(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Current string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if has, err := s.hasLocalPassword(ctx, u); err != nil {
		internalError(w, err)
		return
	} else if !has || !s.passwordEnabled(ctx) {
		invalid2FA(w, "Two-step verification protects signing in with a password, which this account does not use.")
		return
	}
	if !s.internalConfirmPassword(w, r, u, in.Current) {
		return
	}
	secret, err := s.pendingTOTPSecret(ctx, u.ID)
	if errors.Is(err, store.ErrExists) {
		invalid2FA(w, "Two-step verification is already on.")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	setup, err := s.twoFactorSetupFor(u, secret)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, apiclient.TwoFactorChange{Setup: setup})
}

func (s *Backend) internalTwoFactorConfirm(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	t, err := s.store.GetTOTP(ctx, u.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Confirmed) {
		invalid2FA(w, "Start the setup first.")
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	// A refusal shows the same secret again, so the person can try the next code.
	refuse := func(msg string, status int) {
		setup, serr := s.twoFactorSetupFor(u, t.Secret)
		if serr != nil {
			internalError(w, serr)
			return
		}
		internalJSON(w, http.StatusOK, apiclient.TwoFactorChange{Error: msg, Status: status, Setup: setup})
	}
	if s.secondFactorBlocked(r, u.ID) {
		refuse("Too many wrong codes. Try again later.", http.StatusTooManyRequests)
		return
	}
	step, good := checkTOTP(t.Secret, in.Code, time.Now())
	if !good {
		s.limiter.Fail(mfaKey(u.ID))
		s.limiter.Fail(ipKey(r))
		s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "setup")
		refuse("That code is not right. Check that the app shows the entry for this site and try the next code.", http.StatusUnprocessableEntity)
		return
	}
	if err := s.store.ConfirmTOTP(ctx, u.ID, step); err != nil {
		internalError(w, err)
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	s.limiter.Reset(mfaKey(u.ID))
	s.audit(r, u.ID, auditTOTPEnabled, "")
	internalJSON(w, http.StatusOK, apiclient.TwoFactorChange{Codes: codes})
}

func (s *Backend) internalTwoFactorDisable(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Current, Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if s.twoFactorRequired(ctx, u) {
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "Your administrator requires two-step verification.")
		return
	}
	if !s.internalConfirmPassword(w, r, u, in.Current) || !s.internalCheckSecondFactor(w, r, u, in.Code) {
		return
	}
	if err := s.store.DeleteTOTP(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditTOTPDisabled, "")
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalTwoFactorRecovery(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Current, Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if has, err := s.hasTwoFactor(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	} else if !has {
		invalid2FA(w, "Two-step verification is not on.")
		return
	}
	if !s.internalConfirmPassword(w, r, u, in.Current) || !s.internalCheckSecondFactor(w, r, u, in.Code) {
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditRecoveryRegenerated, "")
	internalJSON(w, http.StatusOK, apiclient.TwoFactorChange{Codes: codes})
}
