package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// The Admin pages. Every call needs an administrator who has passed the step-up check; the backend
// answers CodeUnauthorized, CodeForbidden or CodeStepUp otherwise.

// AdminResult is the outcome of a change. Message is the flash text for the Admin page (refusals
// the administrator can fix are messages too, not errors). ConfigChanged is true when the change
// touched the settings the frontend acts on (base URL, proxies, TLS): the frontend fetches them
// again before it answers. ListenCheck is true when the frontend's own listener state decides the
// message (HTTPS was turned on). ResetLink is set instead of a Message when a link was made.
type AdminResult struct {
	Message       string     `json:"message,omitempty"`
	ConfigChanged bool       `json:"config_changed,omitempty"`
	ListenCheck   bool       `json:"listen_check,omitempty"`
	ResetLink     *ResetLink `json:"reset_link,omitempty"`
}

// ResetLink is the one-time link shown to the administrator who made it.
type ResetLink struct {
	For     string
	URL     string
	Expires string
	Emailed bool
	Warning bool // made from the request's host, which the request controls
}

// AdminStatus is the database and usage figures of the Admin page.
type AdminStatus struct {
	Driver        string
	Encrypted     bool
	SchemaVersion int64
	Users         int
	Admins        int
	DexaScans     int
	ScaleReadings int
	// BodyUnavailable is set when the service that holds the scans and readings did not answer, so
	// the two counts above are unknown and not zero.
	BodyUnavailable bool
	Sessions        int
	AccessTokens    int
	TwoFactorUsers  int
}

// KeyView describes the encryption key on the Admin page.
type KeyView struct {
	Shown   bool
	ID      string
	Since   string // date the key was introduced, at least; empty when unknown
	AgeDays int
	Due     bool // older than the reminder
	Rekey   bool // the database still belongs to a previous key
}

type AdminUser struct {
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
	TwoFactor         bool // two-step verification is on
	// JobPriority (1-100) orders the user's worker jobs among other users'; a higher number goes
	// first, and administrators always go before everyone.
	JobPriority int
}

// AdminProvider is a sign-in provider as the Admin page shows it. The secret is never sent.
type AdminProvider struct {
	models.AuthProvider
	CallbackURL string
	HasSecret   bool
}

type AdminWithings struct {
	Available       bool
	Configured      bool
	ClientID        string
	RedirectURL     string
	DefaultRedirect string // used when the field is left empty
	SecretSaved     bool
}

// ServerSettingsView is the stored text of the network and retention settings.
type ServerSettingsView struct {
	BaseURL         string
	TrustedProxies  string
	ProxyProtocol   bool
	AuditDays       string
	KeyReminderDays string
}

// SMTPView is what the Admin page shows and saves for e-mail.
type SMTPView struct {
	Host, Username, From, TLS string
	Port                      int
	PasswordSaved             bool
}

// TLSView is the secure connections section, without what only the frontend knows (whether its
// listener could bind).
type TLSView struct {
	Mode        string // off, upload or selfsigned
	Names       string // names for a self-signed certificate, as typed
	Active      bool   // a certificate is in force
	Subject     string
	CertNames   string
	NotAfter    string
	Fingerprint string
	HTTP3       bool
	HTTP3Active bool
	Port        string
}

// AdminPage is the data of the Admin page.
type AdminPage struct {
	Status          AdminStatus
	Users           []AdminUser
	PasswordEnabled bool
	PasswordForced  bool
	SignupOpen      bool
	Providers       []AdminProvider
	Form            AdminProvider // the add or edit form
	Editing         bool
	Withings        AdminWithings
	Audit           []AuditLine
	Key             KeyView
	Require2FA      string // off, admins or everyone
	Server          ServerSettingsView
	SMTP            SMTPView
	ResetByEmail    bool // users can ask for a reset e-mail
	TLS             TLSView
}

// AdminPage fetches the page; edit is the slug of the provider to edit, or empty.
func (c *Client) AdminPage(ctx context.Context, edit string) (AdminPage, error) {
	var p AdminPage
	err := c.Get(ctx, "/admin/page?edit="+url.QueryEscape(edit), &p)
	return p, err
}

func (c *Client) adminDo(ctx context.Context, method, path string, in any) (AdminResult, error) {
	var out AdminResult
	_, err := c.Do(ctx, method, path, in, &out)
	return out, err
}

func adminUserPath(id int64, tail string) string {
	return "/admin/users/" + strconv.FormatInt(id, 10) + tail
}

// AdminAddUser creates an account with a password.
func (c *Client) AdminAddUser(ctx context.Context, name, email, password string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/users", map[string]string{"name": name, "email": email, "password": password})
}

func (c *Client) AdminSetRole(ctx context.Context, id int64, role string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(id, "/role"), map[string]string{"role": role})
}

func (c *Client) AdminSetStrengthLevel(ctx context.Context, id int64, on bool) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(id, "/strengthlevel"), map[string]bool{"enabled": on})
}

func (c *Client) AdminSetStrengthLevelBulk(ctx context.Context, id int64, on bool) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(id, "/strengthlevel-bulk"), map[string]bool{"enabled": on})
}

func (c *Client) AdminSetPassword(ctx context.Context, id int64, password string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(id, "/password"), map[string]string{"password": password})
}

func (c *Client) AdminDeleteUser(ctx context.Context, id int64) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodDelete, adminUserPath(id, ""), nil)
}

// AdminRemoveTwoFactor removes a user's second step.
func (c *Client) AdminRemoveTwoFactor(ctx context.Context, id int64) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodDelete, adminUserPath(id, "/2fa"), nil)
}

// AdminResetLink makes a one-time reset link, and e-mails it too when email is set and possible.
func (c *Client) AdminResetLink(ctx context.Context, id int64, email bool) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, adminUserPath(id, "/reset-link"), map[string]bool{"email": email})
}

func (c *Client) AdminPasswordLogin(ctx context.Context, on bool) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/password-login", map[string]bool{"enabled": on})
}

func (c *Client) AdminSignup(ctx context.Context, open bool) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/signup", map[string]bool{"enabled": open})
}

// AdminTwoFactorPolicy sets who must use two-step verification: off, admins or everyone.
func (c *Client) AdminTwoFactorPolicy(ctx context.Context, require string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/two-factor", map[string]string{"require": require})
}

// ProviderForm is the text of the provider form, as typed.
type ProviderForm struct {
	Slug, Label, Kind, Issuer, AuthURL, TokenURL, UserinfoURL, ClientID, ClientSecret string
	AppleTeamID, AppleKeyID, Scopes, SubjectClaim, NameClaim                          string
	AllowSignup                                                                       bool
}

func (c *Client) AdminSaveProvider(ctx context.Context, f ProviderForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/providers", f)
}

func (c *Client) AdminDeleteProvider(ctx context.Context, slug string) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodDelete, "/admin/providers/"+url.PathEscape(slug), nil)
}

// WithingsForm is the text of the Withings credentials form.
type WithingsForm struct {
	ClientID, RedirectURL, ClientSecret string
	ClearSecret                         bool
}

func (c *Client) AdminSaveWithings(ctx context.Context, f WithingsForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/withings", f)
}

// ServerForm is the text of the server settings form, as typed.
type ServerForm struct {
	BaseURL, TrustedProxies, AuditDays, KeyReminderDays string
	ProxyProtocol                                       bool
}

func (c *Client) AdminSaveServer(ctx context.Context, f ServerForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/server", f)
}

// SMTPForm is the text of the e-mail form, as typed.
type SMTPForm struct {
	Host, Port, Username, Password, From, TLS string
	RemovePassword                            bool
}

func (c *Client) AdminSaveSMTP(ctx context.Context, f SMTPForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/smtp", f)
}

func (c *Client) AdminTestSMTP(ctx context.Context) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/smtp/test", nil)
}

// TLSForm is the text of the TLS form, as typed.
type TLSForm struct {
	Mode, Port, Cert, Key, Names string
	HTTP3                        bool
}

func (c *Client) AdminSaveTLS(ctx context.Context, f TLSForm) (AdminResult, error) {
	return c.adminDo(ctx, http.MethodPost, "/admin/tls", f)
}
