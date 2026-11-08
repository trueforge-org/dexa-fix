// Package withings imports body-scale measurements from the Withings API.
//
// Each user links their own Withings account through OAuth2; the tokens are kept in
// user_integrations (provider "withings"). Measure groups are stored as scale readings with
// Source "withings" and the group id as ExternalID, so repeated imports are idempotent.
//
// A bulk import fetches the user's whole history (or everything since Options.Since) and applies
// the rules documented on plan: days without readings are filled first, days with manual readings
// are left alone. A day re-import (Options.Day) replaces that single day.
package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Name is the provider name, also used as the readings' Source.
const Name = "withings"

// refreshMargin is how long before expiry the access token is refreshed.
const refreshMargin = time.Minute

// Importer imports Withings measurements.
type Importer struct {
	base Config // from the server configuration

	cfgMu sync.RWMutex
	cfg   Config // base with any credentials saved by an administrator

	http *http.Client
	now  func() time.Time
	// userLocks holds one mutex per user: refresh tokens are single use, so one user's refreshes
	// must not race, but a slow refresh for one user must not hold up the others.
	userLocks sync.Map // int64 -> *sync.Mutex
}

func (i *Importer) lockUser(userID int64) func() {
	m, _ := i.userLocks.LoadOrStore(userID, new(sync.Mutex))
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

var (
	_ importer.Importer      = (*Importer)(nil)
	_ importer.OAuthProvider = (*Importer)(nil)
	_ importer.Configurable  = (*Importer)(nil)
)

// New returns an importer for the given application settings. A nil client uses a 30s timeout.
func New(cfg Config, client *http.Client) *Importer {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Importer{base: cfg, cfg: cfg, http: client, now: time.Now}
}

// Name returns the provider name.
func (*Importer) Name() string { return Name }

// Configured reports whether client credentials and a redirect URL are set.
func (i *Importer) Configured() bool {
	if i == nil {
		return false
	}
	c := i.config()
	return c.ClientID != "" && c.ClientSecret != "" && c.RedirectURL != ""
}

func (i *Importer) config() Config {
	i.cfgMu.RLock()
	defer i.cfgMu.RUnlock()
	return i.cfg
}

// BaseCredentials returns the credentials from the server configuration.
func (i *Importer) BaseCredentials() importer.Credentials {
	return importer.Credentials{ClientID: i.base.ClientID, ClientSecret: i.base.ClientSecret, RedirectURL: i.base.RedirectURL}
}

// SetCredentials applies overrides; an empty field falls back to the server configuration.
func (i *Importer) SetCredentials(c importer.Credentials) {
	next := i.base
	if c.ClientID != "" {
		next.ClientID = c.ClientID
	}
	if c.ClientSecret != "" {
		next.ClientSecret = c.ClientSecret
	}
	if c.RedirectURL != "" {
		next.RedirectURL = c.RedirectURL
	}
	i.cfgMu.Lock()
	defer i.cfgMu.Unlock()
	i.cfg = next
}

// AuthURL returns the Withings authorisation URL.
func (i *Importer) AuthURL(state string) string { return i.config().authURL(state) }

// Connected reports whether the user has linked a Withings account.
func (i *Importer) Connected(ctx context.Context, st store.Store, userID int64) bool {
	_, err := st.GetIntegration(ctx, userID, Name)
	return err == nil
}

// Connect exchanges the authorisation code and stores the user's tokens.
func (i *Importer) Connect(ctx context.Context, st store.Store, userID int64, code string) error {
	if !i.Configured() {
		return importer.ErrNotConfigured
	}
	t, err := i.requestToken(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {i.config().RedirectURL},
	})
	if err != nil {
		return err
	}
	return saveTokens(ctx, st, userID, t)
}

// Disconnect forgets the user's tokens.
func (i *Importer) Disconnect(ctx context.Context, st store.Store, userID int64) error {
	return st.DeleteIntegration(ctx, userID, Name)
}

// saveTokens stores a token set. A refresh token is single use: once Withings handed out a new
// pair, failing to store it would leave the old, now invalid, pair behind and force the user to
// reconnect. So the save is retried, and it carries on if the request that triggered the refresh
// was cancelled in the meantime.
func saveTokens(ctx context.Context, st store.Store, userID int64, t tokens) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	for attempt := 0; ; attempt++ {
		if err = st.PutIntegration(ctx, userID, Name, data); err == nil || attempt == 2 {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryBackoff(attempt)):
		}
	}
}

// accessToken returns a valid access token for the user, refreshing (and persisting) it if needed.
func (i *Importer) accessToken(ctx context.Context, st store.Store, userID int64) (string, error) {
	defer i.lockUser(userID)()
	data, err := st.GetIntegration(ctx, userID, Name)
	if errors.Is(err, store.ErrNotFound) {
		return "", importer.ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	var t tokens
	if err := json.Unmarshal(data, &t); err != nil || t.RefreshToken == "" {
		return "", fmt.Errorf("withings: stored tokens are unreadable, reconnect: %w", importer.ErrNotConnected)
	}
	if i.now().Add(refreshMargin).Before(t.ExpiresAt) {
		return t.AccessToken, nil
	}
	nt, err := i.requestToken(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
	if err != nil {
		return "", fmt.Errorf("refresh withings token (reconnect if this persists): %w", err)
	}
	if err := saveTokens(ctx, st, userID, nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

// Import fetches measurements and stores them according to the rules on plan.
func (i *Importer) Import(ctx context.Context, st store.Store, userID int64, o importer.Options) (importer.Result, error) {
	if !i.Configured() {
		return importer.Result{}, importer.ErrNotConfigured
	}
	token, err := i.accessToken(ctx, st, userID)
	if err != nil {
		return importer.Result{}, err
	}
	user, err := st.GetUser(ctx, userID)
	if err != nil {
		return importer.Result{}, err
	}

	// Days are the user's calendar days, in their time zone. o.Day is a plain date.
	loc := user.Location()
	dayMode := !o.Day.IsZero()
	day := o.Day.UTC().Truncate(24 * time.Hour)
	var start, end time.Time
	switch {
	case dayMode:
		start = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
		end = start.AddDate(0, 0, 1).Add(-time.Second)
	case !o.Since.IsZero():
		start = o.Since
	}
	groups, err := i.getMeas(ctx, token, start, end)
	if err != nil {
		return importer.Result{}, err
	}
	fetched, seen := mapGroups(groups)
	for k := range fetched {
		fetched[k].AthleteMode = user.AthleteDefault // new readings follow the user's default
	}

	existing, err := st.ListScaleReadings(ctx, userID)
	if err != nil {
		return importer.Result{}, err
	}
	c := plan(existing, fetched, seen, dayMode, day, loc)

	// Days without any readings go first so a failure further down still keeps them.
	if len(c.Gap) > 0 {
		if _, err := st.UpsertScaleReadings(ctx, userID, c.Gap); err != nil {
			return importer.Result{}, err
		}
	}
	if len(c.Other) > 0 {
		if _, err := st.UpsertScaleReadings(ctx, userID, c.Other); err != nil {
			return importer.Result{}, err
		}
	}
	for _, id := range c.Delete {
		if err := st.DeleteScaleReading(ctx, userID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return importer.Result{}, err
		}
	}
	return c.Result, nil
}
