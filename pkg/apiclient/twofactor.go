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
