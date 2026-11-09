package web

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	maxTokenNameLen = 64
	maxEmailLen     = 254
	minHeightCm     = 50
	maxHeightCm     = 280
)

func dayOrDash(t *time.Time, none string) string {
	if t == nil {
		return none
	}
	return t.UTC().Format(dateLayout)
}

func (s *Server) securityView(r *http.Request, u models.User, newToken string) (*securityView, error) {
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

// saveProfile stores the name, e-mail address and height shown on the settings page.
func (s *Server) saveProfile(w http.ResponseWriter, r *http.Request, u models.User) {
	err := s.api.SaveProfile(r.Context(), u.ID, apiclient.Profile{
		Name: r.PostFormValue("name"), Email: r.PostFormValue("email"), Timezone: r.PostFormValue("timezone"),
		HeightCm: r.PostFormValue("height_cm"), Current: r.PostFormValue("current"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	settingsRedirect(w, r, u.ID, "Profile saved.")
}

// settingsError shows a refused change on the settings page with the status the backend gave; any
// other failure is answered as usual.
func (s *Server) settingsError(w http.ResponseWriter, r *http.Request, u models.User, err error) {
	var e *apiclient.Error
	if errors.As(err, &e) && (e.Code == apiclient.CodeInvalid || e.Code == apiclient.CodeThrottled ||
		e.Code == apiclient.CodeForbidden || e.Code == apiclient.CodeExists) {
		s.settingsPage(w, r, u, e.Message, "", e.Status)
		return
	}
	s.fail(w, err)
}

// confirmPassword asks a user who signs in with a password for it again before a change that
// could hand the account to someone holding only a stolen session: a new e-mail address, an access
// token, a linked sign-in provider. The attempts are limited like sign-in attempts. It reports
// whether to go on; if not, the response has been written. Users without a password (they sign in
// through a provider) and servers with password sign-in off have nothing to confirm with.
func (s *Server) confirmPassword(w http.ResponseWriter, r *http.Request, u models.User) bool {
	ok, status, msg, err := s.checkReauth(r, u, false)
	switch {
	case err != nil:
		s.fail(w, err)
		return false
	case !ok:
		s.settingsPage(w, r, u, msg, "", status)
		return false
	}
	return true
}

// reauthWindow is how recent the sign-in of a user without a password must be for a sensitive
// change, where recent sign-in is required.
const reauthWindow = 10 * time.Minute

// checkReauth is confirmPassword without the response, for callers that answer in their own way
// (the JSON endpoints of the passkey page). A user with a password confirms it in the form field
// "current". A user without one has nothing to confirm with: unless recent is set they pass, and
// with it they need a session that is at most reauthWindow old. When it reports false, status and
// msg are what to answer; err is a server failure.
func (s *Server) checkReauth(r *http.Request, u models.User, recent bool) (ok bool, status int, msg string, err error) {
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

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u models.User) {
	pg, err := s.api.ChangePassword(r.Context(), u.ID, apiclient.PasswordChange{
		Current: r.PostFormValue("current"), Password: r.PostFormValue("password"), Confirm: r.PostFormValue("confirm"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	// The old session is gone; the backend sent the page data along with the new session cookie.
	s.renderSettingsPage(w, u, pg, "", "Password saved. Access tokens were revoked; create new ones if you use the API.", "", http.StatusOK, nil)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, u models.User) {
	token, err := s.api.CreateToken(r.Context(), u.ID, apiclient.NewTokenRequest{
		Name: r.PostFormValue("name"), Expires: r.PostFormValue("expires"), Current: r.PostFormValue("current"),
	})
	if err != nil {
		s.settingsError(w, r, u, err)
		return
	}
	// Rendered directly, never redirected, so the token does not end up in a URL.
	s.renderSettings(w, r, u, "", "Token created.", token, http.StatusOK)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.api.DeleteToken(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	settingsRedirect(w, r, u.ID, "Token revoked.")
}

func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request, u models.User) {
	msg, err := s.api.UnlinkIdentity(r.Context(), u.ID, r.PathValue("provider"))
	if err != nil {
		s.fail(w, err)
		return
	}
	settingsRedirect(w, r, u.ID, msg)
}

func settingsRedirect(w http.ResponseWriter, r *http.Request, userID int64, msg string) {
	http.Redirect(w, r, settingsPath(userID, msg), http.StatusSeeOther)
}

// settingsPath is the address of a user's settings page with a message to show.
func settingsPath(userID int64, msg string) string {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	return userPath(userID, "settings") + "?msg=" + url.QueryEscape(msg)
}
