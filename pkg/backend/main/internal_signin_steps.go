package mainbackend

import (
	"crypto/hmac"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/must"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Internal endpoints for the rest of signing in: the second step, password reset and external
// providers. They answer like the password sign-in does; the sealed state cookies (mfa, provider
// state) are read from the visitor's Cookie header and set with Set-Cookie, which the frontend relays.

func (s *Backend) internalSignInStepRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix
	mux.HandleFunc("GET "+p+"/pages/login/2fa", s.internalLoginTwoFactorPage)
	mux.HandleFunc("POST "+p+"/auth/2fa", s.internalLoginTwoFactor)
	mux.HandleFunc("GET "+p+"/pages/login/2fa/setup", s.internalLoginSetupPage)
	mux.HandleFunc("POST "+p+"/auth/2fa/setup", s.internalLoginSetup)

	mux.HandleFunc("GET "+p+"/pages/password/forgot", s.internalForgotPage)
	mux.HandleFunc("POST "+p+"/auth/password/forgot", s.internalForgot)
	mux.HandleFunc("GET "+p+"/pages/password/reset", s.internalResetPage)
	mux.HandleFunc("POST "+p+"/auth/password/reset", s.internalReset)

	mux.HandleFunc("POST "+p+"/auth/providers/{provider}/start", s.internalProviderStart)
	mux.HandleFunc("POST "+p+"/auth/providers/{provider}/callback", s.internalProviderCallback)
}

// ---- second step of a sign-in ----

func secondStepRedirect(w http.ResponseWriter, target string) {
	internalJSON(w, http.StatusOK, apiclient.SecondStep{Redirect: target})
}

func secondStepPage(w http.ResponseWriter, v apiclient.SecondStep, status int) {
	if status != http.StatusOK {
		v.Status = status
	}
	internalJSON(w, http.StatusOK, v)
}

func (s *Backend) internalLoginTwoFactorPage(w http.ResponseWriter, r *http.Request) {
	st, _, _, ok := s.readMFA(r)
	if !ok || st.Enrol {
		secondStepRedirect(w, "/")
		return
	}
	secondStepPage(w, apiclient.SecondStep{Mode: "verify"}, http.StatusOK)
}

func (s *Backend) internalLoginTwoFactor(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	st, nonce, expires, ok := s.readMFA(r)
	if !ok || st.Enrol || s.nonces.seen(r.Context(), nonce) { // seen: the cookie already signed someone in
		secondStepRedirect(w, "/")
		return
	}
	if s.secondFactorRefused(r, st.UID) {
		s.auditAs(r, st.UID, st.UID, auditSecondFactorThrottled, "")
		secondStepPage(w, apiclient.SecondStep{Mode: "verify", Error: "Too many wrong codes. Please wait a few minutes and sign in again."}, http.StatusTooManyRequests)
		return
	}
	good, recovery, err := s.verifySecondFactor(ctx, st.UID, in.Code)
	if err != nil {
		internalError(w, err)
		return
	}
	if !good {
		s.limiter.Fail(r.Context(), ipKey(r))
		s.auditAs(r, st.UID, st.UID, auditSecondFactorFailed, "")
		secondStepPage(w, apiclient.SecondStep{Mode: "verify", Error: secondFactorMessage}, http.StatusUnauthorized)
		return
	}
	if !s.nonces.use(r.Context(), nonce, expires) { // the cookie was already used for a sign-in
		secondStepRedirect(w, "/")
		return
	}
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		internalError(w, err)
		return
	}
	s.limiter.Reset(r.Context(), mfaKey(st.UID))
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
		internalError(w, err)
		return
	}
	s.markAdminVerified(w, r, u.ID) // the code just entered counts for the Admin pages too
	secondStepRedirect(w, target)
}

func (s *Backend) internalLoginSetupPage(w http.ResponseWriter, r *http.Request) {
	st, _, _, ok := s.readMFA(r)
	if !ok || !st.Enrol {
		secondStepRedirect(w, "/")
		return
	}
	s.internalLoginSetupView(w, r, st, "", http.StatusOK)
}

func (s *Backend) internalLoginSetupView(w http.ResponseWriter, r *http.Request, st mfaState, errMsg string, status int) {
	ctx := r.Context()
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		internalError(w, err)
		return
	}
	secret, err := s.pendingTOTPSecret(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	setup := s.twoFactorSetupFor(u, secret)
	secondStepPage(w, apiclient.SecondStep{Mode: "setup", Error: errMsg, QR: string(setup.QR), Secret: setup.Secret}, status)
}

func (s *Backend) internalLoginSetup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	st, nonce, expires, ok := s.readMFA(r)
	if !ok || !st.Enrol || s.nonces.seen(r.Context(), nonce) {
		secondStepRedirect(w, "/")
		return
	}
	if s.secondFactorRefused(r, st.UID) {
		s.auditAs(r, st.UID, st.UID, auditSecondFactorThrottled, "")
		s.internalLoginSetupView(w, r, st, "Too many wrong codes. Please wait a few minutes and sign in again.", http.StatusTooManyRequests)
		return
	}
	t, err := s.store.GetTOTP(ctx, st.UID)
	if err != nil || t.Confirmed {
		secondStepRedirect(w, "/")
		return
	}
	step, good := checkTOTP(t.Secret, in.Code, time.Now())
	if !good {
		s.limiter.Fail(r.Context(), ipKey(r))
		s.auditAs(r, st.UID, st.UID, auditSecondFactorFailed, "setup")
		s.internalLoginSetupView(w, r, st, "That code is not right. Check that the app shows the entry for this site and try the next code.", http.StatusUnprocessableEntity)
		return
	}
	if !s.nonces.use(r.Context(), nonce, expires) {
		secondStepRedirect(w, "/")
		return
	}
	if err := s.store.ConfirmTOTP(ctx, st.UID, step); err != nil {
		internalError(w, err)
		return
	}
	codes, err := s.issueRecoveryCodes(ctx, st.UID)
	if err != nil {
		internalError(w, err)
		return
	}
	u, err := s.store.GetUser(ctx, st.UID)
	if err != nil {
		internalError(w, err)
		return
	}
	s.limiter.Reset(r.Context(), mfaKey(st.UID))
	clearMFACookie(w, r)
	s.auditAs(r, u.ID, u.ID, auditTOTPEnabled, "required")
	target, err := s.completeSignIn(w, r, u, st.Method)
	if err != nil {
		internalError(w, err)
		return
	}
	s.markAdminVerified(w, r, u.ID)
	// Signed in; the recovery codes are shown once, on this response, never in a redirect.
	secondStepPage(w, apiclient.SecondStep{Mode: "codes", Codes: codes, Continue: target}, http.StatusOK)
}

// ---- password reset ----

func resetStepPage(w http.ResponseWriter, v apiclient.ResetStep, status int) {
	if status != http.StatusOK {
		v.Status = status
	}
	internalJSON(w, http.StatusOK, v)
}

func resetOff(w http.ResponseWriter) {
	internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "signing in with a password is turned off")
}

func (s *Backend) internalForgotPage(w http.ResponseWriter, r *http.Request) {
	if !s.selfServiceReset(r.Context()) {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "404 page not found")
		return
	}
	resetStepPage(w, apiclient.ResetStep{Mode: "forgot"}, http.StatusOK)
}

// internalForgot answers the same, with the same status, whoever the address belongs to. The work
// (lookup, token, mail) runs in the background, so the response time says nothing either.
func (s *Backend) internalForgot(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if !s.selfServiceReset(ctx) {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "404 page not found")
		return
	}
	email, valid := parseEmail(in.Email)
	ipK, addrK := "reset-ip:"+ipKey(r), "reset-addr:"+email
	allowed := valid && !s.limiter.Blocked(r.Context(), ipK, maxResetIP) && !s.limiter.Blocked(r.Context(), addrK, maxResetAddr)
	// Every request counts, not only failures: this is a mail sender.
	s.limiter.Fail(r.Context(), ipK)
	if valid {
		s.limiter.Fail(r.Context(), addrK)
	}
	if allowed {
		base := s.cfg().baseURL
		s.bg.Start(func() { s.sendResetFor(email, base) })
	}
	resetStepPage(w, apiclient.ResetStep{Mode: "sent"}, http.StatusOK)
}

func (s *Backend) internalResetPage(w http.ResponseWriter, r *http.Request) {
	if !s.passwordEnabled(r.Context()) {
		resetOff(w)
		return
	}
	u, ok, err := s.resetUser(r, r.Header.Get(apiclient.HeaderResetToken))
	if err != nil {
		internalError(w, err)
		return
	}
	if !ok {
		resetStepPage(w, apiclient.ResetStep{Mode: "invalid"}, http.StatusNotFound)
		return
	}
	has, err := s.hasTwoFactor(r.Context(), u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	resetStepPage(w, apiclient.ResetStep{Mode: "form", NeedsCode: has}, http.StatusOK)
}

func (s *Backend) internalReset(w http.ResponseWriter, r *http.Request) {
	var in struct{ Password, Confirm, Code string }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if !s.passwordEnabled(ctx) {
		resetOff(w)
		return
	}
	if s.limiter.Blocked(r.Context(), ipKey(r), maxIPFail) {
		resetStepPage(w, apiclient.ResetStep{Mode: "invalid", Error: "Too many attempts. Please wait a few minutes."}, http.StatusTooManyRequests)
		return
	}
	token := r.Header.Get(apiclient.HeaderResetToken)
	u, ok, err := s.resetUser(r, token)
	if err != nil {
		internalError(w, err)
		return
	}
	if !ok {
		s.limiter.Fail(r.Context(), ipKey(r))
		s.auditAs(r, 0, 0, auditResetInvalid, "")
		resetStepPage(w, apiclient.ResetStep{Mode: "invalid"}, http.StatusNotFound)
		return
	}
	has, err := s.hasTwoFactor(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	form := func(msg string, status int) {
		resetStepPage(w, apiclient.ResetStep{Mode: "form", NeedsCode: has, Error: msg}, status)
	}
	if msg := validatePassword(in.Password, in.Confirm); msg != "" {
		form(msg, http.StatusUnprocessableEntity)
		return
	}
	if has {
		if s.secondFactorRefused(r, u.ID) {
			s.auditAs(r, u.ID, u.ID, auditSecondFactorThrottled, "reset")
			form("Too many wrong codes. Please wait a few minutes.", http.StatusTooManyRequests)
			return
		}
		good, _, err := s.verifySecondFactor(ctx, u.ID, in.Code)
		if err != nil {
			internalError(w, err)
			return
		}
		if !good {
			s.limiter.Fail(r.Context(), ipKey(r))
			s.auditAs(r, u.ID, u.ID, auditSecondFactorFailed, "reset")
			form(secondFactorMessage, http.StatusUnauthorized)
			return
		}
		s.limiter.Reset(r.Context(), mfaKey(u.ID))
	}
	hash := must.Value(hashPassword(in.Password)) // cannot fail: validatePassword capped the length at bcrypt's limit
	// The reset signs the user out everywhere. The sessions table is emptied by CompletePasswordReset
	// itself; in Valkey it is done here first, and the reset fails if it cannot be.
	if err := s.sessions.DeleteUserSessions(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	}
	if _, err := s.store.CompletePasswordReset(ctx, hashToken(token), hash); errors.Is(err, store.ErrNotFound) {
		resetStepPage(w, apiclient.ResetStep{Mode: "invalid"}, http.StatusNotFound) // used or expired a moment ago
		return
	} else if err != nil {
		internalError(w, err)
		return
	}
	// A sign-in with the old password between the delete above and the reset must not survive.
	if err := s.sessions.DeleteUserSessions(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	}
	s.auditAs(r, u.ID, u.ID, auditResetCompleted, "")
	var target string
	if has { // the code was just checked: no second prompt for the same sign-in
		target, err = s.completeSignIn(w, r, u, models.ProviderLocal)
		if err == nil {
			s.markAdminVerified(w, r, u.ID)
		}
	} else {
		target, err = s.signInTarget(w, r, u, models.ProviderLocal)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, apiclient.ResetStep{Redirect: target})
}

// ---- external providers ----

// internalProviderStart answers with the provider's address, to sign in or, for a signed-in visitor
// who asked to link, to link it. Linking needs the current password.
func (s *Backend) internalProviderStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Link    bool
		Current string
	}
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	u := currentUser(r)
	linking := in.Link && u != nil
	if linking && !s.internalConfirmPassword(w, r, *u, in.Current) {
		return // linking a provider needs the current password
	}
	p, err := s.store.GetAuthProvider(ctx, r.PathValue("provider"))
	if err != nil {
		internalError(w, err)
		return
	}
	cfg, _, err := s.oauthConfig(ctx, p, r)
	if err != nil {
		log.Error().Err(err).Str("provider", p.Slug).Msg("provider setup failed")
		signInFail(w, p.Label+" sign-in is not available right now.", http.StatusBadGateway)
		return
	}
	st := providerState{Slug: p.Slug, Verifier: oauth2.GenerateVerifier()}
	st.State = randomHex(16)
	st.Nonce = randomHex(16)
	if linking {
		st.Link = u.ID
	}
	sealed := s.sealState(st)
	setProviderCookie(w, r, sealed, int(providerStateTTL.Seconds()))
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(st.Verifier)}
	if p.Kind != models.ProviderKindOAuth2 {
		opts = append(opts, oidc.Nonce(st.Nonce))
	}
	if p.Kind == models.ProviderKindApple {
		opts = append(opts, oauth2.SetAuthURLParam("response_mode", "form_post"))
	}
	signInRedirect(w, cfg.AuthCodeURL(st.State, opts...))
}

func providerCallbackInvalid(w http.ResponseWriter) {
	internalFail(w, http.StatusBadRequest, apiclient.CodeInvalid, "invalid or expired sign-in, please start again")
}

// internalProviderCallback finishes a sign-in. Accounts are matched only by the provider's subject:
// never by name or e-mail, because that would let someone take over an account by claiming its
// address. The body holds every query and form parameter the provider sent.
func (s *Backend) internalProviderCallback(w http.ResponseWriter, r *http.Request) {
	var params map[string]string
	if !internalDecode(w, r, &params) {
		return
	}
	r.Form = url.Values{}
	for k, v := range params {
		r.Form.Set(k, v)
	}
	ctx := r.Context()
	slug := r.PathValue("provider")
	p, err := s.store.GetAuthProvider(ctx, slug)
	if err != nil {
		internalError(w, err)
		return
	}
	c, err := r.Cookie(providerCookie)
	if err != nil {
		providerCallbackInvalid(w)
		return
	}
	setProviderCookie(w, r, "", -1)
	st, nonce, expires, ok := s.openState(c.Value)
	if !ok || st.Slug != slug || !hmac.Equal([]byte(st.State), []byte(r.FormValue("state"))) {
		providerCallbackInvalid(w)
		return
	}
	if !s.nonces.use(ctx, nonce, expires) { // a state is good for one callback; fails closed
		providerCallbackInvalid(w)
		return
	}
	if r.FormValue("error") != "" || r.FormValue("code") == "" {
		s.internalProviderFailed(w, st, p.Label+" did not complete the sign-in.")
		return
	}
	subject, claims, err := s.exchangeCode(ctx, p, r, st)
	if err != nil {
		log.Warn().Err(err).Str("provider", slug).Msg("provider sign-in failed")
		s.internalProviderFailed(w, st, "Could not sign in with "+p.Label+".")
		return
	}
	id, err := s.store.GetIdentity(ctx, slug, subject)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, err)
		return
	}
	if st.Link != 0 {
		s.internalLinkIdentity(w, r, p, st.Link, subject, id, found)
		return
	}
	if found {
		u, err := s.store.GetUser(ctx, id.UserID)
		if err != nil {
			internalError(w, err)
			return
		}
		s.internalSignIn(w, r, u, slug)
		return
	}
	open, err := s.signupOpen(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	if !p.AllowSignup || !open {
		signInFail(w, "No account is linked to this "+p.Label+" login. Sign in another way and link it in Settings.", http.StatusForbidden)
		return
	}
	u, err := s.createProviderUser(ctx, p, subject, claims)
	if err == nil {
		s.auditAs(r, u.ID, u.ID, auditSignup, slug)
	}
	if errors.Is(err, store.ErrExists) { // never linked automatically: the provider may not own that address
		signInFail(w, "An account with that e-mail address already exists. Sign in and link "+p.Label+" in Settings.", http.StatusConflict)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.internalSignIn(w, r, u, slug)
}

func (s *Backend) internalProviderFailed(w http.ResponseWriter, st providerState, msg string) {
	if st.Link != 0 {
		signInRedirect(w, settingsPath(st.Link, msg))
		return
	}
	signInFail(w, msg, http.StatusBadGateway)
}

func (s *Backend) internalLinkIdentity(w http.ResponseWriter, r *http.Request, p models.AuthProvider, userID int64, subject string, existing models.Identity, found bool) {
	if found && existing.UserID != userID {
		signInRedirect(w, settingsPath(userID, "That "+p.Label+" login is already linked to another account."))
		return
	}
	err := s.store.PutIdentity(r.Context(), models.Identity{UserID: userID, Provider: p.Slug, Subject: subject})
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, userID, auditIdentityLinked, p.Slug)
	signInRedirect(w, settingsPath(userID, p.Label+" linked."))
}

// ---- reset helpers ----

func (s *Backend) sendResetFor(email string, base *url.URL) {
	ctx := s.bg.Ctx
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			log.Error().Err(err).Msg("password reset lookup")
		}
		return
	}
	if has, err := s.hasLocalPassword(ctx, u); err != nil || !has { // provider-only accounts have no password to reset
		return
	}
	token, _, err := s.newResetToken(ctx, u.ID, nil, resetEmailTTL)
	if err != nil {
		log.Error().Err(err).Msg("password reset token")
		return
	}
	link := base.Scheme + "://" + base.Host + resetPath + "?t=" + url.QueryEscape(token)
	s.auditBackground(u.ID, auditResetRequested, "")
	if !s.sendResetMail(u, link, resetEmailTTL) {
		s.auditBackground(u.ID, auditResetEmailFailed, "")
	}
}

func (s *Backend) resetUser(r *http.Request, token string) (models.User, bool, error) {
	if token == "" || len(token) > 128 {
		return models.User{}, false, nil
	}
	u, err := s.store.PasswordResetUser(r.Context(), hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return models.User{}, false, nil
	}
	return u, err == nil, err
}
