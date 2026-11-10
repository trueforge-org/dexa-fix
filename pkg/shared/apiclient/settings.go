package apiclient

import (
	"context"
	"html/template"
	"net/url"
	"strconv"

	"github.com/trueforge-org/dexafix/pkg/estimate"
)

// The views below are what the settings and estimates pages show. The backend builds them from its
// data; the frontend only renders them. The field names are the ones the templates use.

// IntegrationView is the state of an OAuth integration for the current user.
type IntegrationView struct {
	Configured bool // the server has the provider's credentials
	Connected  bool // the user has linked their account
	// AutoPaused is why automatic runs for the user were paused after repeated failures; empty when they are not.
	AutoPaused string
}

// AuditLine is one event as shown on a page.
type AuditLine struct {
	When, Action, Detail, IP string
	Actor, Target            string
}

// SecurityView is the settings page's sign-in section.
type SecurityView struct {
	Activity        []AuditLine // the latest events about this account
	PasswordEnabled bool
	HasPassword     bool
	ConfirmPassword bool // sensitive changes ask for the current password
	Tokens          []TokenView
	NewToken        string // shown once, right after it was created
	Linked          []LinkedProvider
	TwoFactor       TwoFactorSettings
	Passkeys        PasskeysView
}

type TokenView struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastUsed string
	Expires  string
}

type LinkedProvider struct {
	Slug, Label string
	Linked      bool
}

// TwoFactorSetup is a secret waiting to be confirmed.
type TwoFactorSetup struct {
	QR     template.HTML
	Secret string
}

// TwoFactorSettings is the two-step part of the settings page.
type TwoFactorSettings struct {
	Available    bool // the user signs in with a password, so there is something to protect
	Enabled      bool
	Required     bool // an administrator requires it for this user
	RecoveryLeft int
	Setup        *TwoFactorSetup
	NewCodes     []string // shown once
}

// PasskeysView is the passkey section of the settings page.
type PasskeysView struct {
	Ready bool // the browser can use passkeys here: HTTPS or localhost
	Items []PasskeyItem
}

type PasskeyItem struct {
	ID, Name, Created, LastUsed string
}

// SLJobView is what the settings page shows about a Strength Level job.
type SLJobView struct {
	Running     bool
	Text        string
	Done, Total int
}

// SLView is the Strength Level card of the settings page.
type SLView struct {
	UserID int64
	Saved  bool
	Email  string // the saved login's e-mail address; the password is never sent back
	Msg    string
	Bulk   bool // the multi-day sync is allowed
	// AutoPaused is why automatic syncs for the user were paused after repeated failures; empty when they are not.
	AutoPaused string
	Job        *SLJobView // the latest multi-day sync, if any
}

// SettingsPage is the data the settings page needs besides the user.
type SettingsPage struct {
	Withings      *IntegrationView
	StrengthLevel *SLView
	Security      *SecurityView
}

// SettingsPage returns the data of the settings page. msg is the flash message of the request; the
// Strength Level card shows it.
func (c *Client) SettingsPage(ctx context.Context, userID int64, msg string) (SettingsPage, error) {
	var p SettingsPage
	path := userPath(userID, "/pages/settings")
	if msg != "" {
		path += "?msg=" + url.QueryEscape(msg)
	}
	err := c.Get(ctx, path, &p)
	return p, err
}

// Settings is the estimation settings a user can save: whether new readings default to athlete mode
// and the method ranking, best first.
type Settings struct {
	AthleteDefault bool     `json:"athlete_default"`
	MethodRanking  []string `json:"method_ranking"`
}

// SaveSettings stores the estimation settings.
func (c *Client) SaveSettings(ctx context.Context, userID int64, s Settings) error {
	return c.Post(ctx, userPath(userID, "/settings"), s, nil)
}

// EstimatesPage is the data the estimates page needs besides the user: the estimates, the state of
// the Withings integration and whether the single-day Strength Level button is shown.
type EstimatesPage struct {
	Result   estimate.Result  `json:"result"`
	Withings *IntegrationView `json:"withings"`
	SLReady  bool             `json:"sl_ready"`
}

// EstimatesPage returns the data of the estimates page. With days > 0 the result holds only the days
// within that many days of the newest one (estimate.WithinDays) and all the calibrations; 0 means
// every day.
func (c *Client) EstimatesPage(ctx context.Context, userID int64, days int) (EstimatesPage, error) {
	var p EstimatesPage
	path := userPath(userID, "/pages/estimates")
	if days > 0 {
		path += "?days=" + strconv.Itoa(days)
	}
	err := c.Get(ctx, path, &p)
	return p, err
}

// SaveHiddenColumns stores which columns of the daily table are hidden.
func (c *Client) SaveHiddenColumns(ctx context.Context, userID int64, hidden []string) error {
	return c.Post(ctx, userPath(userID, "/estimates/columns"), struct {
		Hidden []string `json:"hidden"`
	}{hidden}, nil)
}
