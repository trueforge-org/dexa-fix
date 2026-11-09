package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/mailer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/tlsutil"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// The backend half of the Admin pages. Every endpoint is behind internalAdmin: an administrator who
// passed the step-up check. Refusals the administrator can fix come back as a message in a normal
// answer (the Admin page shows them as a flash text); real failures are errors.

const (
	settingWithingsClientID = "withings.client_id"
	settingWithingsSecret   = "withings.client_secret"
	settingWithingsRedirect = "withings.redirect_url"
)

var providerSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func (s *Backend) internalAdminRoutes(mux *http.ServeMux) {
	p := apiclient.Prefix + "/admin"
	mux.HandleFunc("GET "+p+"/page", s.internalAdmin(s.internalAdminPage))
	mux.HandleFunc("POST "+p+"/users", s.internalAdmin(s.internalAdminAddUser))
	mux.HandleFunc("POST "+p+"/users/{id}/role", s.internalAdmin(s.internalAdminSetRole))
	mux.HandleFunc("POST "+p+"/users/{id}/strengthlevel", s.internalAdmin(s.internalAdminSetStrengthLevel))
	mux.HandleFunc("POST "+p+"/users/{id}/strengthlevel-bulk", s.internalAdmin(s.internalAdminSetStrengthLevelBulk))
	mux.HandleFunc("POST "+p+"/users/{id}/password", s.internalAdmin(s.internalAdminSetPassword))
	mux.HandleFunc("DELETE "+p+"/users/{id}", s.internalAdmin(s.internalAdminDeleteUser))
	mux.HandleFunc("DELETE "+p+"/users/{id}/2fa", s.internalAdmin(s.internalAdminRemoveTwoFactor))
	mux.HandleFunc("POST "+p+"/users/{id}/reset-link", s.internalAdmin(s.internalAdminResetLink))
	mux.HandleFunc("POST "+p+"/password-login", s.internalAdmin(s.internalAdminPasswordLogin))
	mux.HandleFunc("POST "+p+"/signup", s.internalAdmin(s.internalAdminSignup))
	mux.HandleFunc("POST "+p+"/two-factor", s.internalAdmin(s.internalAdminTwoFactorPolicy))
	mux.HandleFunc("POST "+p+"/providers", s.internalAdmin(s.internalAdminSaveProvider))
	mux.HandleFunc("DELETE "+p+"/providers/{slug}", s.internalAdmin(s.internalAdminDeleteProvider))
	mux.HandleFunc("POST "+p+"/withings", s.internalAdmin(s.internalAdminSaveWithings))
	mux.HandleFunc("POST "+p+"/server", s.internalAdmin(s.internalAdminSaveServer))
	mux.HandleFunc("POST "+p+"/smtp", s.internalAdmin(s.internalAdminSaveSMTP))
	mux.HandleFunc("POST "+p+"/smtp/test", s.internalAdmin(s.internalAdminTestSMTP))
	mux.HandleFunc("POST "+p+"/tls", s.internalAdmin(s.internalAdminSaveTLS))
}

// adminMsg answers a change with the text to show on the Admin page.
func adminMsg(w http.ResponseWriter, msg string) {
	internalJSON(w, http.StatusOK, apiclient.AdminResult{Message: msg})
}

// adminMsgChanged is adminMsg for a change of the settings the frontend acts on.
func adminMsgChanged(w http.ResponseWriter, msg string) {
	internalJSON(w, http.StatusOK, apiclient.AdminResult{Message: msg, ConfigChanged: true})
}

// internalTarget finds the user the path names.
func (s *Backend) internalTarget(w http.ResponseWriter, r *http.Request) (models.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return models.User{}, false
	}
	t, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return models.User{}, false
	}
	return t, true
}

// ---- the page ----

func (s *Backend) keyView(st store.Stats) apiclient.KeyView {
	v := apiclient.KeyView{Shown: st.Encrypted, ID: st.KeyID, Rekey: st.KeyPrevious}
	if !st.KeySince.IsZero() {
		v.Since = st.KeySince.UTC().Format(dateLayout)
		v.AgeDays = int(time.Since(st.KeySince) / (24 * time.Hour))
		v.Due = s.cfg().keyReminder > 0 && time.Since(st.KeySince) > s.cfg().keyReminder
	}
	return v
}

func (s *Backend) internalAdminPage(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	stats, err := s.store.Stats(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	ids, err := s.store.IdentityProviders(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	view := apiclient.AdminPage{
		Status: apiclient.AdminStatus{
			Driver: stats.Driver, Encrypted: stats.Encrypted, SchemaVersion: stats.SchemaVersion, Users: stats.Users,
			Admins: stats.Admins, DexaScans: stats.DexaScans, ScaleReadings: stats.ScaleReadings,
			Sessions: stats.Sessions, AccessTokens: stats.AccessTokens, TwoFactorUsers: stats.TwoFactorUsers,
		},
		PasswordEnabled: s.passwordEnabled(ctx), PasswordForced: s.ForcePasswordLogin,
		Withings: s.adminWithingsView(r), Key: s.keyView(stats), Require2FA: s.twoFactorPolicy(ctx),
		Form: apiclient.AdminProvider{AuthProvider: models.AuthProvider{Kind: models.ProviderKindOIDC}},
	}
	if view.SignupOpen, err = s.signupOpen(ctx); err != nil {
		internalError(w, err)
		return
	}
	if view.Server, err = s.serverSettingsFor(ctx); err != nil {
		internalError(w, err)
		return
	}
	if _, view.SMTP, err = s.loadSMTP(ctx); err != nil {
		internalError(w, err)
		return
	}
	view.ResetByEmail = s.selfServiceReset(ctx)
	if view.TLS, err = s.tlsViewFor(r); err != nil {
		internalError(w, err)
		return
	}
	twoFactor, err := s.store.TwoFactorUsers(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	for _, x := range users {
		signIn := strings.Join(ids[x.ID], ", ")
		if signIn == "" {
			signIn = "None"
		}
		view.Users = append(view.Users, apiclient.AdminUser{ID: x.ID, Name: x.Name, Email: x.Email, Admin: x.IsAdmin(), Self: x.ID == u.ID,
			Created: x.CreatedAt.UTC().Format(dateLayout), SignIn: signIn, StrengthLevel: x.StrengthLevelAuto,
			StrengthLevelBulk: x.StrengthLevelBulk, TwoFactor: twoFactor[x.ID]})
	}
	events, err := s.store.ListAuditEvents(ctx, store.AuditFilter{Limit: auditPageSize})
	if err != nil {
		internalError(w, err)
		return
	}
	names := make(map[int64]string, len(users))
	for _, x := range users {
		names[x.ID] = x.Name
	}
	view.Audit = auditLines(events, func(id int64) string {
		if n, ok := names[id]; ok {
			return n
		}
		return "deleted user"
	})
	edit := r.URL.Query().Get("edit")
	for _, p := range providers {
		ap := apiclient.AdminProvider{AuthProvider: p, CallbackURL: externalURL(r, "/auth/"+p.Slug+"/callback"), HasSecret: p.ClientSecret != ""}
		ap.ClientSecret = "" // never sent back to the browser
		view.Providers = append(view.Providers, ap)
		if p.Slug == edit {
			view.Form, view.Editing = ap, true
		}
	}
	internalJSON(w, http.StatusOK, view)
}

// ---- users ----

func (s *Backend) internalAdminAddUser(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in struct{ Name, Email, Password string }
	if !internalDecode(w, r, &in) {
		return
	}
	email, ok := parseEmail(in.Email)
	if !ok {
		adminMsg(w, "Please enter a valid e-mail address.")
		return
	}
	if msg := validatePassword(in.Password, in.Password); msg != "" {
		adminMsg(w, msg)
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		internalError(w, err)
		return
	}
	u, err := s.store.CreateUserWithIdentity(r.Context(), in.Name, email, models.Identity{Provider: models.ProviderLocal, Secret: hash})
	switch {
	case errors.Is(err, store.ErrExists):
		adminMsg(w, "An account with that e-mail address already exists.")
	case errors.Is(err, store.ErrInvalid):
		adminMsg(w, "Please enter a name of 1-64 characters.")
	case err != nil:
		internalError(w, err)
	default:
		s.audit(r, u.ID, auditUserAdded, "")
		adminMsg(w, u.Name+" was added.")
	}
}

func (s *Backend) internalAdminSignup(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in struct{ Enabled bool }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if in.Enabled {
		if err := s.store.PutSetting(ctx, settingSignup, valueEnabled); err != nil {
			internalError(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "signup=open")
		adminMsg(w, "New accounts are open.")
		return
	}
	if err := s.store.DeleteSetting(ctx, settingSignup); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "signup=closed")
	adminMsg(w, "New accounts are closed. Add people from the Users list.")
}

func (s *Backend) internalAdminSetRole(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Role string }
	if !internalDecode(w, r, &in) {
		return
	}
	role := models.RoleUser
	if in.Role == models.RoleAdmin {
		role = models.RoleAdmin
	}
	switch err := s.store.SetUserRole(r.Context(), t.ID, role); {
	case errors.Is(err, store.ErrLastAdmin):
		adminMsg(w, "There must be at least one administrator.")
	case err != nil:
		internalError(w, err)
	default:
		s.audit(r, t.ID, auditRoleChanged, role)
		adminMsg(w, t.Name+" is now "+role+".")
	}
}

func (s *Backend) internalAdminSetStrengthLevel(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Enabled bool }
	if !internalDecode(w, r, &in) {
		return
	}
	if err := s.store.SetUserStrengthLevelAuto(r.Context(), t.ID, in.Enabled); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, t.ID, auditStrengthLevel, onOff(in.Enabled))
	if in.Enabled {
		adminMsg(w, t.Name+" may now store a Strength Level login and send single days.")
		return
	}
	s.sl.stop(t.ID)
	adminMsg(w, "Strength Level sync is off for "+t.Name+", and their stored login is deleted.")
}

func (s *Backend) internalAdminSetStrengthLevelBulk(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Enabled bool }
	if !internalDecode(w, r, &in) {
		return
	}
	if !t.StrengthLevelAuto {
		adminMsg(w, "Enable Strength Level sync for "+t.Name+" first.")
		return
	}
	if err := s.store.SetUserStrengthLevelBulk(r.Context(), t.ID, in.Enabled); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, t.ID, auditStrengthLevelBlk, onOff(in.Enabled))
	if in.Enabled {
		adminMsg(w, t.Name+" may now run the multi-day Strength Level sync from Settings.")
		return
	}
	s.sl.stop(t.ID)
	adminMsg(w, "The multi-day Strength Level sync is off for "+t.Name+".")
}

func (s *Backend) internalAdminSetPassword(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Password string }
	if !internalDecode(w, r, &in) {
		return
	}
	if msg := validatePassword(in.Password, in.Password); msg != "" {
		adminMsg(w, msg)
		return
	}
	ctx := r.Context()
	if err := s.setPassword(ctx, t.ID, in.Password); err != nil {
		internalError(w, err)
		return
	}
	if err := s.store.DeleteUserSessions(ctx, t.ID); err != nil {
		internalError(w, err)
		return
	}
	if err := s.store.DeleteUserAccessTokens(ctx, t.ID); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, t.ID, auditPasswordSet, "")
	adminMsg(w, "Password set for "+t.Name+".")
}

func (s *Backend) internalAdminDeleteUser(w http.ResponseWriter, r *http.Request, u models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	if t.ID == u.ID {
		adminMsg(w, "You cannot delete your own account.")
		return
	}
	switch err := s.store.DeleteUser(r.Context(), t.ID); {
	case errors.Is(err, store.ErrLastAdmin):
		adminMsg(w, "There must be at least one administrator.")
	case err != nil:
		internalError(w, err)
	default:
		s.audit(r, 0, auditUserDeleted, "user_id="+strconv.FormatInt(t.ID, 10))
		adminMsg(w, t.Name+" and their data were deleted.")
	}
}

// internalAdminRemoveTwoFactor lets an administrator remove the second step of a user who lost
// their device and their recovery codes. The user is asked to set it up again if the policy requires it.
func (s *Backend) internalAdminRemoveTwoFactor(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTOTP(r.Context(), t.ID); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, t.ID, auditTOTPRemovedByAdmin, "")
	adminMsg(w, "Two-step verification was removed for "+t.Name+".")
}

func (s *Backend) internalAdminResetLink(w http.ResponseWriter, r *http.Request, admin models.User) {
	ctx := r.Context()
	if !s.passwordEnabled(ctx) {
		adminMsg(w, "Password sign-in is off, so there is nothing to reset.")
		return
	}
	t, ok := s.internalTarget(w, r)
	if !ok {
		return
	}
	var in struct{ Email bool }
	if !internalDecode(w, r, &in) {
		return
	}
	token, expires, err := s.newResetToken(ctx, t.ID, &admin.ID, resetAdminTTL)
	if err != nil {
		internalError(w, err)
		return
	}
	emailed := false
	if in.Email && s.cfg().mailer != nil && s.cfg().baseURL != nil && t.Email != "" {
		if link, _ := s.resetURL(r, token, true); link != "" {
			emailed = s.sendResetMail(t, link, resetAdminTTL)
		}
	}
	link, fromRequest := s.resetURL(r, token, false)
	s.audit(r, t.ID, auditResetLinkCreated, "")
	internalJSON(w, http.StatusOK, apiclient.AdminResult{ResetLink: &apiclient.ResetLink{
		For: t.Name, URL: link, Expires: expires.UTC().Format("2006-01-02 15:04 UTC"), Emailed: emailed, Warning: fromRequest,
	}})
}

// ---- sign-in policy ----

// hasExternalSignIn reports whether the user can sign in through a provider other than excluded.
func hasExternalSignIn(providers []string, excluded string) bool {
	return slices.ContainsFunc(providers, func(p string) bool { return p != models.ProviderLocal && p != excluded })
}

func (s *Backend) internalAdminPasswordLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	var in struct{ Enabled bool }
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	if in.Enabled {
		if err := s.store.DeleteSetting(ctx, settingPasswordOn); err != nil {
			internalError(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "password_login=on")
		adminMsg(w, "Password sign-in is on.")
		return
	}
	ids, err := s.store.IdentityProviders(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	if !hasExternalSignIn(ids[u.ID], "") {
		adminMsg(w, "Link your account to a sign-in provider in Settings first, so you are not locked out.")
		return
	}
	if err := s.store.PutSetting(ctx, settingPasswordOn, valueDisabled); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "password_login=off")
	adminMsg(w, "Password sign-in is off. People sign in through a provider, and scripts use access tokens.")
}

func (s *Backend) internalAdminTwoFactorPolicy(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in struct{ Require string }
	if !internalDecode(w, r, &in) {
		return
	}
	v := in.Require
	switch v {
	case require2FAOff, require2FAAdmins, require2FAEveryone:
	default:
		adminMsg(w, "Choose who must use two-step verification.")
		return
	}
	if err := s.store.PutSetting(r.Context(), settingRequire2FA, v); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "require_2fa="+v)
	switch v {
	case require2FAAdmins:
		adminMsg(w, "Administrators must now use two-step verification when they sign in with a password. They set it up at their next sign-in.")
	case require2FAEveryone:
		adminMsg(w, "Everyone must now use two-step verification when they sign in with a password. They set it up at their next sign-in.")
	default:
		adminMsg(w, "Two-step verification is optional.")
	}
}

// ---- providers ----

func validProviderURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"))
}

func (s *Backend) internalAdminSaveProvider(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.ProviderForm
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	field := strings.TrimSpace
	p := models.AuthProvider{
		Slug: field(in.Slug), Label: field(in.Label), Kind: field(in.Kind), Issuer: strings.TrimRight(field(in.Issuer), "/"),
		AuthURL: field(in.AuthURL), TokenURL: field(in.TokenURL), UserinfoURL: field(in.UserinfoURL),
		ClientID: field(in.ClientID), ClientSecret: field(in.ClientSecret),
		AppleTeamID: field(in.AppleTeamID), AppleKeyID: field(in.AppleKeyID),
		Scopes: field(in.Scopes), SubjectClaim: field(in.SubjectClaim), NameClaim: field(in.NameClaim),
		AllowSignup: in.AllowSignup,
	}
	if !providerSlug.MatchString(p.Slug) || p.Slug == models.ProviderLocal || p.Slug == models.ProviderPasskey {
		adminMsg(w, "The short name may use lowercase letters, digits and dashes, up to 32 characters.")
		return
	}
	if p.Label == "" || p.ClientID == "" {
		adminMsg(w, "A display name and client ID are required.")
		return
	}
	old, err := s.store.GetAuthProvider(ctx, p.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, err)
		return
	}
	if p.ClientSecret == "" {
		p.ClientSecret = old.ClientSecret
	}
	if msg := s.prepareProvider(ctx, &p); msg != "" {
		adminMsg(w, msg)
		return
	}
	if err := s.store.PutAuthProvider(ctx, p); err != nil {
		internalError(w, err)
		return
	}
	s.forgetOIDCProvider(old.Issuer)
	s.audit(r, 0, auditProviderSaved, p.Slug)
	adminMsg(w, p.Label+" saved.")
}

// prepareProvider validates a provider by kind and fills in defaults. It returns a message for the
// administrator, or "" when the provider is ready to save.
func (s *Backend) prepareProvider(ctx context.Context, p *models.AuthProvider) string {
	switch p.Kind {
	case models.ProviderKindOIDC:
		if !validProviderURL(p.Issuer) {
			return "OpenID Connect needs an https issuer URL."
		}
		if p.ClientSecret == "" {
			return "A client secret is required."
		}
		if p.Scopes == "" {
			p.Scopes = "openid email profile"
		}
		s.forgetOIDCProvider(p.Issuer)
		if _, err := s.oidcProvider(ctx, *p); err != nil {
			return "Could not read the OpenID configuration from that issuer: " + err.Error()
		}
	case models.ProviderKindApple:
		p.Issuer = appleIssuer
		if p.AppleTeamID == "" || p.AppleKeyID == "" {
			return "Apple needs a team ID and key ID."
		}
		if _, err := parseAppleKey(p.ClientSecret); err != nil {
			return "Paste the contents of the .p8 key file from Apple as the secret."
		}
		if p.Scopes == "" {
			p.Scopes = "name email"
		}
	case models.ProviderKindOAuth2:
		if !validProviderURL(p.AuthURL) || !validProviderURL(p.TokenURL) || !validProviderURL(p.UserinfoURL) {
			return "OAuth 2.0 needs https authorisation, token and userinfo URLs."
		}
		if p.ClientSecret == "" {
			return "A client secret is required."
		}
		if p.SubjectClaim == "" {
			p.SubjectClaim = "sub"
		}
		p.Issuer = ""
	default:
		return "Unknown provider type."
	}
	return ""
}

func (s *Backend) internalAdminDeleteProvider(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	p, err := s.store.GetAuthProvider(ctx, slug)
	if err != nil {
		internalError(w, err)
		return
	}
	if !s.passwordEnabled(ctx) {
		ids, err := s.store.IdentityProviders(ctx)
		if err != nil {
			internalError(w, err)
			return
		}
		if !hasExternalSignIn(ids[u.ID], slug) {
			adminMsg(w, "Password sign-in is off and this is your only way in. Turn passwords on or link another provider first.")
			return
		}
	}
	if err := s.store.DeleteAuthProvider(ctx, slug); err != nil {
		internalError(w, err)
		return
	}
	s.forgetOIDCProvider(p.Issuer)
	s.audit(r, 0, auditProviderRemoved, slug)
	adminMsg(w, p.Label+" removed.")
}

// ---- Withings application credentials ----

func (s *Backend) withingsConfigurable() (importer.Configurable, bool) {
	imp, ok := s.importers.Get(providerWithings)
	if !ok {
		return nil, false
	}
	c, ok := imp.(importer.Configurable)
	return c, ok
}

func (s *Backend) settingOrEmpty(ctx context.Context, name string) (string, error) {
	v, err := s.store.GetSetting(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// applyWithingsCredentials loads the credentials saved by an administrator into the importer.
func (s *Backend) applyWithingsCredentials(ctx context.Context) error {
	c, ok := s.withingsConfigurable()
	if !ok {
		return nil
	}
	var creds importer.Credentials
	for _, f := range []struct {
		name string
		dst  *string
	}{{settingWithingsClientID, &creds.ClientID}, {settingWithingsSecret, &creds.ClientSecret}, {settingWithingsRedirect, &creds.RedirectURL}} {
		v, err := s.settingOrEmpty(ctx, f.name)
		if err != nil {
			return err
		}
		*f.dst = v
	}
	c.SetCredentials(creds)
	return nil
}

// withingsCallback is this server's Withings OAuth callback URL, the one to register with Withings.
func withingsCallback(r *http.Request) string {
	return externalURL(r, "/integrations/withings/callback")
}

func (s *Backend) adminWithingsView(r *http.Request) apiclient.AdminWithings {
	ctx := r.Context()
	if _, ok := s.withingsConfigurable(); !ok {
		return apiclient.AdminWithings{}
	}
	id, _ := s.settingOrEmpty(ctx, settingWithingsClientID)
	secret, _ := s.settingOrEmpty(ctx, settingWithingsSecret)
	redirect, _ := s.settingOrEmpty(ctx, settingWithingsRedirect)
	v := apiclient.AdminWithings{
		Available: true, ClientID: id, RedirectURL: redirect, DefaultRedirect: withingsCallback(r),
		SecretSaved: secret != "",
	}
	if p, ok := s.oauthProvider(providerWithings); ok {
		v.Configured = p.Configured()
	}
	return v
}

func (s *Backend) internalAdminSaveWithings(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	if _, ok := s.withingsConfigurable(); !ok {
		internalFail(w, http.StatusNotFound, apiclient.CodeNotFound, "not found")
		return
	}
	var in apiclient.WithingsForm
	if !internalDecode(w, r, &in) {
		return
	}
	save := func(name, value string) error {
		if value == "" {
			return s.store.DeleteSetting(ctx, name)
		}
		return s.store.PutSetting(ctx, name, value)
	}
	id, redirect := strings.TrimSpace(in.ClientID), strings.TrimSpace(in.RedirectURL)
	if redirect != "" && !validProviderURL(redirect) {
		adminMsg(w, "The redirect URL must use https, or http on localhost.")
		return
	}
	if redirect == "" && id != "" {
		redirect = withingsCallback(r) // this server's own callback, as the browser reaches it
	}
	err := save(settingWithingsClientID, id)
	if err == nil {
		err = save(settingWithingsRedirect, redirect)
	}
	if secret := strings.TrimSpace(in.ClientSecret); err == nil && secret != "" {
		err = save(settingWithingsSecret, secret)
	} else if err == nil && in.ClearSecret {
		err = save(settingWithingsSecret, "")
	}
	if err == nil {
		err = s.applyWithingsCredentials(ctx)
	}
	if err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "withings")
	adminMsg(w, "Withings settings saved.")
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// ---- server settings ----

// serverSettingsFor reads the stored text of the settings for the form.
func (s *Backend) serverSettingsFor(ctx context.Context) (apiclient.ServerSettingsView, error) {
	var v apiclient.ServerSettingsView
	var err error
	get := func(name string) string {
		if err != nil {
			return ""
		}
		var x string
		x, err = s.settingValue(ctx, name)
		return x
	}
	v.BaseURL, v.TrustedProxies = get(settingBaseURL), get(settingTrustedProxies)
	v.ProxyProtocol = get(settingProxyProtocol) == valueEnabled
	v.AuditDays, v.KeyReminderDays = get(settingAuditRetention), get(settingKeyReminder)
	return v, err
}

// putOrClear stores a value, or removes the setting when it is empty so the default applies.
func (s *Backend) putOrClear(ctx context.Context, name, value string) error {
	if value == "" {
		return s.store.DeleteSetting(ctx, name)
	}
	return s.store.PutSetting(ctx, name, value)
}

// parseDays reads a number of days from the form: empty means the default, and 0 is allowed.
func parseDays(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 || n > maxSettingDays {
		return "", false
	}
	return strconv.Itoa(n), true
}

func hostOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(strings.Trim(hostport, "[]"))
}

func (s *Backend) internalAdminSaveServer(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.ServerForm
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	base, err := ParseBaseURL(in.BaseURL)
	if err != nil {
		adminMsg(w, err.Error())
		return
	}
	// A wrong address would make the cookies and the same-origin check fail for the page the
	// administrator is on, with no way back in. So the address has to be the one in use right now
	// (r.Host is the browser's, forwarded by the frontend).
	if base != nil && hostOf(base.Host) != hostOf(r.Host) {
		adminMsg(w, "The base URL must be the address you are using now ("+r.Host+"). Open the Admin page at "+base.Host+" and save it there, so you cannot lock yourself out.")
		return
	}
	proxiesText := strings.TrimSpace(in.TrustedProxies)
	trusted, err := ParseTrustedProxies([]string{proxiesText})
	if err != nil {
		adminMsg(w, err.Error())
		return
	}
	if in.ProxyProtocol && len(trusted) == 0 {
		adminMsg(w, "The PROXY protocol needs the trusted proxies: only connections from them may send the header.")
		return
	}
	audit, ok := parseDays(in.AuditDays)
	if !ok {
		adminMsg(w, "Audit log retention is a number of days from 0 to 36500 (0 keeps the log for good).")
		return
	}
	reminder, ok := parseDays(in.KeyReminderDays)
	if !ok {
		adminMsg(w, "The key reminder is a number of days from 0 to 36500 (0 never reminds).")
		return
	}
	baseText := ""
	if base != nil {
		baseText = base.Scheme + "://" + base.Host
	}
	proxyValue := valueDisabled
	if in.ProxyProtocol {
		proxyValue = valueEnabled
	}
	for _, p := range []struct{ name, value string }{
		{settingBaseURL, baseText}, {settingTrustedProxies, proxiesText}, {settingProxyProtocol, proxyValue},
		{settingAuditRetention, audit}, {settingKeyReminder, reminder},
	} {
		if err := s.putOrClear(ctx, p.name, p.value); err != nil {
			internalError(w, err)
			return
		}
	}
	if err := s.reloadSettings(ctx); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "server_settings")
	adminMsgChanged(w, "Server settings saved. They apply now.")
}

func (s *Backend) internalAdminSaveSMTP(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.SMTPForm
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	cur, _, err := s.loadSMTP(ctx)
	if err != nil {
		internalError(w, err)
		return
	}
	cfg := mailer.Config{
		Host: strings.TrimSpace(in.Host), Username: strings.TrimSpace(in.Username),
		From: strings.TrimSpace(in.From), TLS: in.TLS, Password: cur.Password,
	}
	if in.Password != "" {
		cfg.Password = in.Password
	}
	if in.RemovePassword {
		cfg.Password = ""
	}
	if cfg.Host == "" { // an empty host turns e-mail off
		if err := s.store.DeleteSetting(ctx, settingSMTPHost); err != nil {
			internalError(w, err)
			return
		}
		if err := s.reloadSettings(ctx); err != nil {
			internalError(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "smtp=off")
		adminMsg(w, "E-mail is off. Administrators can still make reset links.")
		return
	}
	cfg.Port = defaultSMTPPort
	if raw := strings.TrimSpace(in.Port); raw != "" {
		n, perr := strconv.Atoi(raw)
		if perr != nil || n < 1 || n > 65535 {
			adminMsg(w, "The port is a number from 1 to 65535.")
			return
		}
		cfg.Port = n
	}
	if _, err := mailer.New(cfg); err != nil {
		adminMsg(w, err.Error())
		return
	}
	for _, p := range []struct{ name, value string }{
		{settingSMTPHost, cfg.Host}, {settingSMTPPort, strconv.Itoa(cfg.Port)}, {settingSMTPUsername, cfg.Username},
		{settingSMTPPassword, cfg.Password}, {settingSMTPFrom, cfg.From}, {settingSMTPTLS, cfg.TLS},
	} {
		if err := s.putOrClear(ctx, p.name, p.value); err != nil {
			internalError(w, err)
			return
		}
	}
	if err := s.reloadSettings(ctx); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "smtp=on")
	msg := "E-mail settings saved. Send a test e-mail to check them."
	if s.cfg().baseURL == nil {
		msg += " Users can only ask for a reset e-mail once the base URL is set."
	}
	adminMsg(w, msg)
}

func (s *Backend) internalAdminTestSMTP(w http.ResponseWriter, r *http.Request, u models.User) {
	m := s.cfg().mailer
	if m == nil {
		adminMsg(w, "Set up and save the e-mail settings first.")
		return
	}
	if u.Email == "" {
		adminMsg(w, "Your account has no e-mail address to send the test to.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if err := m.Send(ctx, u.Email, "dexa-fix test e-mail", "This is a test e-mail from dexa-fix. E-mail is working.\n"); err != nil {
		adminMsg(w, "Could not send the test e-mail: "+err.Error())
		return
	}
	adminMsg(w, "A test e-mail was sent to "+u.Email+".")
}

// ---- TLS ----

func (s *Backend) tlsViewFor(r *http.Request) (apiclient.TLSView, error) {
	ctx := r.Context()
	l := s.cfg()
	v := apiclient.TLSView{Mode: l.tlsMode, Active: l.tlsCert != nil, HTTP3Active: l.http3, Port: l.httpsPort}
	var err error
	if v.Names, err = s.settingValue(ctx, settingTLSNames); err != nil {
		return v, err
	}
	h3, err := s.settingValue(ctx, settingHTTP3)
	v.HTTP3 = h3 == valueEnabled
	if l.tlsInfo != nil {
		v.Subject, v.CertNames = l.tlsInfo.Subject, strings.Join(l.tlsInfo.Names, ", ")
		v.NotAfter, v.Fingerprint = l.tlsInfo.NotAfter.UTC().Format(dateLayout), l.tlsInfo.Fingerprint
	}
	return v, err
}

// validName accepts a DNS name or an IP address for a self-signed certificate.
func validName(n string) bool {
	if n == "" || len(n) > 253 || strings.ContainsAny(n, " /\\@?#\t\r\n,") {
		return false
	}
	return true
}

func (s *Backend) internalAdminSaveTLS(w http.ResponseWriter, r *http.Request, _ models.User) {
	var in apiclient.TLSForm
	if !internalDecode(w, r, &in) {
		return
	}
	ctx := r.Context()
	mode := in.Mode
	switch mode {
	case apiclient.TLSOff, apiclient.TLSUpload, apiclient.TLSSelfSigned:
	default:
		adminMsg(w, "Choose off, an uploaded certificate or a self-signed certificate.")
		return
	}
	port := strings.TrimSpace(in.Port)
	if port == "" {
		port = defaultHTTPSPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		adminMsg(w, "The HTTPS port is a number from 1 to 65535.")
		return
	}
	if in.HTTP3 && mode == apiclient.TLSOff {
		adminMsg(w, "HTTP/3 needs TLS: choose a certificate first.")
		return
	}
	prev, err := s.settingValue(ctx, settingTLSMode)
	if err != nil {
		internalError(w, err)
		return
	}
	switch mode {
	case apiclient.TLSUpload:
		cert, key := strings.TrimSpace(in.Cert), strings.TrimSpace(in.Key)
		if cert == "" && key == "" && prev == apiclient.TLSUpload {
			break // keep the stored one
		}
		if _, _, err := tlsutil.Parse(cert, key); err != nil {
			adminMsg(w, err.Error())
			return
		}
		if err := s.store.PutSetting(ctx, settingTLSCert, cert); err != nil {
			internalError(w, err)
			return
		}
		if err := s.store.PutSetting(ctx, settingTLSKey, key); err != nil {
			internalError(w, err)
			return
		}
	case apiclient.TLSSelfSigned:
		names := strings.TrimSpace(in.Names)
		for _, n := range strings.Split(names, ",") {
			if n = strings.TrimSpace(n); names != "" && !validName(n) {
				adminMsg(w, "A name for the certificate is a host name or an IP address, without spaces.")
				return
			}
		}
		if prev != apiclient.TLSSelfSigned { // never reuse an uploaded certificate as a self-signed one
			for _, name := range []string{settingTLSCert, settingTLSKey} {
				if err := s.store.DeleteSetting(ctx, name); err != nil {
					internalError(w, err)
					return
				}
			}
		}
		if err := s.putOrClear(ctx, settingTLSNames, names); err != nil {
			internalError(w, err)
			return
		}
	}
	h3 := valueDisabled
	if in.HTTP3 {
		h3 = valueEnabled
	}
	if err := s.store.PutSetting(ctx, settingTLSMode, mode); err != nil {
		internalError(w, err)
		return
	}
	if err := s.putOrClear(ctx, settingHTTPS, strings.TrimSpace(in.Port)); err != nil {
		internalError(w, err)
		return
	}
	if err := s.store.PutSetting(ctx, settingHTTP3, h3); err != nil {
		internalError(w, err)
		return
	}
	if err := s.reloadSettings(ctx); err != nil {
		internalError(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "tls="+mode)
	switch {
	case mode != apiclient.TLSOff && s.cfg().tlsCert == nil:
		adminMsgChanged(w, "The certificate could not be used, so TLS stays off. Check the server log.")
	case mode == apiclient.TLSOff:
		adminMsgChanged(w, "TLS is off. The server accepts plain HTTP only.")
	default:
		msg := "HTTPS is on, on port " + s.cfg().httpsPort + " (HTTP/2 and HTTP/1.1). Plain HTTP stays on its own port."
		if in.HTTP3 {
			msg += " HTTP/3 listens on UDP port " + s.cfg().httpsPort + "; make sure it is open too."
		}
		// The frontend owns the listener: if it could not bind, its error replaces this message.
		internalJSON(w, http.StatusOK, apiclient.AdminResult{Message: msg, ConfigChanged: true, ListenCheck: true})
	}
}
