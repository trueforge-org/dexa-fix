package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	settingWithingsClientID = "withings.client_id"
	settingWithingsSecret   = "withings.client_secret"
	settingWithingsRedirect = "withings.redirect_url"
)

var providerSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type adminView struct {
	Status          store.Stats
	Version         string
	GoVersion       string
	Users           []adminUser
	PasswordEnabled bool
	PasswordForced  bool
	SignupOpen      bool
	Providers       []adminProvider
	Form            adminProvider // the add or edit form
	Editing         bool
	Withings        adminWithings
	Audit           []auditLine // the latest security events
	Key             keyView
}

// keyView describes the encryption key on the Admin page. Rotation is manual (docs/key-rotation.md);
// the page only shows the key's age and asks for a rotation when a reminder is configured.
type keyView struct {
	Shown   bool
	ID      string
	Since   string // date the key was introduced, at least; empty when unknown
	AgeDays int
	Due     bool // older than the reminder
	Rekey   bool // the database still belongs to a previous key
}

func (s *Server) keyView(st store.Stats) keyView {
	v := keyView{Shown: st.Encrypted, ID: st.KeyID, Rekey: st.KeyPrevious}
	if !st.KeySince.IsZero() {
		v.Since = st.KeySince.UTC().Format(dateLayout)
		v.AgeDays = int(time.Since(st.KeySince) / (24 * time.Hour))
		v.Due = s.KeyReminder > 0 && time.Since(st.KeySince) > s.KeyReminder
	}
	return v
}

type adminUser struct {
	ID      int64
	Name    string
	Email   string
	Admin   bool
	Self    bool
	Created string
	SignIn  string
	// StrengthLevel is whether the user may sync to Strength Level automatically.
	StrengthLevel bool
	// StrengthLevelBulk is whether the user may also run the multi-day sync from Settings.
	StrengthLevelBulk bool
}

type adminProvider struct {
	models.AuthProvider
	CallbackURL string
	HasSecret   bool
}

type adminWithings struct {
	Available       bool
	Configured      bool
	ClientID        string
	RedirectURL     string
	DefaultRedirect string // used when the field is left empty
	SecretSaved     bool
}

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin", s.admin(s.adminPage))
	mux.HandleFunc("POST /admin/users", s.admin(s.adminAddUser))
	mux.HandleFunc("POST /admin/users/{id}/role", s.admin(s.adminSetRole))
	mux.HandleFunc("POST /admin/users/{id}/strengthlevel", s.admin(s.adminSetStrengthLevel))
	mux.HandleFunc("POST /admin/users/{id}/strengthlevel-bulk", s.admin(s.adminSetStrengthLevelBulk))
	mux.HandleFunc("POST /admin/users/{id}/password", s.admin(s.adminSetPassword))
	mux.HandleFunc("POST /admin/users/{id}/delete", s.admin(s.adminDeleteUser))
	mux.HandleFunc("POST /admin/password-login", s.admin(s.adminPasswordLogin))
	mux.HandleFunc("POST /admin/signup", s.admin(s.adminSignup))
	mux.HandleFunc("POST /admin/providers", s.admin(s.adminSaveProvider))
	mux.HandleFunc("POST /admin/providers/{slug}/delete", s.admin(s.adminDeleteProvider))
	mux.HandleFunc("POST /admin/withings", s.admin(s.adminSaveWithings))
}

// admin restricts a page to administrators. It answers 404 to everyone else so that the page does
// not advertise itself.
func (s *Server) admin(h func(http.ResponseWriter, *http.Request, models.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		if !u.IsAdmin() {
			http.NotFound(w, r)
			return
		}
		h(w, r, *u)
	}
}

func adminRedirect(w http.ResponseWriter, r *http.Request, msg string) {
	if len(msg) > maxFlashLen {
		msg = msg[:maxFlashLen]
	}
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	stats, err := s.store.Stats(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	ids, err := s.store.IdentityProviders(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	providers, err := s.store.ListAuthProviders(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	view := &adminView{
		Status: stats, Version: s.Version, GoVersion: runtime.Version(),
		PasswordEnabled: s.passwordEnabled(ctx), PasswordForced: s.ForcePasswordLogin,
		Withings: s.adminWithingsView(r), Key: s.keyView(stats),
		Form: adminProvider{AuthProvider: models.AuthProvider{Kind: models.ProviderKindOIDC}},
	}
	if view.SignupOpen, err = s.signupOpen(ctx); err != nil {
		s.fail(w, err)
		return
	}
	if view.Version == "" {
		view.Version = "dev"
	}
	for _, x := range users {
		signIn := strings.Join(ids[x.ID], ", ")
		if signIn == "" {
			signIn = "None"
		}
		view.Users = append(view.Users, adminUser{x.ID, x.Name, x.Email, x.IsAdmin(), x.ID == u.ID, x.CreatedAt.UTC().Format(dateLayout), signIn, x.StrengthLevelAuto, x.StrengthLevelBulk})
	}
	events, err := s.store.ListAuditEvents(ctx, store.AuditFilter{Limit: auditPageSize})
	if err != nil {
		s.fail(w, err)
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
		ap := adminProvider{AuthProvider: p, CallbackURL: externalURL(r, "/auth/"+p.Slug+"/callback"), HasSecret: p.ClientSecret != ""}
		ap.ClientSecret = "" // never sent back to the browser
		view.Providers = append(view.Providers, ap)
		if p.Slug == edit {
			view.Form, view.Editing = ap, true
		}
	}
	s.render(w, "admin.html", http.StatusOK, pageData{Title: "Admin · dexa-fix", Tab: "admin", User: &u, Msg: flashParam(r), Admin: view})
}

func (s *Server) targetUser(w http.ResponseWriter, r *http.Request) (models.User, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return models.User{}, false
	}
	t, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return models.User{}, false
	}
	return t, true
}

func (s *Server) adminAddUser(w http.ResponseWriter, r *http.Request, _ models.User) {
	email, ok := parseEmail(r.PostFormValue("email"))
	if !ok {
		adminRedirect(w, r, "Please enter a valid e-mail address.")
		return
	}
	password := r.PostFormValue("password")
	if msg := validatePassword(password, password); msg != "" {
		adminRedirect(w, r, msg)
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.store.CreateUserWithIdentity(r.Context(), r.PostFormValue("name"), email, models.Identity{Provider: models.ProviderLocal, Secret: hash})
	switch {
	case errors.Is(err, store.ErrExists):
		adminRedirect(w, r, "An account with that e-mail address already exists.")
	case errors.Is(err, store.ErrInvalid):
		adminRedirect(w, r, "Please enter a name of 1-64 characters.")
	case err != nil:
		s.fail(w, err)
	default:
		s.audit(r, u.ID, auditUserAdded, "")
		adminRedirect(w, r, u.Name+" was added.")
	}
}

func (s *Server) adminSignup(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	if r.PostFormValue("enabled") == "1" {
		if err := s.store.PutSetting(ctx, settingSignup, valueEnabled); err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "signup=open")
		adminRedirect(w, r, "New accounts are open.")
		return
	}
	if err := s.store.DeleteSetting(ctx, settingSignup); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "signup=closed")
	adminRedirect(w, r, "New accounts are closed. Add people from the Users list.")
}

func (s *Server) adminSetRole(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	role := models.RoleUser
	if r.PostFormValue("role") == models.RoleAdmin {
		role = models.RoleAdmin
	}
	switch err := s.store.SetUserRole(r.Context(), t.ID, role); {
	case errors.Is(err, store.ErrLastAdmin):
		adminRedirect(w, r, "There must be at least one administrator.")
	case err != nil:
		s.fail(w, err)
	default:
		s.audit(r, t.ID, auditRoleChanged, role)
		adminRedirect(w, r, t.Name+" is now "+role+".")
	}
}

func (s *Server) adminSetStrengthLevel(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	on := r.PostFormValue("enabled") == "1"
	if err := s.store.SetUserStrengthLevelAuto(r.Context(), t.ID, on); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, t.ID, auditStrengthLevel, onOff(on))
	if on {
		adminRedirect(w, r, t.Name+" may now store a Strength Level login and send single days.")
		return
	}
	s.sl.stop(t.ID)
	adminRedirect(w, r, "Strength Level sync is off for "+t.Name+", and their stored login is deleted.")
}

func (s *Server) adminSetStrengthLevelBulk(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if !t.StrengthLevelAuto {
		adminRedirect(w, r, "Enable Strength Level sync for "+t.Name+" first.")
		return
	}
	on := r.PostFormValue("enabled") == "1"
	if err := s.store.SetUserStrengthLevelBulk(r.Context(), t.ID, on); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, t.ID, auditStrengthLevelBlk, onOff(on))
	if on {
		adminRedirect(w, r, t.Name+" may now run the multi-day Strength Level sync from Settings.")
		return
	}
	s.sl.stop(t.ID)
	adminRedirect(w, r, "The multi-day Strength Level sync is off for "+t.Name+".")
}

func (s *Server) adminSetPassword(w http.ResponseWriter, r *http.Request, _ models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	password := r.PostFormValue("password")
	if msg := validatePassword(password, password); msg != "" {
		adminRedirect(w, r, msg)
		return
	}
	if err := s.setPassword(r.Context(), t.ID, password); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteUserSessions(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.DeleteUserAccessTokens(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, t.ID, auditPasswordSet, "")
	adminRedirect(w, r, "Password set for "+t.Name+".")
}

func (s *Server) adminDeleteUser(w http.ResponseWriter, r *http.Request, u models.User) {
	t, ok := s.targetUser(w, r)
	if !ok {
		return
	}
	if t.ID == u.ID {
		adminRedirect(w, r, "You cannot delete your own account.")
		return
	}
	switch err := s.store.DeleteUser(r.Context(), t.ID); {
	case errors.Is(err, store.ErrLastAdmin):
		adminRedirect(w, r, "There must be at least one administrator.")
	case err != nil:
		s.fail(w, err)
	default:
		s.audit(r, 0, auditUserDeleted, "user_id="+strconv.FormatInt(t.ID, 10))
		adminRedirect(w, r, t.Name+" and their data were deleted.")
	}
}

// hasExternalSignIn reports whether the user can sign in through a provider other than excluded.
func hasExternalSignIn(providers []string, excluded string) bool {
	return slices.ContainsFunc(providers, func(p string) bool { return p != models.ProviderLocal && p != excluded })
}

func (s *Server) adminPasswordLogin(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	if r.PostFormValue("enabled") == "1" {
		if err := s.store.DeleteSetting(ctx, settingPasswordOn); err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, 0, auditSettingChanged, "password_login=on")
		adminRedirect(w, r, "Password sign-in is on.")
		return
	}
	ids, err := s.store.IdentityProviders(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !hasExternalSignIn(ids[u.ID], "") {
		adminRedirect(w, r, "Link your account to a sign-in provider in Settings first, so you are not locked out.")
		return
	}
	if err := s.store.PutSetting(ctx, settingPasswordOn, valueDisabled); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "password_login=off")
	adminRedirect(w, r, "Password sign-in is off. People sign in through a provider, and scripts use access tokens.")
}

func validProviderURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return u.Scheme == "https" || (u.Scheme == "http" && (h == "localhost" || h == "127.0.0.1" || h == "::1"))
}

func (s *Server) adminSaveProvider(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	field := func(n string) string { return strings.TrimSpace(r.PostFormValue(n)) }
	p := models.AuthProvider{
		Slug: field("slug"), Label: field("label"), Kind: field("kind"), Issuer: strings.TrimRight(field("issuer"), "/"),
		AuthURL: field("auth_url"), TokenURL: field("token_url"), UserinfoURL: field("userinfo_url"),
		ClientID: field("client_id"), ClientSecret: strings.TrimSpace(r.PostFormValue("client_secret")),
		AppleTeamID: field("apple_team_id"), AppleKeyID: field("apple_key_id"),
		Scopes: field("scopes"), SubjectClaim: field("subject_claim"), NameClaim: field("name_claim"),
		AllowSignup: r.PostFormValue("allow_signup") != "",
	}
	if !providerSlug.MatchString(p.Slug) || p.Slug == models.ProviderLocal {
		adminRedirect(w, r, "The short name may use lowercase letters, digits and dashes, up to 32 characters.")
		return
	}
	if p.Label == "" || p.ClientID == "" {
		adminRedirect(w, r, "A display name and client ID are required.")
		return
	}
	old, err := s.store.GetAuthProvider(ctx, p.Slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if p.ClientSecret == "" {
		p.ClientSecret = old.ClientSecret
	}
	if msg := s.prepareProvider(ctx, &p); msg != "" {
		adminRedirect(w, r, msg)
		return
	}
	if err := s.store.PutAuthProvider(ctx, p); err != nil {
		s.fail(w, err)
		return
	}
	s.forgetOIDCProvider(old.Issuer)
	s.audit(r, 0, auditProviderSaved, p.Slug)
	adminRedirect(w, r, p.Label+" saved.")
}

// prepareProvider validates a provider by kind and fills in defaults. It returns a message for the
// administrator, or "" when the provider is ready to save.
func (s *Server) prepareProvider(ctx context.Context, p *models.AuthProvider) string {
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

func (s *Server) adminDeleteProvider(w http.ResponseWriter, r *http.Request, u models.User) {
	ctx := r.Context()
	slug := r.PathValue("slug")
	p, err := s.store.GetAuthProvider(ctx, slug)
	if err != nil {
		s.fail(w, err)
		return
	}
	if !s.passwordEnabled(ctx) {
		ids, err := s.store.IdentityProviders(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !hasExternalSignIn(ids[u.ID], slug) {
			adminRedirect(w, r, "Password sign-in is off and this is your only way in. Turn passwords on or link another provider first.")
			return
		}
	}
	if err := s.store.DeleteAuthProvider(ctx, slug); err != nil {
		s.fail(w, err)
		return
	}
	s.forgetOIDCProvider(p.Issuer)
	s.audit(r, 0, auditProviderRemoved, slug)
	adminRedirect(w, r, p.Label+" removed.")
}

// ---- Withings application credentials ----

func (s *Server) withingsConfigurable() (importer.Configurable, bool) {
	imp, ok := s.importers.Get(providerWithings)
	if !ok {
		return nil, false
	}
	c, ok := imp.(importer.Configurable)
	return c, ok
}

func (s *Server) settingOrEmpty(ctx context.Context, name string) (string, error) {
	v, err := s.store.GetSetting(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return v, err
}

// applyWithingsCredentials loads the credentials saved by an administrator into the importer.
func (s *Server) applyWithingsCredentials(ctx context.Context) error {
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

func (s *Server) adminWithingsView(r *http.Request) adminWithings {
	ctx := r.Context()
	if _, ok := s.withingsConfigurable(); !ok {
		return adminWithings{}
	}
	id, _ := s.settingOrEmpty(ctx, settingWithingsClientID)
	secret, _ := s.settingOrEmpty(ctx, settingWithingsSecret)
	redirect, _ := s.settingOrEmpty(ctx, settingWithingsRedirect)
	v := adminWithings{
		Available: true, ClientID: id, RedirectURL: redirect, DefaultRedirect: withingsCallback(r),
		SecretSaved: secret != "",
	}
	if p, ok := s.oauthProvider(providerWithings); ok {
		v.Configured = p.Configured()
	}
	return v
}

func (s *Server) adminSaveWithings(w http.ResponseWriter, r *http.Request, _ models.User) {
	ctx := r.Context()
	if _, ok := s.withingsConfigurable(); !ok {
		http.NotFound(w, r)
		return
	}
	save := func(name, value string) error {
		if value == "" {
			return s.store.DeleteSetting(ctx, name)
		}
		return s.store.PutSetting(ctx, name, value)
	}
	id, redirect := strings.TrimSpace(r.PostFormValue("client_id")), strings.TrimSpace(r.PostFormValue("redirect_url"))
	if redirect != "" && !validProviderURL(redirect) {
		adminRedirect(w, r, "The redirect URL must use https, or http on localhost.")
		return
	}
	if redirect == "" && id != "" {
		redirect = withingsCallback(r) // this server's own callback, as the browser reaches it
	}
	err := save(settingWithingsClientID, id)
	if err == nil {
		err = save(settingWithingsRedirect, redirect)
	}
	if secret := strings.TrimSpace(r.PostFormValue("client_secret")); err == nil && secret != "" {
		err = save(settingWithingsSecret, secret)
	} else if err == nil && r.PostFormValue("clear_secret") != "" {
		err = save(settingWithingsSecret, "")
	}
	if err == nil {
		err = s.applyWithingsCredentials(ctx)
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, 0, auditSettingChanged, "withings")
	adminRedirect(w, r, "Withings settings saved.")
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
