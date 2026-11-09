package apiclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Profile is the profile form of the settings page, as typed. The backend validates every field
// again; Current is the current password, asked for when the e-mail address changes.
type Profile struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Timezone string `json:"timezone"`
	HeightCm string `json:"height_cm"`
	Current  string `json:"current"`
}

// SaveProfile stores the profile.
func (c *Client) SaveProfile(ctx context.Context, userID int64, p Profile) error {
	return c.Post(ctx, userPath(userID, "/profile"), p, nil)
}

// PasswordChange is the change-password form.
type PasswordChange struct {
	Current  string `json:"current"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

// ChangePassword sets a new password. The backend signs the user in again with a new session (the
// Set-Cookie is relayed) and answers with the settings page data, since the old session is gone.
func (c *Client) ChangePassword(ctx context.Context, userID int64, p PasswordChange) (SettingsPage, error) {
	var pg SettingsPage
	err := c.Post(ctx, userPath(userID, "/password"), p, &pg)
	return pg, err
}

// NewTokenRequest is the create-token form.
type NewTokenRequest struct {
	Name    string `json:"name"`
	Expires string `json:"expires"` // days, "never" or empty
	Current string `json:"current"`
}

// CreateToken creates an access token and returns it. It is shown once and never stored in clear.
func (c *Client) CreateToken(ctx context.Context, userID int64, rq NewTokenRequest) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	err := c.Post(ctx, userPath(userID, "/tokens"), rq, &out)
	return out.Token, err
}

// DeleteToken revokes one of the user's access tokens.
func (c *Client) DeleteToken(ctx context.Context, userID, id int64) error {
	return c.Delete(ctx, userPath(userID, "/tokens/"+strconv.FormatInt(id, 10)))
}

// UnlinkIdentity unlinks a sign-in provider. The answer is the message to show, whether the
// account was unlinked or not.
func (c *Client) UnlinkIdentity(ctx context.Context, userID int64, provider string) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	_, err := c.Do(ctx, http.MethodDelete, userPath(userID, "/identities/"+url.PathEscape(provider)), nil, &out)
	return out.Message, err
}

// Logout ends the visitor's session. The cleared cookie is relayed.
func (c *Client) Logout(ctx context.Context) error {
	return c.Delete(ctx, "/session")
}
