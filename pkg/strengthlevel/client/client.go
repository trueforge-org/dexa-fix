// Package client talks to my.strengthlevel.com the way its own web app does: it signs in with an
// e-mail address and password and uses the app's JSON bodyweight endpoints. Strength Level
// publishes no API; use is by agreement with its developer, and only for the user's own account.
//
// The client only adds missing days. It never changes or deletes an entry on Strength Level.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/strengthlevel"
)

// DefaultBase is the Strength Level web app.
const DefaultBase = "https://my.strengthlevel.com"

const (
	maxBody   = 16 << 20
	userAgent = "dexa-fix"
)

var (
	// ErrLogin means Strength Level did not accept the e-mail address and password.
	ErrLogin = errors.New("strengthlevel: sign-in was refused, check the e-mail address and password")
	// ErrPage means a page did not have the shape this client expects, so Strength Level probably changed.
	ErrPage = errors.New("strengthlevel: unexpected page, the site may have changed")
)

var (
	metaCSRF  = regexp.MustCompile(`<meta[^>]+name=["']csrf-token["'][^>]+content=["']([^"']+)["']`)
	inputCSRF = regexp.MustCompile(`<input[^>]+name=["']csrf_token["'][^>]+value=["']([^"']+)["']`)
	uuid      = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	userIDRes = []*regexp.Regexp{
		regexp.MustCompile(`["']?user[_-]?id["']?\s*[:=]\s*["'](` + uuid + `)["']`),
		regexp.MustCompile(`data-user-id=["'](` + uuid + `)["']`),
	}
)

// Client is one signed-in session. It is not safe for concurrent use.
type Client struct {
	base   string
	http   *http.Client
	csrf   string
	userID string
	// Delay is the pause between writes.
	Delay time.Duration
}

// New returns a client for the given base URL ("" means DefaultBase). A nil httpClient gets a
// default one; its cookie jar is replaced so that every Client has its own session.
func New(base string, httpClient *http.Client) *Client {
	if base == "" {
		base = DefaultBase
	}
	hc := http.Client{Timeout: 30 * time.Second}
	if httpClient != nil {
		hc = *httpClient
	}
	hc.Jar, _ = cookiejar.New(nil)
	return &Client{base: strings.TrimRight(base, "/"), http: &hc, Delay: 300 * time.Millisecond}
}

// Login signs in and then reads the CSRF token and user id from the user's bodyweight page.
// username is the profile name in the page address (my.strengthlevel.com/<username>/bodyweight).
func (c *Client) Login(ctx context.Context, username, email, password string) error {
	page, _, err := c.get(ctx, "/login")
	if err != nil {
		return err
	}
	token := firstMatch(inputCSRF, page)
	if token == "" {
		token = firstMatch(metaCSRF, page)
	}
	if token == "" {
		return fmt.Errorf("%w: no token on the sign-in page", ErrPage)
	}
	form := url.Values{"email": {email}, "password": {password}, "csrf_token": {token}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode >= 400 || strings.HasPrefix(resp.Request.URL.Path, "/login") {
		return ErrLogin
	}

	page, _, err = c.get(ctx, "/"+url.PathEscape(username)+"/bodyweight")
	if err != nil {
		return err
	}
	if c.csrf = firstMatch(metaCSRF, page); c.csrf == "" {
		return fmt.Errorf("%w: no token on the bodyweight page", ErrPage)
	}
	for _, re := range userIDRes {
		if c.userID = firstMatch(re, page); c.userID != "" {
			return nil
		}
	}
	return fmt.Errorf("%w: no user id on the bodyweight page", ErrPage)
}

// Day is an entry that already exists on Strength Level.
type Day struct {
	ID      string
	Date    string // YYYY-MM-DD
	Weight  float64
	BodyFat *float64
}

// List returns the existing entries by date.
func (c *Client) List(ctx context.Context) (map[string]Day, error) {
	q := url.Values{"user_id": {c.userID}, "include_count": {"false"}, "limit": {"9999"}}
	body, _, err := c.get(ctx, "/api/bodyweights?"+q.Encode())
	if err != nil {
		return nil, err
	}
	var out struct {
		Data []struct {
			ID         string   `json:"id"`
			Date       string   `json:"date"`
			Bodyweight float64  `json:"bodyweight"`
			Bodyfat    *float64 `json:"bodyfat"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPage, err)
	}
	days := make(map[string]Day, len(out.Data))
	for _, d := range out.Data {
		days[d.Date] = Day{ID: d.ID, Date: d.Date, Weight: d.Bodyweight, BodyFat: d.Bodyfat}
	}
	return days, nil
}

// Add creates the entry of one day. Body fat is left out when there is none.
func (c *Client) Add(ctx context.Context, e strengthlevel.Entry) error {
	payload := map[string]any{
		"user_id": c.userID, "date": e.Day, "bodyweight": round1(e.WeightKg),
		"timezone": "UTC", "timezone_offset_mins": 0,
	}
	if e.FatPct != nil {
		payload["bodyfat"] = round1(*e.FatPct)
		payload["bodyfat_method"] = "scales"
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/bodyweights", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("strengthlevel: add %s: %s %s", e.Day, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Result reports what a sync did.
type Result struct {
	Added, Skipped int
	Failed         []string // one message per day that could not be added
}

// Sync adds every entry of local from the given day (YYYY-MM-DD, "" for all) on that Strength Level
// has no entry for. Days that exist there are left as they are, even when their values differ.
// It stops at the first error that is not about a single day, such as a canceled context.
func (c *Client) Sync(ctx context.Context, local []strengthlevel.Entry, since string) (Result, error) {
	var res Result
	remote, err := c.List(ctx)
	if err != nil {
		return res, err
	}
	for i := len(local) - 1; i >= 0; i-- { // oldest first
		e := local[i]
		if e.Day < since {
			continue
		}
		if _, ok := remote[e.Day]; ok {
			res.Skipped++
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if err := c.Add(ctx, e); err != nil {
			res.Failed = append(res.Failed, err.Error())
			continue
		}
		res.Added++
		time.Sleep(c.Delay)
	}
	return res, nil
}

func (c *Client) get(ctx context.Context, path string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/html")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("strengthlevel: GET %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	return string(b), resp.StatusCode, nil
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
