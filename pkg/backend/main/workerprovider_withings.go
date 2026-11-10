package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// Withings: a source of scale readings that users connect to by OAuth. The worker (pkg/workers/
// withings) talks to Withings; this is what the backend does around it: it holds the application's
// credentials (Admin settings) and each user's tokens, hands them to a worker only inside a lease,
// and stores what comes back by the rules in reconcile.go.

const (
	providerWithings = workerapi.ProviderWithings

	settingWithingsClientID = "withings.client_id"
	settingWithingsSecret   = "withings.client_secret"
	settingWithingsRedirect = "withings.redirect_url"

	// maxImportReadings bounds what one import result may hold.
	maxImportReadings = 200000
	// scheduledImportWindow is how far back an automatic import looks.
	scheduledImportWindow = 14 * 24 * time.Hour
)

var (
	kindWithingsConnect = workerapi.Kind(providerWithings, workerapi.ActionConnect)
	kindWithingsImport  = workerapi.Kind(providerWithings, workerapi.ActionImport)

	// withingsAuthBase is where users are sent to grant access; tests point it elsewhere.
	withingsAuthBase = "https://account.withings.com"
)

// importPayload is the parameters of an import job, kept with the job (the credentials are not: they
// are read when the job is leased).
type importPayload struct {
	Since time.Time `json:"since,omitempty"`
	Day   string    `json:"day,omitempty"` // YYYY-MM-DD
}

// connectPayload is the parameters of a connect job: the one-time code from the provider.
type connectPayload struct {
	Code string `json:"code"`
}

// withingsApp reads the application's credentials, as an administrator saved them, now: a change
// reaches every backend and every worker at once because nothing keeps a copy.
func (s *Backend) withingsApp(ctx context.Context) (app workerapi.OAuthApp, configured bool, err error) {
	for _, f := range []struct {
		name string
		dst  *string
	}{{settingWithingsClientID, &app.ClientID}, {settingWithingsSecret, &app.ClientSecret}, {settingWithingsRedirect, &app.RedirectURL}} {
		if *f.dst, err = s.settingOrEmpty(ctx, f.name); err != nil {
			return app, false, err
		}
	}
	return app, app.ClientID != "" && app.ClientSecret != "" && app.RedirectURL != "", nil
}

func withingsProvider() workerProvider {
	return workerProvider{
		Name: providerWithings, Label: "Withings", Integration: providerWithings,
		OAuth: &oauthSpec{
			Configured: func(ctx context.Context, s *Backend) (bool, error) {
				_, ok, err := s.withingsApp(ctx)
				return ok, err
			},
			AuthURL: func(ctx context.Context, s *Backend, state string) (string, error) {
				app, _, err := s.withingsApp(ctx)
				if err != nil {
					return "", err
				}
				q := url.Values{
					"response_type": {"code"},
					"client_id":     {app.ClientID},
					"redirect_uri":  {app.RedirectURL},
					"scope":         {"user.metrics"},
					"state":         {state},
				}
				return withingsAuthBase + "/oauth2_user/authorize2?" + q.Encode(), nil
			},
		},
		Kinds: []kindSpec{
			{
				Kind: kindWithingsConnect, Label: "Withings connect",
				// The code Withings hands out is short-lived and works once, so a job that waits is useless.
				Defaults: kindDefaults{MaxRunning: 4, MaxAttempts: 2, RetryBase: 3 * time.Second, Timeout: time.Minute, PauseAfter: 0, Breaker: 0},
				Lease:    leaseWithingsConnect,
				Apply:    applyWithingsConnect,
				Credentials: func(ctx context.Context, s *Backend, j models.Job, data json.RawMessage) error {
					return s.storeWithingsTokens(ctx, j, data, true)
				},
			},
			{
				Kind: kindWithingsImport, Label: "Withings import",
				Defaults: kindDefaults{MaxRunning: 4, MaxAttempts: 3, RetryBase: time.Minute, Timeout: 5 * time.Minute, PauseAfter: 5, Breaker: 10},
				ScheduleFor: func(ctx context.Context, s *Backend) ([]scheduleTarget, error) {
					ids, err := s.store.ListUsersWithIntegration(ctx, providerWithings)
					out := make([]scheduleTarget, 0, len(ids))
					for _, id := range ids {
						out = append(out, scheduleTarget{UserID: id, Payload: importPayload{Since: time.Now().UTC().Add(-scheduledImportWindow).Truncate(time.Hour)}})
					}
					return out, err
				},
				Lease: leaseWithingsImport,
				Apply: applyWithingsImport,
				Credentials: func(ctx context.Context, s *Backend, j models.Job, data json.RawMessage) error {
					return s.storeWithingsTokens(ctx, j, data, false)
				},
			},
		},
	}
}

func leaseWithingsConnect(ctx context.Context, s *Backend, j models.Job) (any, error) {
	app, ok, err := s.withingsApp(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, permanent(errors.New("Withings is not configured on this server."))
	}
	var in connectPayload
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil || in.Code == "" {
		return nil, permanent(errors.New("the connection has no authorisation code"))
	}
	return workerapi.ConnectPayload{App: app, Code: in.Code}, nil
}

func leaseWithingsImport(ctx context.Context, s *Backend, j models.Job) (any, error) {
	app, ok, err := s.withingsApp(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, permanent(errors.New("Withings is not configured on this server."))
	}
	data, err := s.store.GetIntegration(ctx, j.UserID, providerWithings)
	if errors.Is(err, store.ErrNotFound) {
		return nil, permanent(errors.New("Withings is not connected."))
	}
	if err != nil {
		return nil, err
	}
	u, err := s.store.GetUser(ctx, j.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, permanent(errors.New("the user no longer exists"))
	}
	if err != nil {
		return nil, err
	}
	var in importPayload
	if j.Payload != "" {
		if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
			return nil, permanent(errors.New("unreadable import parameters"))
		}
	}
	return workerapi.ImportPayload{App: app, Credentials: data, Since: in.Since, Day: in.Day, Zone: u.Timezone}, nil
}

// storeWithingsTokens keeps a token pair a worker obtained. A refresh token works once, so a worker
// hands over the new pair before using it. A job that only refreshes (an import) must not bring
// back a connection the user removed meanwhile.
func (s *Backend) storeWithingsTokens(ctx context.Context, j models.Job, data json.RawMessage, connecting bool) error {
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &t); err != nil || t.AccessToken == "" || t.RefreshToken == "" {
		return errBadResult
	}
	if !connecting {
		if _, err := s.store.GetIntegration(ctx, j.UserID, providerWithings); errors.Is(err, store.ErrNotFound) {
			return nil
		}
	}
	return s.store.PutIntegration(ctx, j.UserID, providerWithings, data)
}

func applyWithingsConnect(ctx context.Context, s *Backend, j models.Job, _ json.RawMessage) (string, error) {
	// The tokens were stored when the worker handed them over; without them the job did not work.
	if _, err := s.store.GetIntegration(ctx, j.UserID, providerWithings); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", errBadResult
		}
		return "", err
	}
	s.resumeSchedule(ctx, j.UserID, kindWithingsImport) // a user who fixed the connection is scheduled again
	s.auditBackground(j.UserID, auditConnect, providerWithings)
	return "", nil
}

func applyWithingsImport(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error) {
	var res workerapi.ImportResult
	if err := json.Unmarshal(result, &res); err != nil || len(res.Readings) > maxImportReadings {
		return "", errBadResult
	}
	for i := range res.Readings {
		r := &res.Readings[i]
		if r.Source != providerWithings || r.ExternalID == nil || *r.ExternalID == "" || r.MeasuredAt.IsZero() {
			return "", errBadResult
		}
		r.ID, r.UserID = 0, j.UserID // a worker never chooses whose readings these are
	}
	var in importPayload
	if j.Payload != "" {
		if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
			return "", errBadResult
		}
	}
	u, err := s.store.GetUser(ctx, j.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return "", errBadResult // the user was deleted meanwhile
	}
	if err != nil {
		return "", err
	}
	if in.Day != "" {
		if _, err = time.Parse(dateLayout, in.Day); err != nil {
			return "", errBadResult
		}
	}
	out, err := s.body.Reconcile(ctx, u, bodyapi.ReconcileRequest{
		Source: providerWithings, Fetched: res.Readings, Seen: res.Seen, DayMode: in.Day != "", Day: in.Day,
	})
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}
