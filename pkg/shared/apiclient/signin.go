package apiclient

import "context"

// LoginProvider is a way of signing in through an external provider.
type LoginProvider struct{ Slug, Label string }

// LoginPage is what the sign-in page shows.
type LoginPage struct {
	PasswordEnabled bool
	SignupOpen      bool
	FirstUser       bool
	Providers       []LoginProvider
	CanReset        bool // people can ask for a reset e-mail
	Passkeys        bool // the browser can use passkeys here
}

// SignInOutcome is the answer to a step of signing in. Exactly one of Redirect and Error is set.
// When the step succeeded the backend already set the session (or second-step) cookies, and the
// frontend relays them with the redirect.
type SignInOutcome struct {
	Redirect string `json:"redirect,omitempty"`
	Error    string `json:"error,omitempty"`  // fit to show on the sign-in page
	Status   int    `json:"status,omitempty"` // HTTP status to show it with
}

// LoginPage returns the data of the sign-in page.
func (c *Client) LoginPage(ctx context.Context) (LoginPage, error) {
	var p LoginPage
	err := c.Get(ctx, "/pages/login", &p)
	return p, err
}

// Login checks an e-mail address and password.
func (c *Client) Login(ctx context.Context, email, password string) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.Post(ctx, "/auth/password", map[string]string{"email": email, "password": password}, &o)
	return o, err
}

// Signup creates an account with a password.
func (c *Client) Signup(ctx context.Context, username, email, password, confirm string) (SignInOutcome, error) {
	var o SignInOutcome
	err := c.Post(ctx, "/auth/signup", map[string]string{"username": username, "email": email, "password": password, "confirm": confirm}, &o)
	return o, err
}
