package apiclient

import (
	"context"
	"net/url"
)

// ProviderStart begins a sign-in with an external provider, or (link set, for a signed-in visitor)
// linking it to the account, which needs the current password. The outcome's Redirect is the
// provider's address; the sealed state cookie travels in the relayed Set-Cookie headers.
func (c *Client) ProviderStart(ctx context.Context, slug string, link bool, current string) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.Post(ctx, "/auth/providers/"+url.PathEscape(slug)+"/start", map[string]any{"link": link, "current": current}, &o)
	return o, err
}

// ProviderCallback finishes a sign-in or a linking with every query and form parameter the provider
// sent back (Apple answers with a cross-site form post).
func (c *Client) ProviderCallback(ctx context.Context, slug string, params map[string]string) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.Post(ctx, "/auth/providers/"+url.PathEscape(slug)+"/callback", params, &o)
	return o, err
}
