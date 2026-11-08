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
		// The page after signing in is the user's own profile.
		regexp.MustCompile(`["']otherUserViewModel["']\s*:\s*\{\s*["']id["']\s*:\s*["'](` + uuid + `)["']`),
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

// Login signs in and reads the CSRF token and user id from the page Strength Level lands on.
func (c *Client) Login(ctx context.Context, email, password string) error {
	page, path, err := c.get(ctx, "/login")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/login") {
		// Strength Level forwards a signed-in browser away from the sign-in page.
		return c.readSession(page)
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
	landed, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 || strings.HasPrefix(resp.Request.URL.Path, "/login") {
		return ErrLogin
	}

	return c.readSession(string(landed))
}

// readSession reads the CSRF token and the user id from a page of the signed-in app.
func (c *Client) readSession(page string) error {
	if c.csrf = firstMatch(metaCSRF, page); c.csrf == "" {
		return fmt.Errorf("%w: no token after signing in", ErrPage)
	}
	for _, re := range userIDRes {
		if c.userID = firstMatch(re, page); c.userID != "" {
			return nil
		}
	}
	return fmt.Errorf("%w: no user id after signing in", ErrPage)
}

// Day is an entry that already exists on Strength Level.
type Day struct {
	ID         string
	Date       string // YYYY-MM-DD
	Weight     float64
	BodyFat    *float64
	FatMethod  string
	Timezone   string
	OffsetMins int
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
			Method     *string  `json:"bodyfat_method"`
			Timezone   string   `json:"timezone"`
			Offset     int      `json:"timezone_offset_mins"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPage, err)
	}
	days := make(map[string]Day, len(out.Data))
	for _, d := range out.Data {
		day := Day{ID: d.ID, Date: d.Date, Weight: d.Bodyweight, BodyFat: d.Bodyfat, Timezone: d.Timezone, OffsetMins: d.Offset}
		if d.Method != nil {
			day.FatMethod = *d.Method
		}
		days[d.Date] = day
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
	return c.write(ctx, http.MethodPost, "/api/bodyweights", e.Day, payload)
}

// Update changes the existing entry of a day the way the web app's edit form does. The entry keeps
// its time zone, and keeps its body fat when e has none.
func (c *Client) Update(ctx context.Context, old Day, e strengthlevel.Entry) error {
	payload := map[string]any{
		"user_id": c.userID, "date": old.Date, "bodyweight": round1(e.WeightKg),
		"timezone": old.Timezone, "timezone_offset_mins": old.OffsetMins,
		"bodyfat": nil, "bodyfat_method": nil,
	}
	if old.Timezone == "" {
		payload["timezone"], payload["timezone_offset_mins"] = "UTC", 0
	}
	method := old.FatMethod
	if method == "" {
		method = "scales"
	}
	switch {
	case e.FatPct != nil:
		payload["bodyfat"], payload["bodyfat_method"] = round1(*e.FatPct), method
	case old.BodyFat != nil:
		payload["bodyfat"], payload["bodyfat_method"] = *old.BodyFat, method
	}
	return c.write(ctx, http.MethodPut, "/api/bodyweights/"+url.PathEscape(old.ID), e.Day, payload)
}

// Outcome is what Upsert did.
type Outcome string

const (
	Added     Outcome = "added"
	Updated   Outcome = "updated"
	Unchanged Outcome = "unchanged"
)

// Upsert adds the day, or updates it when it exists with other values.
func (c *Client) Upsert(ctx context.Context, e strengthlevel.Entry) (Outcome, error) {
	remote, err := c.List(ctx)
	if err != nil {
		return "", err
	}
	return c.apply(ctx, remote, e, true)
}

// apply adds e, or when it exists and update is set, updates it if the values differ.
func (c *Client) apply(ctx context.Context, remote map[string]Day, e strengthlevel.Entry, update bool) (Outcome, error) {
	old, ok := remote[e.Day]
	switch {
	case !ok:
		return Added, c.Add(ctx, e)
	case !update || same(old, e):
		return Unchanged, nil
	}
	return Updated, c.Update(ctx, old, e)
}

// same reports whether the entry on Strength Level already has the values of e, to one decimal.
// An e without body fat leaves the body fat there alone.
func same(old Day, e strengthlevel.Entry) bool {
	if round1(old.Weight) != round1(e.WeightKg) {
		return false
	}
	return e.FatPct == nil || (old.BodyFat != nil && round1(*old.BodyFat) == round1(*e.FatPct))
}

// write sends a JSON request that creates or changes an entry.
func (c *Client) write(ctx context.Context, method, path, day string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(raw))
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
		return fmt.Errorf("strengthlevel: %s %s: %s %s", method, day, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// Result reports what a sync did.
type Result struct {
	Added, Updated, Skipped int
	Failed                  []string // one message per day that could not be written
}

// Options tune a sync.
type Options struct {
	// Update also changes days that exist on Strength Level with other values. Without it they are
	// left as they are.
	Update bool
	// Progress is called after each day of the period with the number done and the total.
	Progress func(done, total int)
}

// Sync writes the entries of local from the given day (YYYY-MM-DD, "" for all) on, oldest first.
// Days missing on Strength Level are added. Days that exist are left alone, or updated when
// opt.Update is set. Nothing is ever deleted. It stops at the first error that is not about a
// single day, such as a canceled context.
func (c *Client) Sync(ctx context.Context, local []strengthlevel.Entry, since string, opt Options) (Result, error) {
	var res Result
	remote, err := c.List(ctx)
	if err != nil {
		return res, err
	}
	var todo []strengthlevel.Entry
	for i := len(local) - 1; i >= 0; i-- { // oldest first
		if local[i].Day >= since {
			todo = append(todo, local[i])
		}
	}
	for i, e := range todo {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		out, err := c.apply(ctx, remote, e, opt.Update)
		switch {
		case err != nil:
			res.Failed = append(res.Failed, err.Error())
		case out == Added:
			res.Added++
		case out == Updated:
			res.Updated++
		default:
			res.Skipped++
		}
		if out == Added || out == Updated {
			time.Sleep(c.Delay)
		}
		if opt.Progress != nil {
			opt.Progress(i+1, len(todo))
		}
	}
	return res, nil
}

func (c *Client) get(ctx context.Context, path string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/html")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("strengthlevel: GET %s: %s", strings.SplitN(path, "?", 2)[0], resp.Status)
	}
	return string(b), resp.Request.URL.Path, nil
}

func firstMatch(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
