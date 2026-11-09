package apiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// The WebAuthn ceremony state travels in a sealed cookie the backend sets and reads (relayed like
// any other). The bodies of the finish steps come from the browser's script and are forwarded
// unchanged: the backend parses them with the WebAuthn library.

func passkeyPath(userID int64, tail string) string { return userPath(userID, "/passkeys"+tail) }

// postBody sends body unchanged as JSON and decodes the answer into out.
func (c *Client) postBody(ctx context.Context, path string, body io.Reader, out any) error {
	resp, err := c.Raw(ctx, http.MethodPost, path, body, http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errorFrom(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("backend answer: %w", err)
		}
	}
	return nil
}

// PasskeyLoginBegin starts a passkey sign-in and returns the WebAuthn options for the browser.
func (c *Client) PasskeyLoginBegin(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.Post(ctx, "/auth/passkey/begin", nil, &out)
	return out, err
}

// PasskeyLoginFinish forwards the browser's assertion unchanged. The session cookies are relayed.
func (c *Client) PasskeyLoginFinish(ctx context.Context, body io.Reader) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.postBody(ctx, "/auth/passkey/finish", body, &o)
	return o, err
}

// PasskeyRegisterBegin starts adding a passkey and returns the WebAuthn options. current is the
// password, when the account has one.
func (c *Client) PasskeyRegisterBegin(ctx context.Context, userID int64, name, current string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.Post(ctx, passkeyPath(userID, "/begin"), map[string]string{"name": name, "current": current}, &out)
	return out, err
}

// PasskeyRegisterFinish forwards the browser's attestation unchanged and returns where to go.
func (c *Client) PasskeyRegisterFinish(ctx context.Context, userID int64, body io.Reader) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.postBody(ctx, passkeyPath(userID, "/finish"), body, &o)
	return o, err
}

// RenamePasskey renames one of the user's passkeys.
func (c *Client) RenamePasskey(ctx context.Context, userID int64, cred, name string) error {
	return c.Post(ctx, passkeyPath(userID, "/"+url.PathEscape(cred)+"/rename"), map[string]string{"name": name}, nil)
}

// DeletePasskey removes a passkey. The answer is the message to show, whether it was removed or not.
func (c *Client) DeletePasskey(ctx context.Context, userID int64, cred, current string) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	_, err := c.Do(ctx, http.MethodDelete, passkeyPath(userID, "/"+url.PathEscape(cred)), map[string]string{"current": current}, &out)
	return out.Message, err
}
