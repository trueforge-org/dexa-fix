package workerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

// Client is what a worker uses to talk to the backend. It is safe for concurrent use.
type Client struct {
	base   *url.URL
	hc     *http.Client
	secret string
}

// New returns a client for the backend at base (for example http://api:8081). hc may be nil; build
// one with apiclient.HTTPClient for mutual TLS.
func New(base string, hc *http.Client, secret string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("backend URL %q: use the form http://host:port", base)
	}
	if hc == nil {
		hc = apiclient.DefaultHTTPClient()
	}
	return &Client{base: u, hc: hc, secret: secret}, nil
}

// do sends one call. A 409 is ErrLeaseLost, other 4xx are *apiclient.Error, and a failure to reach
// the backend or a 5xx is apiclient.ErrUnavailable, which a worker answers by waiting and trying again.
func (c *Client) do(ctx context.Context, method, path, lease string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	u := *c.base
	u.Path = Prefix + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(HeaderWorker, c.secret)
	if lease != "" {
		req.Header.Set(HeaderLease, lease)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, fmt.Errorf("%w: %v", apiclient.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
		return resp.StatusCode, ErrLeaseLost
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		e := &apiclient.Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		e.Status = resp.StatusCode
		if resp.StatusCode >= 500 {
			return resp.StatusCode, fmt.Errorf("%w: %s", apiclient.ErrUnavailable, e.Message)
		}
		return resp.StatusCode, e
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("backend answer: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Health asks the backend whether it is up.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/healthz", "", nil, nil)
	return err
}

// Register introduces the worker and returns the configuration.
func (c *Client) Register(ctx context.Context, r RegisterRequest) (Config, error) {
	var cfg Config
	_, err := c.do(ctx, http.MethodPost, "/register", "", r, &cfg)
	return cfg, err
}

// Config returns the current configuration.
func (c *Client) Config(ctx context.Context) (Config, error) {
	var cfg Config
	_, err := c.do(ctx, http.MethodGet, "/config", "", nil, &cfg)
	return cfg, err
}

// Lease asks for a job and waits for one for up to r.WaitSeconds. It returns nil when there is none.
func (c *Client) Lease(ctx context.Context, r LeaseRequest) (*Lease, error) {
	var l Lease
	status, err := c.do(ctx, http.MethodPost, "/lease", "", r, &l)
	if err != nil || status == http.StatusNoContent {
		return nil, err
	}
	return &l, nil
}

func jobPath(id, action string) string { return "/jobs/" + url.PathEscape(id) + "/" + action }

// Heartbeat renews the lease. It returns ErrLeaseLost when the job is no longer the worker's.
func (c *Client) Heartbeat(ctx context.Context, id, token string, progress json.RawMessage) (cancel bool, err error) {
	var a HeartbeatAnswer
	_, err = c.do(ctx, http.MethodPost, jobPath(id, "heartbeat"), token, Heartbeat{Progress: progress}, &a)
	return a.Cancel, err
}

// SaveCredentials hands the backend credentials that were renewed during the job.
func (c *Client) SaveCredentials(ctx context.Context, id, token string, data json.RawMessage) error {
	_, err := c.do(ctx, http.MethodPost, jobPath(id, "credentials"), token, Credentials{Data: data}, nil)
	return err
}

// Complete reports how the attempt ended.
func (c *Client) Complete(ctx context.Context, id, token string, comp Completion) error {
	_, err := c.do(ctx, http.MethodPost, jobPath(id, "complete"), token, comp, nil)
	return err
}

// Release gives the job back without counting the attempt, for a worker that is shutting down.
func (c *Client) Release(ctx context.Context, id, token string) error {
	_, err := c.do(ctx, http.MethodPost, jobPath(id, "release"), token, nil, nil)
	return err
}
