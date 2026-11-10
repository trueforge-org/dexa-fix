package mainbackend

import (
	"errors"
	"net/http"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/must"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Internal endpoints for signing in. A step answers with a SignInOutcome; the cookies it sets
// travel back in Set-Cookie headers, which the frontend relays to the browser unchanged.

func (s *Backend) internalSignInRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix
	mux.HandleFunc("GET "+p+"/pages/login", s.internalLoginPage)
	mux.HandleFunc("POST "+p+"/auth/password", s.internalLogin)
	mux.HandleFunc("POST "+p+"/auth/signup", s.internalSignup)
	s.internalSignInStepRoutes(mux)
}

func signInFail(w http.ResponseWriter, msg string, status int) {
	internalJSON(w, http.StatusOK, apiclient.SignInOutcome{Error: msg, Status: status})
}

func signInRedirect(w http.ResponseWriter, target string) {
	internalJSON(w, http.StatusOK, apiclient.SignInOutcome{Redirect: target})
}

func (s *Backend) internalLoginPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hasUsers, err := s.store.HasUsers(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	view := apiclient.LoginPage{PasswordEnabled: s.passwordEnabled(ctx), FirstUser: !hasUsers, CanReset: s.selfServiceReset(ctx), Passkeys: passkeysReady(r)}
	for _, p := range providers {
		view.Providers = append(view.Providers, apiclient.LoginProvider{Slug: p.Slug, Label: p.Label})
	}
	if view.SignupOpen, err = s.signupOpen(ctx); err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, view)
}

// internalSignIn is signIn for the internal API: it starts the session (or the second step) and
// answers where to continue.
func (s *Backend) internalSignIn(w http.ResponseWriter, r *http.Request, u models.User, method string) {
	target, err := s.signInTarget(w, r, u, method)
	if err != nil {
		internalError(w, err)
		return
	}
	signInRedirect(w, target)
}

func (s *Backend) internalLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Email, Password string }
	if !internalDecode(w, r, &in) {
		return
	}
	if !s.passwordEnabled(r.Context()) {
		signInFail(w, "Signing in with a password is turned off.", http.StatusForbidden)
		return
	}
	u, ok, err := s.throttledVerify(r, in.Email, in.Password)
	if errors.Is(err, errThrottled) {
		s.auditAs(r, 0, 0, auditLoginThrottled, "")
		signInFail(w, "Too many failed attempts. Please wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	if err != nil {
		internalError(w, err)
		return
	}
	if !ok {
		s.auditAs(r, 0, 0, auditLoginFailed, "")
		signInFail(w, "Wrong e-mail address or password.", http.StatusUnauthorized)
		return
	}
	s.internalSignIn(w, r, u, models.ProviderLocal)
}

func (s *Backend) internalSignup(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, Email, Password, Confirm string }
	if !internalDecode(w, r, &in) {
		return
	}
	if s.limiter.Blocked(r.Context(), ipKey(r), maxIPFail) {
		signInFail(w, "Too many attempts. Please wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	if !s.passwordEnabled(r.Context()) {
		signInFail(w, "Creating an account with a password is turned off.", http.StatusForbidden)
		return
	}
	if open, err := s.signupOpen(r.Context()); err != nil {
		internalError(w, err)
		return
	} else if !open {
		signInFail(w, "New accounts are closed. Ask your administrator for one.", http.StatusForbidden)
		return
	}
	email, ok := parseEmail(in.Email)
	if !ok {
		signInFail(w, "Please enter a valid e-mail address.", http.StatusUnprocessableEntity)
		return
	}
	if msg := validatePassword(in.Password, in.Confirm); msg != "" {
		signInFail(w, msg, http.StatusUnprocessableEntity)
		return
	}
	hash := must.Value(hashPassword(in.Password)) // cannot fail: validatePassword capped the length at bcrypt's limit
	u, err := s.createUserWithIdentity(r.Context(), in.Name, email, models.Identity{Provider: models.ProviderLocal, Secret: hash})
	switch {
	case errors.Is(err, store.ErrExists):
		s.limiter.Fail(r.Context(), ipKey(r)) // repeated probing for registered addresses is throttled
		signInFail(w, "An account with that e-mail address already exists.", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrInvalid):
		signInFail(w, "Please enter a name of 1-64 characters.", http.StatusUnprocessableEntity)
	case err != nil:
		internalError(w, err)
	default:
		s.auditAs(r, u.ID, u.ID, auditSignup, models.ProviderLocal)
		s.internalSignIn(w, r, u, models.ProviderLocal)
	}
}
