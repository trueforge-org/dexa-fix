package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Internal endpoints for the account section of the settings page: profile, password, access
// tokens, linked accounts, and signing out.

func (s *Backend) internalSecurityRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/users/{id}"
	mux.HandleFunc("POST "+p+"/profile", s.internalUser(s.internalProfileSave))
	mux.HandleFunc("POST "+p+"/password", s.internalUser(s.internalPasswordChange))
	mux.HandleFunc("POST "+p+"/tokens", s.internalUser(s.internalTokenCreate))
	mux.HandleFunc("DELETE "+p+"/tokens/{item}", s.internalUser(s.internalTokenDelete))
	mux.HandleFunc("DELETE "+p+"/identities/{provider}", s.internalUser(s.internalIdentityUnlink))
	mux.HandleFunc("DELETE "+apiclient.Prefix+"/session", s.internalLogout)
}

// internalConfirmPassword is confirmPassword for the internal API. It reports whether to go on; if
// not, the answer has been written.
func (s *Backend) internalConfirmPassword(w http.ResponseWriter, r *http.Request, u models.User, current string) bool {
	if r.PostForm == nil {
		r.PostForm = map[string][]string{}
	}
	r.PostForm.Set("current", current) // checkReauth reads the password from the form field
	ok, status, msg, err := s.checkReauth(r, u, false)
	switch {
	case err != nil:
		internalError(w, err)
		return false
	case !ok:
		code := apiclient.CodeInvalid
		if status == http.StatusTooManyRequests {
			code = apiclient.CodeThrottled
		}
		internalFail(w, status, code, msg)
		return false
	}
	return true
}

func (s *Backend) internalProfileSave(w http.ResponseWriter, r *http.Request, u models.User) {
	var in apiclient.Profile
	if !internalDecode(w, r, &in) {
		return
	}
	invalid := func(msg string) { internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, msg) }
	email, ok := parseEmail(in.Email)
	if !ok {
		invalid("Please enter a valid e-mail address.")
		return
	}
	if email != u.Email && !s.internalConfirmPassword(w, r, u, in.Current) {
		return
	}
	timezone := u.Timezone
	if raw := strings.TrimSpace(in.Timezone); raw != "" {
		var err error
		if timezone, err = parseTimezone(raw); err != nil {
			invalid(err.Error())
			return
		}
	}
	var height *float64
	if raw := strings.TrimSpace(in.HeightCm); raw != "" {
		v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
		if err != nil || v < minHeightCm || v > maxHeightCm {
			invalid("Height must be between 50 and 280 cm.")
			return
		}
		height = &v
	}
	switch err := s.store.UpdateUserProfile(r.Context(), u.ID, in.Name, email, height); {
	case errors.Is(err, store.ErrExists):
		invalid("Another account already uses that e-mail address.")
	case errors.Is(err, store.ErrInvalid):
		invalid("Please enter a name of 1-64 characters.")
	case err != nil:
		internalError(w, err)
	default:
		if email != u.Email {
			s.forgetResets(r.Context(), u.ID) // links made for the old address must not work
		}
		if timezone != u.Timezone {
			if err := s.store.UpdateUserTimezone(r.Context(), u.ID, timezone); err != nil {
				internalError(w, err)
				return
			}
		}
		s.audit(r, u.ID, auditProfile, "")
		internalJSON(w, http.StatusNoContent, nil)
	}
}

func (s *Backend) internalPasswordChange(w http.ResponseWriter, r *http.Request, u models.User) {
	var in apiclient.PasswordChange
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if !s.passwordEnabled(ctx) {
		internalFail(w, http.StatusForbidden, apiclient.CodeForbidden, "Signing in with a password is turned off.")
		return
	}
	if !s.internalConfirmPassword(w, r, u, in.Current) { // changing a password needs the current one
		return
	}
	if msg := validatePassword(in.Password, in.Confirm); msg != "" {
		internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, msg)
		return
	}
	if err := s.setPassword(ctx, u.ID, in.Password); err != nil {
		internalError(w, err)
		return
	}
	// A changed password signs out everywhere else and revokes API tokens, which would otherwise
	// keep working for whoever stole the old session.
	if err := s.store.DeleteUserSessions(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	}
	if err := s.store.DeleteUserAccessTokens(ctx, u.ID); err != nil {
		internalError(w, err)
		return
	}
	if err := s.startSession(w, r, u.ID, models.ProviderLocal); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditPasswordChanged, "")
	// The old session is gone, so the page cannot be fetched again with the visitor's cookie.
	pg, err := s.settingsPageData(r, u)
	if err != nil {
		internalError(w, err)
		return
	}
	internalJSON(w, http.StatusOK, pg)
}

func (s *Backend) internalTokenCreate(w http.ResponseWriter, r *http.Request, u models.User) {
	var in apiclient.NewTokenRequest
	if !internalDecode(w, r, &in) {
		return
	}
	if !s.internalConfirmPassword(w, r, u, in.Current) {
		return
	}
	invalid := func(msg string) { internalFail(w, http.StatusUnprocessableEntity, apiclient.CodeInvalid, msg) }
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxTokenNameLen {
		invalid("Please give the token a name of 1-64 characters.")
		return
	}
	t := models.AccessToken{UserID: u.ID, Name: name}
	if days := in.Expires; days != "" && days != "never" {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > 3650 {
			invalid("Please choose a valid expiry.")
			return
		}
		exp := time.Now().UTC().AddDate(0, 0, n)
		t.ExpiresAt = &exp
	}
	random, err := randomHex(32)
	if err != nil {
		internalError(w, err)
		return
	}
	token := tokenPrefix + random
	t.Prefix, t.Hash = token[:tokenPrefixShown], hashToken(token)
	if err := s.store.CreateAccessToken(r.Context(), &t); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditTokenCreated, t.Prefix)
	internalJSON(w, http.StatusOK, map[string]string{"token": token})
}

func (s *Backend) internalTokenDelete(w http.ResponseWriter, r *http.Request, u models.User) {
	id, err := strconv.ParseInt(r.PathValue("item"), 10, 64)
	if err != nil {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	if err := s.store.DeleteAccessToken(r.Context(), u.ID, id); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditTokenRevoked, "")
	internalJSON(w, http.StatusNoContent, nil)
}

func (s *Backend) internalIdentityUnlink(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	provider := r.PathValue("provider")
	answer := func(msg string) { internalJSON(w, http.StatusOK, map[string]string{"message": msg}) }
	if provider == models.ProviderPasskey {
		answer("Remove passkeys in the Passkeys section.")
		return
	}
	mine, err := s.store.UserProviders(ctx, u.ID)
	if err != nil {
		internalError(w, err)
		return
	}
	if !s.keepsSignIn(ctx, mine, provider) {
		answer("Add another way to sign in before unlinking this account.")
		return
	}
	if err := s.store.DeleteIdentity(ctx, u.ID, provider); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, u.ID, auditIdentityUnlinked, provider)
	answer("Account unlinked.")
}

// internalLogout ends the visitor's session and clears the cookie.
func (s *Backend) internalLogout(w http.ResponseWriter, r *http.Request) {
	if u := currentUser(r); u != nil {
		s.audit(r, u.ID, auditLogout, "")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			internalError(w, err)
			return
		}
	}
	clearSessionCookie(w, r)
	internalJSON(w, http.StatusNoContent, nil)
}
