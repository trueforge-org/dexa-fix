package apiclient

import "context"

// SecondStep is the answer to a request of the second step of signing in. Either Redirect is set,
// or the page described by Mode ("verify", "setup" or "codes") is to be shown with Status. When a
// step finished a sign-in the backend already set the session cookies; the frontend relays them.
type SecondStep struct {
	Redirect string   `json:"redirect,omitempty"`
	Status   int      `json:"status,omitempty"` // HTTP status of the page; 200 when zero
	Mode     string   `json:"mode,omitempty"`
	Error    string   `json:"error,omitempty"`
	QR       string   `json:"qr,omitempty"` // SVG made by the backend
	Secret   string   `json:"secret,omitempty"`
	Codes    []string `json:"codes,omitempty"` // recovery codes, shown once
	Continue string   `json:"continue,omitempty"`
}

// LoginSecondStepPage asks what the verification page shows. The sealed state cookie comes from
// the visitor's request.
func (c *Client) LoginSecondStepPage(ctx context.Context) (SecondStep, error) {
	var o SecondStep
	err := c.Get(ctx, "/pages/login/2fa", &o)
	return o, err
}

// LoginSecondStep checks the code of a sign-in.
func (c *Client) LoginSecondStep(ctx context.Context, code string) (SecondStep, error) {
	var o SecondStep
	err := c.Post(ctx, "/auth/2fa", map[string]string{"code": code}, &o)
	return o, err
}

// LoginSetupPage asks what the required setup page shows.
func (c *Client) LoginSetupPage(ctx context.Context) (SecondStep, error) {
	var o SecondStep
	err := c.Get(ctx, "/pages/login/2fa/setup", &o)
	return o, err
}

// LoginSetup confirms the required setup of two-step verification with a first code.
func (c *Client) LoginSetup(ctx context.Context, code string) (SecondStep, error) {
	var o SecondStep
	err := c.Post(ctx, "/auth/2fa/setup", map[string]string{"code": code}, &o)
	return o, err
}

// TwoFactorChange is the answer to a change of two-step verification on the settings page. A
// refusal that needs the setup shown again (a wrong first code) is Error with Status and Setup;
// other refusals are errors. Codes are the recovery codes, shown once.
type TwoFactorChange struct {
	Error  string          `json:"error,omitempty"`
	Status int             `json:"status,omitempty"`
	Setup  *TwoFactorSetup `json:"setup,omitempty"`
	Codes  []string        `json:"codes,omitempty"`
}

func twoFactorPath(userID int64, tail string) string { return userPath(userID, "/2fa/"+tail) }

// TwoFactorBegin starts the setup and returns the secret to show. current is the password.
func (c *Client) TwoFactorBegin(ctx context.Context, userID int64, current string) (TwoFactorChange, error) {
	var o TwoFactorChange
	err := c.Post(ctx, twoFactorPath(userID, "begin"), map[string]string{"current": current}, &o)
	return o, err
}

// TwoFactorConfirm checks the first code and turns two-step verification on.
func (c *Client) TwoFactorConfirm(ctx context.Context, userID int64, code string) (TwoFactorChange, error) {
	var o TwoFactorChange
	err := c.Post(ctx, twoFactorPath(userID, "confirm"), map[string]string{"code": code}, &o)
	return o, err
}

// TwoFactorDisable turns two-step verification off.
func (c *Client) TwoFactorDisable(ctx context.Context, userID int64, current, code string) error {
	return c.Post(ctx, twoFactorPath(userID, "disable"), map[string]string{"current": current, "code": code}, nil)
}

// TwoFactorRecovery replaces the recovery codes and returns the new ones.
func (c *Client) TwoFactorRecovery(ctx context.Context, userID int64, current, code string) (TwoFactorChange, error) {
	var o TwoFactorChange
	err := c.Post(ctx, twoFactorPath(userID, "recovery"), map[string]string{"current": current, "code": code}, &o)
	return o, err
}

// AdminStepUp reports whether the signed-in administrator has to enter a code before the Admin
// pages. It fails with CodeUnauthorized or CodeForbidden for anyone who is not an administrator.
func (c *Client) AdminStepUp(ctx context.Context) (bool, error) {
	var out struct {
		Needed bool `json:"needed"`
	}
	err := c.Get(ctx, "/admin/step-up", &out)
	return out.Needed, err
}

// AdminVerify checks a code for the Admin pages. On success the proof cookie is relayed and the
// answer redirects to /admin.
func (c *Client) AdminVerify(ctx context.Context, code string) (SecondStep, error) {
	var o SecondStep
	err := c.Post(ctx, "/admin/step-up", map[string]string{"code": code}, &o)
	return o, err
}
