package backend

import (
	"errors"
	"net/http"
	"net/mail"
	"slices"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
	"golang.org/x/crypto/bcrypt"
)

func (s *Backend) securityView(r *http.Request, u models.User, newToken string) (*securityView, error) {
	ctx := r.Context()
	mine, err := s.store.UserProviders(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	pwOn := s.passwordEnabled(ctx)
	hasPw := slices.Contains(mine, models.ProviderLocal)
	view := &securityView{PasswordEnabled: pwOn, HasPassword: hasPw, ConfirmPassword: pwOn && hasPw, NewToken: newToken}
	if view.Passkeys, err = s.passkeysFor(r, u); err != nil {
		return nil, err
	}
	view.TwoFactor.Available = pwOn && hasPw
	view.TwoFactor.Required = s.twoFactorRequired(ctx, u)
	if view.TwoFactor.Enabled, err = s.hasTwoFactor(ctx, u.ID); err != nil {
		return nil, err
	}
	if view.TwoFactor.Enabled {
		if view.TwoFactor.RecoveryLeft, err = s.store.CountRecoveryCodes(ctx, u.ID); err != nil {
			return nil, err
		}
	}
	tokens, err := s.store.ListAccessTokens(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range tokens {
		view.Tokens = append(view.Tokens, tokenView{
			ID: t.ID, Name: t.Name, Prefix: t.Prefix, Created: t.CreatedAt.UTC().Format(dateLayout),
			LastUsed: dayOrDash(t.LastUsedAt, "Never"), Expires: dayOrDash(t.ExpiresAt, "Never"),
		})
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		view.Linked = append(view.Linked, linkedProvider{Slug: p.Slug, Label: p.Label, Linked: slices.Contains(mine, p.Slug)})
	}
	return view, nil
}

// parseEmail validates an address and returns its canonical form.
func parseEmail(raw string) (string, bool) {
	email := models.NormalizeEmail(raw)
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > maxEmailLen {
		return "", false
	}
	return email, true
}

// checkReauth is confirmPassword without the response, for callers that answer in their own way
// (the JSON endpoints of the passkey page). A user with a password confirms it in the form field
// "current". A user without one has nothing to confirm with: unless recent is set they pass, and
// with it they need a session that is at most reauthWindow old. When it reports false, status and
// msg are what to answer; err is a server failure.
func (s *Backend) checkReauth(r *http.Request, u models.User, recent bool) (ok bool, status int, msg string, err error) {
	ctx := r.Context()
	var id models.Identity
	hasPassword := false
	if s.passwordEnabled(ctx) {
		id, err = s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
		switch {
		case err == nil:
			hasPassword = true
		case errors.Is(err, store.ErrNotFound):
			err = nil
		default:
			return false, 0, "", err
		}
	}
	if !hasPassword {
		if !recent {
			return true, 0, "", nil
		}
		c, cerr := r.Cookie(sessionCookie)
		if cerr != nil {
			return false, http.StatusUnauthorized, "Sign in again to make this change.", nil
		}
		sess, serr := s.store.GetSession(ctx, hashToken(c.Value))
		if errors.Is(serr, store.ErrNotFound) || (serr == nil && (sess.UserID != u.ID || time.Since(sess.CreatedAt) > reauthWindow)) {
			return false, http.StatusUnauthorized, "Sign in again to make this change.", nil
		}
		return serr == nil, 0, "", serr
	}
	key := "pw:" + strconv.FormatInt(u.ID, 10)
	if s.limiter.Blocked(key, maxLoginFail) || s.limiter.Blocked(ipKey(r), maxIPFail) {
		return false, http.StatusTooManyRequests, "Too many wrong passwords. Try again later.", nil
	}
	if bcrypt.CompareHashAndPassword([]byte(id.Secret), []byte(r.PostFormValue("current"))) != nil {
		s.limiter.Fail(key)
		s.limiter.Fail(ipKey(r))
		s.audit(r, u.ID, auditPasswordWrong, "")
		return false, http.StatusUnprocessableEntity, "Enter your current password to make this change.", nil
	}
	s.limiter.Reset(key)
	return true, 0, "", nil
}
