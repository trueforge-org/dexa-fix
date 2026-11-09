package bodyapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
)

// maxAnswer bounds what a client reads from one answer.
const maxAnswer = 64 << 20

// Client calls the body service on behalf of the main backend. It makes a token for every call.
type Client struct {
	base   *url.URL
	hc     *http.Client
	secret string
	signer *Signer
	now    func() time.Time
}

// New returns a Client for the service at base. hc carries the transport (TLS, mutual TLS, or the
// in-process transport of apiclient); nil means http.DefaultClient.
func New(base string, hc *http.Client, secret string, signer *Signer) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("body service URL %q is not a valid address", base)
	}
	if secret == "" || signer == nil {
		return nil, errors.New("the body client needs a secret and a signing key")
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{base: u, hc: hc, secret: secret, signer: signer, now: time.Now}, nil
}

// call makes one request. in is sent as JSON when not nil; a successful answer is decoded into out
// when that is not nil. It returns the response so a caller can read headers; the body is closed.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, subject int64, scope Scope, params *EstimateParams, header http.Header, in, out any) (*http.Response, error) {
	u := *c.base
	u.Path = c.base.Path + Prefix + path
	u.RawQuery = q.Encode()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set(HeaderSecret, c.secret)
	req.Header.Set(HeaderToken, c.signer.Sign(subject, scope, params, c.now()))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: CodeUnavailable, Message: "the body service cannot be reached: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, &Error{Status: http.StatusServiceUnavailable, Code: CodeUnavailable, Message: "the body service answer was cut off: " + err.Error()}
	}
	if resp.StatusCode == http.StatusNotModified {
		return resp, nil
	}
	if resp.StatusCode >= 300 {
		e := &Error{}
		if json.Unmarshal(raw, e) != nil || e.Code == "" {
			e = &Error{Code: CodeInternal, Message: "unexpected answer from the body service"}
			if resp.StatusCode >= 500 {
				e.Code = CodeUnavailable
			}
		}
		e.Status = resp.StatusCode
		if resp.StatusCode >= 500 && e.Code == CodeInternal {
			e.Code = CodeUnavailable // the caller cannot tell a crashed service from a stopped one
		}
		return resp, e
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp, fmt.Errorf("body service answer: %w", err)
		}
	}
	return resp, nil
}

func userPath(id int64, rest string) string { return "/users/" + strconv.FormatInt(id, 10) + rest }

func (p Page) values() url.Values {
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if p.Before != "" {
		q.Set("before", p.Before)
	}
	if !p.Since.IsZero() {
		q.Set("since", p.Since.UTC().Format(time.RFC3339Nano))
	}
	if !p.Until.IsZero() {
		q.Set("until", p.Until.UTC().Format(time.RFC3339Nano))
	}
	return q
}

// pageAnswer is the answer to a list call.
type pageAnswer[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next,omitempty"`
}

// Scans lists a user's DEXA scans, newest first. next is empty on the last page.
func (c *Client) Scans(ctx context.Context, u Subject, p Page) (scans []models.DexaScan, next string, err error) {
	var a pageAnswer[models.DexaScan]
	_, err = c.call(ctx, http.MethodGet, userPath(u.ID, "/scans"), p.values(), u.ID, ScopeRead, nil, nil, nil, &a)
	return a.Items, a.Next, err
}

// CreateScan stores a scan for the user; the scan's ID is set.
func (c *Client) CreateScan(ctx context.Context, u Subject, s *models.DexaScan) error {
	var out models.DexaScan
	if _, err := c.call(ctx, http.MethodPost, userPath(u.ID, "/scans"), nil, u.ID, ScopeWrite, nil, nil, s, &out); err != nil {
		return err
	}
	*s = out
	return nil
}

// DeleteScan removes one of the user's scans.
func (c *Client) DeleteScan(ctx context.Context, u Subject, id int64) error {
	_, err := c.call(ctx, http.MethodDelete, userPath(u.ID, "/scans/"+strconv.FormatInt(id, 10)), nil, u.ID, ScopeWrite, nil, nil, nil, nil)
	return err
}

// Readings lists a user's scale readings, newest first. next is empty on the last page.
func (c *Client) Readings(ctx context.Context, u Subject, p Page) (readings []models.ScaleReading, next string, err error) {
	var a pageAnswer[models.ScaleReading]
	_, err = c.call(ctx, http.MethodGet, userPath(u.ID, "/readings"), p.values(), u.ID, ScopeRead, nil, nil, nil, &a)
	return a.Items, a.Next, err
}

// UpsertReadings stores readings; one with an external id replaces the stored one of the same
// source and id. It returns how many were stored.
func (c *Client) UpsertReadings(ctx context.Context, u Subject, rs []models.ScaleReading) (int, error) {
	var out struct {
		Upserted int `json:"upserted"`
	}
	_, err := c.call(ctx, http.MethodPost, userPath(u.ID, "/readings"), nil, u.ID, ScopeWrite, nil, nil, rs, &out)
	return out.Upserted, err
}

// DeleteReading removes one of the user's readings.
func (c *Client) DeleteReading(ctx context.Context, u Subject, id int64) error {
	_, err := c.call(ctx, http.MethodDelete, userPath(u.ID, "/readings/"+strconv.FormatInt(id, 10)), nil, u.ID, ScopeWrite, nil, nil, nil, nil)
	return err
}

// Reconcile stores what a source returned for the user. The body service decides what to write.
func (c *Client) Reconcile(ctx context.Context, u Subject, req ReconcileRequest) (ImportResult, error) {
	var out ImportResult
	_, err := c.call(ctx, http.MethodPost, userPath(u.ID, "/readings/reconcile"), nil, u.ID, ScopeWrite, &u.Params, nil, req, &out)
	return out, err
}

// EstimatesReply is the answer to Estimates.
type EstimatesReply struct {
	Result      estimate.Result
	ETag        string
	NotModified bool // the caller's copy, named by ifNoneMatch, is still good; Result is empty
}

// Estimates returns the user's estimates. ifNoneMatch is the ETag of a copy the caller already has,
// or empty.
func (c *Client) Estimates(ctx context.Context, u Subject, ifNoneMatch string) (EstimatesReply, error) {
	h := http.Header{}
	if ifNoneMatch != "" {
		h.Set("If-None-Match", ifNoneMatch)
	}
	var r EstimatesReply
	resp, err := c.call(ctx, http.MethodGet, userPath(u.ID, "/estimates"), nil, u.ID, ScopeRead, &u.Params, h, nil, &r.Result)
	if err != nil {
		return EstimatesReply{}, err
	}
	r.ETag = resp.Header.Get("ETag")
	r.NotModified = resp.StatusCode == http.StatusNotModified
	return r, nil
}

// Export returns all of the user's scans and readings.
func (c *Client) Export(ctx context.Context, u Subject) (Export, error) {
	var e Export
	_, err := c.call(ctx, http.MethodGet, userPath(u.ID, "/export"), nil, u.ID, ScopeRead, nil, nil, nil, &e)
	return e, err
}

// DeleteUser removes everything the body service holds for a user. It is not an error when there
// is nothing.
func (c *Client) DeleteUser(ctx context.Context, userID int64) error {
	_, err := c.call(ctx, http.MethodDelete, userPath(userID, ""), nil, userID, ScopeAdmin, nil, nil, nil, nil)
	return err
}

// Stats counts the scans and readings.
func (c *Client) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	_, err := c.call(ctx, http.MethodGet, "/stats", nil, 0, ScopeAdmin, nil, nil, nil, &s)
	return s, err
}

// UserIDs lists the users that have data, in id order.
func (c *Client) UserIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	_, err := c.call(ctx, http.MethodGet, "/users", nil, 0, ScopeAdmin, nil, nil, nil, &ids)
	return ids, err
}

// HTTPClient is the client for a body service in another process, with a TLS configuration such as
// the mutual TLS configuration of package mtls (nil means plain HTTP). An import of a long history
// takes a while to answer, so the wait for a response is generous; connecting is not.
func HTTPClient(tlsConfig *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		MaxConnsPerHost:       64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
	}}
}
