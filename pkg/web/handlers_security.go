package web

import (
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	maxTokenNameLen = 64
	maxEmailLen     = 254
	minHeightCm     = 50
	maxHeightCm     = 280
)

// securityView is the settings page's sign-in section.
type securityView struct {
	Activity        []auditLine // the latest events about this account
	PasswordEnabled bool
	HasPassword     bool
	Tokens          []tokenView
	NewToken        string // shown once, right after it was created
	Linked          []linkedProvider
}

type tokenView struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastUsed string
	Expires  string
}

type linkedProvider struct {
	Slug, Label string
	Linked      bool
}

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
	view := &securityView{PasswordEnabled: s.passwordEnabled(ctx), HasPassword: slices.Contains(mine, models.ProviderLocal), NewToken: newToken}
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
		view.Linked = append(view.Linked, linkedProvider{p.Slug, p.Label, slices.Contains(mine, p.Slug)})
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
	email, ok := parseEmail(r.PostFormValue("email"))
	if !ok {
		s.settingsPage(w, r, u, "Please enter a valid e-mail address.", "", http.StatusUnprocessableEntity)
		return
	}
	var height *float64
	if raw := strings.TrimSpace(r.PostFormValue("height_cm")); raw != "" {
		v, err := strconv.ParseFloat(strings.ReplaceAll(raw, ",", "."), 64)
		if err != nil || v < minHeightCm || v > maxHeightCm {
			s.settingsPage(w, r, u, "Height must be between 50 and 280 cm.", "", http.StatusUnprocessableEntity)
			return
		}
		height = &v
	}
	switch err := s.store.UpdateUserProfile(r.Context(), u.ID, r.PostFormValue("name"), email, height); {
	case errors.Is(err, store.ErrExists):
		s.settingsPage(w, r, u, "Another account already uses that e-mail address.", "", http.StatusUnprocessableEntity)
	case errors.Is(err, store.ErrInvalid):
		s.settingsPage(w, r, u, "Please enter a name of 1-64 characters.", "", http.StatusUnprocessableEntity)
	case err != nil:
		s.fail(w, err)
	default:
		s.audit(r, u.ID, auditProfile, "")
		settingsRedirect(w, r, u.ID, "Profile saved.")
	}
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	if !s.passwordEnabled(ctx) {
		s.settingsPage(w, r, u, "Signing in with a password is turned off.", "", http.StatusForbidden)
		return
	}
	_, err := s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
	switch {
	case err == nil: // changing a password needs the current one
		id, ierr := s.store.GetIdentity(ctx, models.ProviderLocal, localSubject(u.ID))
		if ierr != nil {
			s.fail(w, ierr)
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(id.Secret), []byte(r.PostFormValue("current"))) != nil {
			s.settingsPage(w, r, u, "The current password is wrong.", "", http.StatusUnprocessableEntity)
			return
		}
	case !errors.Is(err, store.ErrNotFound):
		s.fail(w, err)
		return
	}
	password := r.PostFormValue("password")
	if msg := validatePassword(password, r.PostFormValue("confirm")); msg != "" {
		s.settingsPage(w, r, u, msg, "", http.StatusUnprocessableEntity)
		return
	}
	if err := s.setPassword(ctx, u.ID, password); err != nil {
		s.fail(w, err)
		return
	}
	// A changed password signs out everywhere else and revokes API tokens, which would otherwise
	// keep working for whoever stole the old session.
	if err := s.store.DeleteUserSessions(ctx, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteUserAccessTokens(ctx, u.ID); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.startSession(w, r, u.ID, models.ProviderLocal); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditPasswordChanged, "")
	s.settingsPage(w, r, u, "", "Password saved. Access tokens were revoked; create new ones if you use the API.", http.StatusOK)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request, u models.User) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" || len(name) > maxTokenNameLen {
		s.settingsPage(w, r, u, "Please give the token a name of 1-64 characters.", "", http.StatusUnprocessableEntity)
		return
	}
	t := models.AccessToken{UserID: u.ID, Name: name}
	if days := r.PostFormValue("expires"); days != "" && days != "never" {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 || n > 3650 {
			s.settingsPage(w, r, u, "Please choose a valid expiry.", "", http.StatusUnprocessableEntity)
			return
		}
		exp := time.Now().UTC().AddDate(0, 0, n)
		t.ExpiresAt = &exp
	}
	random, err := randomHex(32)
	if err != nil {
		s.fail(w, err)
		return
	}
	token := tokenPrefix + random
	t.Prefix, t.Hash = token[:tokenPrefixShown], hashToken(token)
	if err := s.store.CreateAccessToken(r.Context(), &t); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditTokenCreated, t.Prefix)
	// Rendered directly, never redirected, so the token does not end up in a URL.
	s.renderSettings(w, r, u, "", "Token created.", token, http.StatusOK)
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request, u models.User) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteAccessToken(r.Context(), u.ID, id); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditTokenRevoked, "")
	settingsRedirect(w, r, u.ID, "Token revoked.")
}

func (s *Server) unlinkIdentity(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	provider := r.PathValue("provider")
	mine, err := s.store.UserProviders(ctx, u.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.keepsSignIn(ctx, mine, provider) {
		settingsRedirect(w, r, u.ID, "Add another way to sign in before unlinking this account.")
		return
	}
	if err := s.store.DeleteIdentity(ctx, u.ID, provider); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, u.ID, auditIdentityUnlinked, provider)
	settingsRedirect(w, r, u.ID, "Account unlinked.")
}

func settingsRedirect(w http.ResponseWriter, r *http.Request, userID int64, msg string) {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, userPath(userID, "settings")+"?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}
