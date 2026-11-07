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
	cfg  Config
	http *http.Client
	now  func() time.Time
	mu   sync.Mutex // refresh tokens are single use, so refreshes must not race
}

var (
	_ importer.Importer      = (*Importer)(nil)
	_ importer.OAuthProvider = (*Importer)(nil)
)

// New returns an importer for the given application settings. A nil client uses a 30s timeout.
func New(cfg Config, client *http.Client) *Importer {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Importer{cfg: cfg, http: client, now: time.Now}
}

// Name returns the provider name.
func (*Importer) Name() string { return Name }

// Configured reports whether client credentials and a redirect URL are set.
func (i *Importer) Configured() bool {
	return i != nil && i.cfg.ClientID != "" && i.cfg.ClientSecret != "" && i.cfg.RedirectURL != ""
}

// AuthURL returns the Withings authorisation URL.
func (i *Importer) AuthURL(state string) string { return i.cfg.authURL(state) }

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
		"redirect_uri": {i.cfg.RedirectURL},
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

func saveTokens(ctx context.Context, st store.Store, userID int64, t tokens) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return st.PutIntegration(ctx, userID, Name, data)
}

// accessToken returns a valid access token for the user, refreshing (and persisting) it if needed.
func (i *Importer) accessToken(ctx context.Context, st store.Store, userID int64) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
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

	dayMode := !o.Day.IsZero()
	day := dayOf(o.Day)
	var start, end time.Time
	switch {
	case dayMode:
		start, end = day, day.Add(24*time.Hour-time.Second)
	case !o.Since.IsZero():
		start = o.Since
	}
	groups, err := i.getMeas(ctx, token, start, end)
	if err != nil {
		return importer.Result{}, err
	}
	fetched, seen := mapGroups(groups)

	existing, err := st.ListScaleReadings(ctx, userID)
	if err != nil {
		return importer.Result{}, err
	}
	c := plan(existing, fetched, seen, dayMode, day)

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
