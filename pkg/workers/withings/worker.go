// Package withings is the worker for Withings: it talks to the Withings API for the backend. It
// plays the scale-source role (see pkg/shared/workerapi): "withings.connect" exchanges the code
// the user got when they allowed access, and "withings.import" fetches their measurements.
//
// The worker has no database. The application's credentials and the user's tokens come with each
// job; a token pair the worker renews is handed back with SaveCredentials before it is used,
// because a Withings refresh token works once and losing the new pair disconnects the user. What
// to store is decided by the backend: this package only fetches and maps.
package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/workers/runner"
)

// Provider is the provider's name: the first part of its kinds and the readings' Source.
const Provider = workerapi.ProviderWithings

// The kinds this worker runs.
const (
	KindConnect = Provider + "." + workerapi.ActionConnect // same as workerapi.Kind(Provider, workerapi.ActionConnect)
	KindImport  = Provider + "." + workerapi.ActionImport
)

// refreshMargin is how long before expiry the access token is refreshed.
const refreshMargin = time.Minute

// Worker runs the Withings kinds of job.
type Worker struct {
	cfg  Config
	http *http.Client
	now  func() time.Time
}

// New returns a worker. A nil client uses a 30 s timeout.
func New(cfg Config, client *http.Client) *Worker {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &Worker{cfg: cfg, http: client, now: time.Now}
}

// Handlers returns the handler of each kind, for runner.Runner.Handle.
func (w *Worker) Handlers() map[string]runner.Handler {
	return map[string]runner.Handler{
		KindConnect: func(ctx context.Context, job *runner.Job) (any, error) {
			var p workerapi.ConnectPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return nil, runner.Permanent(errors.New("unreadable job"))
			}
			return nil, w.connect(ctx, p, job.SaveCredentials)
		},
		KindImport: func(ctx context.Context, job *runner.Job) (any, error) {
			var p workerapi.ImportPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return nil, runner.Permanent(errors.New("unreadable job"))
			}
			res, err := w.importReadings(ctx, p, job.SaveCredentials)
			if err != nil {
				return nil, err
			}
			return res, nil
		},
	}
}

// saveFunc hands credentials to the backend (runner.Job.SaveCredentials).
type saveFunc func(ctx context.Context, v any) error

// classify says whether a failure is worth another try: a transport problem or a busy Withings is,
// a refusal is not.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errRetryable) {
		return runner.Retryable(err, 0)
	}
	return runner.Permanent(err)
}

// hand gives the backend a new token pair, trying a few times: if it cannot be stored, the old
// pair (now invalid) is all that is left and the user has to connect again.
func hand(ctx context.Context, save saveFunc, t tokens) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = save(ctx, t); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(retryBackoff(attempt)):
		}
	}
	return err
}

// connect exchanges the authorisation code for the user's tokens and hands them over.
func (w *Worker) connect(ctx context.Context, p workerapi.ConnectPayload, save saveFunc) error {
	if p.App.ClientID == "" || p.App.ClientSecret == "" || p.App.RedirectURL == "" || p.Code == "" {
		return runner.Permanent(errors.New("withings is not configured"))
	}
	t, err := w.requestToken(ctx, p.App, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {p.Code},
		"redirect_uri": {p.App.RedirectURL},
	})
	if err != nil {
		return classify(err)
	}
	if err := hand(ctx, save, t); err != nil {
		return runner.Retryable(fmt.Errorf("could not store the tokens: %w", err), 0)
	}
	return nil
}

// importReadings fetches the user's measurements, refreshing the access token first when needed.
func (w *Worker) importReadings(ctx context.Context, p workerapi.ImportPayload, save saveFunc) (workerapi.ImportResult, error) {
	var none workerapi.ImportResult
	if p.App.ClientID == "" || p.App.ClientSecret == "" {
		return none, runner.Permanent(errors.New("withings is not configured"))
	}
	var t tokens
	if err := json.Unmarshal(p.Credentials, &t); err != nil || t.RefreshToken == "" {
		return none, runner.Permanent(errors.New("the stored withings connection is unreadable, reconnect"))
	}
	if !w.now().Add(refreshMargin).Before(t.ExpiresAt) {
		nt, err := w.requestToken(ctx, p.App, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}})
		if err != nil {
			return none, classify(fmt.Errorf("refresh withings token (reconnect if this persists): %w", err))
		}
		// Store the new pair before anything else: the old one no longer works.
		if err := hand(ctx, save, nt); err != nil {
			return none, runner.Permanent(fmt.Errorf("could not store the refreshed tokens, reconnect: %w", err))
		}
		t = nt
	}

	// Days are the user's calendar days, in their time zone; p.Day is a plain date.
	loc := time.UTC
	if p.Zone != "" {
		l, err := time.LoadLocation(p.Zone)
		if err != nil {
			return none, runner.Permanent(fmt.Errorf("unknown time zone %q", p.Zone))
		}
		loc = l
	}
	var start, end time.Time
	switch {
	case p.Day != "":
		d, err := time.Parse("2006-01-02", p.Day)
		if err != nil {
			return none, runner.Permanent(errors.New("day must be a date like 2026-03-01"))
		}
		start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		end = start.AddDate(0, 0, 1).Add(-time.Second)
	case !p.Since.IsZero():
		start = p.Since
	}
	groups, err := w.getMeas(ctx, t.AccessToken, start, end)
	if err != nil {
		return none, classify(err)
	}
	readings, seen := mapGroups(groups)
	res := workerapi.ImportResult{Readings: readings}
	for id := range seen {
		res.Seen = append(res.Seen, id)
	}
	return res, nil
}
