// Package apiclient is how the frontend process talks to the backend process. The backend owns the
// database, the keys and every security decision; the frontend renders pages from what this client
// returns. The package must not import the store or the web package.
//
// The wire protocol is the backend's internal API under /internal/v1. It is not a public API and
// may change between versions: deploy the frontend and the backend from the same release.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Header names of the internal API.
const (
	// HeaderService carries the shared secret that proves the caller is the frontend.
	HeaderService = "X-Dexafix-Service"
	// HeaderClientIP, HeaderSecure and HeaderHost describe the visitor's own request, after the
	// frontend settled the proxy headers. The backend believes them only after the service secret
	// checked out.
	HeaderClientIP = "X-Dexafix-Client-IP"
	HeaderSecure   = "X-Dexafix-Secure"
	HeaderHost     = "X-Dexafix-Host"
	// HeaderCookie carries the visitor's Cookie header. The backend reads its session and sealed
	// state cookies from it and answers with Set-Cookie headers, which the frontend relays. The
	// sealing keys stay in the backend: the frontend only stores and returns the opaque values.
	HeaderCookie = "X-Dexafix-Cookie"
)

// Prefix of every internal API path.
const Prefix = "/internal/v1"

// Error is an error answered by the backend. Message is fit to show to the person.
type Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("backend answered %d", e.Status)
}

// Codes in Error.Code.
const (
	CodeNotFound     = "not_found"
	CodeInvalid      = "invalid"
	CodeExists       = "exists"
	CodeThrottled    = "throttled"
	CodeForbidden    = "forbidden"
	CodeUnauthorized = "unauthorized"
	CodeReauth       = "reauth_required"
	CodeStepUp       = "step_up_required"
	CodeUnavailable  = "unavailable"
	CodeInternal     = "internal"
)

// ErrUnavailable is returned when the backend cannot be reached or answers with a server error.
// Pages render a "temporarily unavailable" notice for it.
var ErrUnavailable = errors.New("the backend is not available")

// IsCode reports whether err is a backend error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// StatusOf returns the HTTP status of a backend error, or 0 for any other error.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Client calls the backend. It is safe for concurrent use.
type Client struct {
	base   *url.URL
	hc     *http.Client
	secret string
}

// New returns a client for the backend at base (for example http://api:8081). hc may be nil.
func New(base string, hc *http.Client, secret string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend URL %q: use the form http://host:port", base)
	}
	if hc == nil {
		hc = DefaultHTTPClient()
	}
	return &Client{base: u, hc: hc, secret: secret}, nil
}

// DefaultHTTPClient is a client with timeouts and connection limits suited to the internal link.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxConnsPerHost:       128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
	}}
}

// Request describes the visitor's request for one call. Build it with FromRequest.
type Request struct {
	Cookie   string
	ClientIP string
	Host     string
	Secure   bool

	// Relay receives the Set-Cookie headers the backend answers with. May be nil.
	Relay http.ResponseWriter
}

type ctxKey struct{}

// WithRequest attaches the visitor's request to ctx. Calls made with the returned context carry
// the visitor's cookies, address and host, and relay the backend's Set-Cookie headers to w.
func WithRequest(ctx context.Context, rq Request) context.Context {
	return context.WithValue(ctx, ctxKey{}, rq)
}

func requestOf(ctx context.Context) Request {
	rq, _ := ctx.Value(ctxKey{}).(Request)
	return rq
}

// Result is a successful answer.
type Result struct {
	Status int
	Header http.Header
}

// Do sends in (JSON-encoded unless nil) and decodes the answer into out (unless nil). An error
// status from the backend is returned as *Error; a failure to reach it as ErrUnavailable.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) (*Result, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.Raw(ctx, method, path, body, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errorFrom(resp)
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return nil, fmt.Errorf("backend answer: %w", err)
		}
	}
	return &Result{Status: resp.StatusCode, Header: resp.Header}, nil
}

// Health asks the backend whether it is up.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.Raw(ctx, http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: health check answered %d", ErrUnavailable, resp.StatusCode)
	}
	return nil
}

// Get is Do with GET and no body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	_, err := c.Do(ctx, http.MethodGet, path, nil, out)
	return err
}

// Post is Do with POST.
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	_, err := c.Do(ctx, http.MethodPost, path, in, out)
	return err
}

// Delete is Do with DELETE and no body.
func (c *Client) Delete(ctx context.Context, path string) error {
	_, err := c.Do(ctx, http.MethodDelete, path, nil, nil)
	return err
}

// Raw sends a request and returns the response unread, for streamed bodies such as exports. Status
// codes of 400 and above are not turned into errors; the caller owns resp.Body. Set-Cookie headers
// are relayed.
func (c *Client) Raw(ctx context.Context, method, path string, body io.Reader, header http.Header) (*http.Response, error) {
	u := *c.base
	if i := strings.IndexByte(path, '?'); i >= 0 {
		u.Path, u.RawQuery = Prefix+path[:i], path[i+1:]
	} else {
		u.Path = Prefix + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(HeaderService, c.secret)
	rq := requestOf(ctx)
	if rq.Cookie != "" {
		req.Header.Set(HeaderCookie, rq.Cookie)
	}
	if rq.ClientIP != "" {
		req.Header.Set(HeaderClientIP, rq.ClientIP)
	}
	if rq.Host != "" {
		req.Header.Set(HeaderHost, rq.Host)
	}
	if rq.Secure {
		req.Header.Set(HeaderSecure, "1")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if rq.Relay != nil {
		for _, sc := range resp.Header.Values("Set-Cookie") {
			rq.Relay.Header().Add("Set-Cookie", sc)
		}
	}
	return resp, nil
}

func errorFrom(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	e := &Error{Status: resp.StatusCode}
	if json.Unmarshal(b, e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
	}
	if e.Status == 0 {
		e.Status = resp.StatusCode
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: %s", ErrUnavailable, e.Message)
	}
	return e
}
