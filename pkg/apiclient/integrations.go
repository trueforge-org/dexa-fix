package apiclient

import (
	"context"
	"net/url"
)

// Flash is the outcome of an action that ends with a message on a page. UserID is set where the
// backend, not the request path, says whose page it is (the OAuth callback).
type Flash struct {
	Message string `json:"message"`
	UserID  int64  `json:"user_id,omitempty"`
}

func providerPath(userID int64, provider, rest string) string {
	return userPath(userID, "/integrations/"+url.PathEscape(provider)+rest)
}

// Import fetches the provider's measurements now. day (YYYY-MM-DD) limits it to one day. The
// message summarises the result or the failure.
func (c *Client) Import(ctx context.Context, userID int64, provider, day string) (Flash, error) {
	var f Flash
	err := c.Post(ctx, userPath(userID, "/import/"+url.PathEscape(provider)), map[string]string{"day": day}, &f)
	return f, err
}

// ConnectIntegration starts the OAuth flow. The backend sets the state cookie (relayed); the
// result is the provider's authorize URL.
func (c *Client) ConnectIntegration(ctx context.Context, userID int64, provider string) (string, error) {
	var o struct {
		Redirect string `json:"redirect"`
	}
	err := c.Post(ctx, providerPath(userID, provider, "/connect"), nil, &o)
	return o.Redirect, err
}

// IntegrationCallback completes the OAuth flow with what the provider sent back.
func (c *Client) IntegrationCallback(ctx context.Context, provider, state, code, providerError string) (Flash, error) {
	var f Flash
	err := c.Post(ctx, "/integrations/"+url.PathEscape(provider)+"/callback",
		map[string]string{"state": state, "code": code, "error": providerError}, &f)
	return f, err
}

// DisconnectIntegration removes the user's link to the provider.
func (c *Client) DisconnectIntegration(ctx context.Context, userID int64, provider string) (Flash, error) {
	var f Flash
	err := c.Post(ctx, providerPath(userID, provider, "/disconnect"), nil, &f)
	return f, err
}

// SaveStrengthLevelLogin checks and stores the user's Strength Level login.
func (c *Client) SaveStrengthLevelLogin(ctx context.Context, userID int64, email, password string) (Flash, error) {
	var f Flash
	err := c.Post(ctx, userPath(userID, "/strengthlevel/login"), map[string]string{"email": email, "password": password}, &f)
	return f, err
}

// DeleteStrengthLevelLogin removes the stored login and stops a running sync.
func (c *Client) DeleteStrengthLevelLogin(ctx context.Context, userID int64) (Flash, error) {
	var f Flash
	_, err := c.Do(ctx, "DELETE", userPath(userID, "/strengthlevel/login"), nil, &f)
	return f, err
}

// StartStrengthLevelSync starts the multi-day sync. days is the raw form value.
func (c *Client) StartStrengthLevelSync(ctx context.Context, userID int64, days string, update bool) (Flash, error) {
	var f Flash
	err := c.Post(ctx, userPath(userID, "/strengthlevel/sync"), map[string]any{"days": days, "update": update}, &f)
	return f, err
}

// StrengthLevelStatus returns the latest sync job, or nil when there has been none.
func (c *Client) StrengthLevelStatus(ctx context.Context, userID int64) (*SLJobView, error) {
	var j *SLJobView
	err := c.Get(ctx, userPath(userID, "/strengthlevel/status"), &j)
	return j, err
}

// SendStrengthLevelDay sends one day's estimate to Strength Level.
func (c *Client) SendStrengthLevelDay(ctx context.Context, userID int64, day string) (Flash, error) {
	var f Flash
	err := c.Post(ctx, userPath(userID, "/strengthlevel/day"), map[string]string{"day": day}, &f)
	return f, err
}
