package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBase  = "https://wbsapi.withings.net"
	defaultAuthBase = "https://account.withings.com"
	// measTypes: 1 weight, 5 fat-free mass, 6 fat ratio, 8 fat mass, 76 muscle mass,
	// 77 hydration, 88 bone mass, 170 visceral fat.
	measTypes = "1,5,6,8,76,77,88,170"
)

// Config holds the server-wide Withings OAuth application settings.
type Config struct {
	ClientID     string
	ClientSecret string
	// RedirectURL must match the callback URL registered with the Withings application.
	RedirectURL string
	// APIBase and AuthBase override the Withings endpoints (used by tests).
	APIBase  string
	AuthBase string
}

// tokens is what we keep per user in user_integrations.
type tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	UserID       string    `json:"withings_userid,omitempty"`
}

type envelope struct {
	Status int             `json:"status"`
	Error  string          `json:"error"`
	Body   json.RawMessage `json:"body"`
}

type tokenBody struct {
	UserID       json.Number `json:"userid"`
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    int64       `json:"expires_in"`
}

type measure struct {
	Value int64 `json:"value"`
	Type  int   `json:"type"`
	Unit  int   `json:"unit"`
}

type measureGroup struct {
	GrpID    int64     `json:"grpid"`
	Attrib   int       `json:"attrib"`
	Date     int64     `json:"date"`
	Category int       `json:"category"`
	Measures []measure `json:"measures"`
}

type measBody struct {
	Groups []measureGroup `json:"measuregrps"`
	More   int            `json:"more"`
	Offset int            `json:"offset"`
}

func (c Config) apiBase() string { return strings.TrimRight(orDefault(c.APIBase, defaultAPIBase), "/") }
func (c Config) authBase() string {
	return strings.TrimRight(orDefault(c.AuthBase, defaultAuthBase), "/")
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// authURL builds the URL the user is sent to for granting access.
func (c Config) authURL(state string) string {
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {c.ClientID},
		"redirect_uri":  {c.RedirectURL},
		"scope":         {"user.metrics"},
		"state":         {state},
	}
	return c.authBase() + "/oauth2_user/authorize2?" + q.Encode()
}

// Retry settings for read-only calls: a transport error, an HTTP 5xx or 429, or the Withings
// "too many requests" status is tried again after a growing pause. Tests shorten the pause.
var (
	maxAttempts  = 3
	backoffBase  = 500 * time.Millisecond
	errRetryable = errors.New("retryable")
)

// withingsTooManyRequests is the status Withings returns when an application sends too many
// requests; 100-series and 5xx statuses are temporary failures on their side as well.
const withingsTooManyRequests = 601

// retryBackoff returns the pause before retry number attempt+1: exponential, with jitter.
func retryBackoff(attempt int) time.Duration {
	d := backoffBase << attempt
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// callRetry is call for requests that are safe to repeat (reading measurements). Token requests
// are not: a refresh token is single use.
func (i *Importer) callRetry(ctx context.Context, path, bearer string, form url.Values, out any) error {
	var err error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err = i.call(ctx, path, bearer, form, out); err == nil || !errors.Is(err, errRetryable) {
			return err
		}
		if attempt == maxAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff(attempt)):
		}
	}
	return err
}

// call POSTs a form to the Withings API and decodes the "body" of a status-0 response into out.
func (i *Importer) call(ctx context.Context, path, bearer string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.config().apiBase()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := i.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("withings request: %w", err)
		}
		return fmt.Errorf("withings request: %w: %w", errRetryable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("withings response: %w", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("withings response (HTTP %d): %w", resp.StatusCode, errRetryable)
		}
		return fmt.Errorf("withings response (HTTP %d): not JSON", resp.StatusCode)
	}
	if env.Status != 0 {
		err := fmt.Errorf("withings api status %d: %s", env.Status, env.Error)
		if env.Status == withingsTooManyRequests || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			err = fmt.Errorf("%w: %w", errRetryable, err)
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Body, out)
}

// requestToken exchanges an authorisation code or refresh token for a new token set.
func (i *Importer) requestToken(ctx context.Context, form url.Values) (tokens, error) {
	form.Set("action", "requesttoken")
	form.Set("client_id", i.config().ClientID)
	form.Set("client_secret", i.config().ClientSecret)
	var b tokenBody
	if err := i.call(ctx, "/v2/oauth2", "", form, &b); err != nil {
		return tokens{}, err
	}
	if b.AccessToken == "" || b.RefreshToken == "" {
		return tokens{}, fmt.Errorf("withings token response is missing tokens")
	}
	return tokens{
		AccessToken:  b.AccessToken,
		RefreshToken: b.RefreshToken,
		ExpiresAt:    i.now().Add(time.Duration(b.ExpiresIn) * time.Second).UTC(),
		UserID:       b.UserID.String(),
	}, nil
}

// getMeas fetches all real measurement groups (category 1) in [start, end]; zero times are open ends.
func (i *Importer) getMeas(ctx context.Context, accessToken string, start, end time.Time) ([]measureGroup, error) {
	var all []measureGroup
	offset := 0
	for page := 0; page < 1000; page++ {
		form := url.Values{"action": {"getmeas"}, "meastypes": {measTypes}, "category": {"1"}}
		if !start.IsZero() {
			form.Set("startdate", strconv.FormatInt(start.Unix(), 10))
		}
		if !end.IsZero() {
			form.Set("enddate", strconv.FormatInt(end.Unix(), 10))
		}
		if offset > 0 {
			form.Set("offset", strconv.Itoa(offset))
		}
		var b measBody
		if err := i.callRetry(ctx, "/measure", accessToken, form, &b); err != nil {
			return nil, err
		}
		all = append(all, b.Groups...)
		if b.More == 0 {
			return all, nil
		}
		offset = b.Offset
	}
	return nil, fmt.Errorf("withings getmeas: too many pages")
}
