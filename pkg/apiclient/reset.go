package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// HeaderResetToken carries a password reset token. It travels in a header, never in a URL, so it
// does not end up in access logs.
const HeaderResetToken = "X-Dexafix-Reset-Token"

// ResetStep is what a reset page shows, or where to go on.
type ResetStep struct {
	Redirect  string `json:"redirect,omitempty"`
	Mode      string `json:"mode,omitempty"` // "forgot", "sent", "form" or "invalid"
	Error     string `json:"error,omitempty"`
	NeedsCode bool   `json:"needs_code,omitempty"` // the account has two-step verification
	Status    int    `json:"status,omitempty"`     // HTTP status of the page; 200 when zero
}

// ResetForgotPage reports whether people can ask for a reset e-mail (else it is a not found error).
func (c *Client) ResetForgotPage(ctx context.Context) (ResetStep, error) {
	var o ResetStep
	err := c.Get(ctx, "/pages/password/forgot", &o)
	return o, err
}

// ResetForgot asks for a reset e-mail. It answers the same whoever the address belongs to.
func (c *Client) ResetForgot(ctx context.Context, email string) (ResetStep, error) {
	var o ResetStep
	err := c.Post(ctx, "/auth/password/forgot", map[string]string{"email": email}, &o)
	return o, err
}

// ResetFormPage checks a reset token and tells which form to show.
func (c *Client) ResetFormPage(ctx context.Context, token string) (ResetStep, error) {
	return c.withToken(ctx, http.MethodGet, "/pages/password/reset", token, nil)
}

// Reset sets a new password with a reset token (and a code when the account has two-step
// verification).
func (c *Client) Reset(ctx context.Context, token, password, confirm, code string) (ResetStep, error) {
	return c.withToken(ctx, http.MethodPost, "/auth/password/reset", token,
		map[string]string{"password": password, "confirm": confirm, "code": code})
}

func (c *Client) withToken(ctx context.Context, method, path, token string, in any) (ResetStep, error) {
	var o ResetStep
	var body *bytes.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return o, err
		}
		body = bytes.NewReader(b)
	}
	hdr := http.Header{HeaderResetToken: {token}}
	var resp *http.Response
	var err error
	if body != nil {
		resp, err = c.Raw(ctx, method, path, body, hdr)
	} else {
		resp, err = c.Raw(ctx, method, path, nil, hdr)
	}
	if err != nil {
		return o, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return o, errorFrom(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return o, fmt.Errorf("backend answer: %w", err)
	}
	return o, nil
}
