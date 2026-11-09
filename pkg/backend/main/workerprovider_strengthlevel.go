package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/estimate"
	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

// Strength Level: an exporter. A user with the administrator's permission keeps their own Strength
// Level login (encrypted, in user_integrations) and sends the daily estimates of weight and body
// fat to it: a single day from the Estimates page, many days from Settings, or on a schedule. The
// worker (pkg/workers/strengthlevel) does the talking; this is what the backend does around it:
// it checks the permission, reads the login and computes the estimates when a job is leased, and
// keeps what comes back.

const (
	// strengthLevelProvider is the provider's name and the integration name under which the login is stored.
	strengthLevelProvider = workerapi.ProviderStrengthLevel

	// A sync adds the missing days of the last N days. N defaults to 90 and is at most 10 years, so
	// a slip of the finger does not write a lifetime of data.
	strengthLevelDefaultDays = 90
	strengthLevelMaxDays     = 3650

	maxStrengthLevelLoginFail = 5 // wrong Strength Level logins per user and limiter window

	// strengthLevelTimeout bounds a sign-in; a sync of many days gets two more seconds per day.
	strengthLevelTimeout = 2 * time.Minute

	optionRequestDelay   = "request_delay_ms"
	optionScheduledDays  = "scheduled_days"
	defaultRequestDelay  = 300
	defaultScheduledDays = 14
)

var (
	kindStrengthLevelCheckLogin = workerapi.Kind(strengthLevelProvider, workerapi.ActionCheckLogin)
	kindStrengthLevelDay        = workerapi.Kind(strengthLevelProvider, workerapi.ActionDay)
	kindStrengthLevelSync       = workerapi.Kind(strengthLevelProvider, workerapi.ActionSync)
)

// slLogin is the Strength Level login, stored as the data of the integration (and, until it was
// checked, as the payload of the job that checks it).
type slLogin struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type slDayPayload struct {
	Day string `json:"day"`
}

type slSyncPayload struct {
	Days   int  `json:"days"`
	Update bool `json:"update,omitempty"`
}

// slSyncSummary is what a finished sync keeps: the worker's counts and what was asked for, because
// the payload is gone when the job ends.
type slSyncSummary struct {
	workerapi.ExportResult
	Days   int  `json:"days"`
	Update bool `json:"update,omitempty"`
}

func strengthLevelProviderSpec() workerProvider {
	opts := []optionField{
		{Name: optionRequestDelay, Label: "Pause between two writes to Strength Level", Default: defaultRequestDelay, Min: 0, Max: 10000, Millis: true},
	}
	return workerProvider{
		Name: strengthLevelProvider, Label: "Strength Level", Integration: strengthLevelProvider,
		Kinds: []kindSpec{
			{
				Kind: kindStrengthLevelCheckLogin, Label: "Strength Level login check",
				Defaults: kindDefaults{MaxRunning: 4, MaxAttempts: 2, RetryBase: 5 * time.Second, Timeout: strengthLevelTimeout},
				Lease:    leaseStrengthLevelCheckLogin,
				Apply:    applyStrengthLevelCheckLogin,
			},
			{
				Kind: kindStrengthLevelDay, Label: "Strength Level day",
				Defaults: kindDefaults{MaxRunning: 4, MaxAttempts: 2, RetryBase: 5 * time.Second, Timeout: strengthLevelTimeout},
				Options:  opts,
				Lease:    leaseStrengthLevelDay,
				Apply:    applyStrengthLevelResult,
			},
			{
				Kind: kindStrengthLevelSync, Label: "Strength Level sync",
				// A few syncs at once over all users, as before: Strength Level is one small site.
				Defaults: kindDefaults{MaxRunning: 3, MaxAttempts: 3, RetryBase: time.Minute, Timeout: time.Hour, PauseAfter: 5, Breaker: 10},
				Options: append(append([]optionField{}, opts...),
					optionField{Name: optionScheduledDays, Label: "Days an automatic sync covers", Default: defaultScheduledDays, Min: 1, Max: strengthLevelMaxDays}),
				ScheduleFor: scheduleStrengthLevelSync,
				Lease:       leaseStrengthLevelSync,
				Apply:       applyStrengthLevelSync,
			},
		},
	}
}

// entriesFromEstimates turns daily estimates into entries, newest day first. Every estimated value
// counts, whatever its origin: measured, interpolated between readings, held after the last one, or
// from a DEXA scan. Days without a weight estimate are left out.
func entriesFromEstimates(days []estimate.DayEstimate) []workerapi.ExportEntry {
	out := make([]workerapi.ExportEntry, 0, len(days))
	for _, d := range days {
		w, ok := d.Values["weight_kg"]
		if !ok {
			continue
		}
		e := workerapi.ExportEntry{Day: d.Date.UTC().Format(dateLayout), WeightKg: w.V}
		if f, ok := d.Values["fat_ratio_pct"]; ok {
			v := f.V
			e.FatPct = &v
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day > out[j].Day })
	return out
}

// strengthLevelUser returns the user of a job if the administrator still allows Strength Level for them.
func (s *Backend) strengthLevelUser(ctx context.Context, userID int64) (models.User, error) {
	u, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return u, permanent(errors.New("the user no longer exists"))
	}
	if err != nil {
		return u, err
	}
	if !u.StrengthLevelAuto {
		return u, permanent(errors.New("Strength Level sync is not enabled for this account"))
	}
	return u, nil
}

// strengthLevelLoginOf reads the user's stored login.
func (s *Backend) strengthLevelLoginOf(ctx context.Context, userID int64) (workerapi.Login, error) {
	data, err := s.store.GetIntegration(ctx, userID, strengthLevelProvider)
	if errors.Is(err, store.ErrNotFound) {
		return workerapi.Login{}, permanent(errors.New("Save your Strength Level login in Settings first."))
	}
	if err != nil {
		return workerapi.Login{}, err
	}
	var l slLogin
	if err := json.Unmarshal(data, &l); err != nil || l.Email == "" {
		return workerapi.Login{}, permanent(errors.New("the stored Strength Level login is unreadable, save it again"))
	}
	return workerapi.Login{Email: l.Email, Password: l.Password}, nil
}

func leaseStrengthLevelCheckLogin(ctx context.Context, s *Backend, j models.Job) (any, error) {
	if _, err := s.strengthLevelUser(ctx, j.UserID); err != nil {
		return nil, err
	}
	var l slLogin
	if err := json.Unmarshal([]byte(j.Payload), &l); err != nil || l.Email == "" {
		return nil, permanent(errors.New("there is no login to check"))
	}
	return workerapi.CheckLoginPayload{Login: workerapi.Login{Email: l.Email, Password: l.Password}}, nil
}

func leaseStrengthLevelDay(ctx context.Context, s *Backend, j models.Job) (any, error) {
	u, err := s.strengthLevelUser(ctx, j.UserID)
	if err != nil {
		return nil, err
	}
	login, err := s.strengthLevelLoginOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	var in slDayPayload
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil {
		return nil, permanent(errors.New("unreadable job"))
	}
	est, err := s.computeEstimates(ctx, u)
	if err != nil {
		return nil, err
	}
	for _, e := range entriesFromEstimates(est.Days) {
		if e.Day == in.Day {
			return workerapi.ExportPayload{Login: login, Entries: []workerapi.ExportEntry{e}, Update: true}, nil
		}
	}
	return nil, permanent(errors.New("There is no weight estimate for " + in.Day + " to send."))
}

func leaseStrengthLevelSync(ctx context.Context, s *Backend, j models.Job) (any, error) {
	u, err := s.strengthLevelUser(ctx, j.UserID)
	if err != nil {
		return nil, err
	}
	login, err := s.strengthLevelLoginOf(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	var in slSyncPayload
	if err := json.Unmarshal([]byte(j.Payload), &in); err != nil || in.Days < 1 || in.Days > strengthLevelMaxDays {
		return nil, permanent(errors.New("unreadable job"))
	}
	est, err := s.computeEstimates(ctx, u)
	if err != nil {
		return nil, err
	}
	return workerapi.ExportPayload{
		Login: login, Entries: entriesFromEstimates(est.Days), Update: in.Update,
		Since: time.Now().UTC().AddDate(0, 0, -in.Days).Format(dateLayout),
	}, nil
}

func scheduleStrengthLevelSync(ctx context.Context, s *Backend) ([]scheduleTarget, error) {
	ids, err := s.store.ListUsersWithIntegration(ctx, strengthLevelProvider)
	if err != nil {
		return nil, err
	}
	days := s.cfg().workers.Kinds[kindStrengthLevelSync].Options[optionScheduledDays]
	if days < 1 {
		days = defaultScheduledDays
	}
	var out []scheduleTarget
	for _, id := range ids {
		if u, err := s.store.GetUser(ctx, id); err == nil && u.StrengthLevelAuto {
			out = append(out, scheduleTarget{UserID: id, Payload: slSyncPayload{Days: days}})
		}
	}
	return out, nil
}

// applyStrengthLevelCheckLogin keeps the login when Strength Level accepted it, and counts a
// refusal against the user like a wrong password anywhere else.
func applyStrengthLevelCheckLogin(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error) {
	var res workerapi.CheckLoginResult
	if err := json.Unmarshal(result, &res); err != nil {
		return "", errBadResult
	}
	key := "sl:" + strconv.FormatInt(j.UserID, 10)
	if !res.LoginOK {
		s.limiter.Fail(ctx, key)
		return string(result), nil
	}
	if _, err := s.strengthLevelUser(ctx, j.UserID); err != nil {
		return "", errBadResult // permission removed while the check ran
	}
	var l slLogin
	if err := json.Unmarshal([]byte(j.Payload), &l); err != nil || l.Email == "" {
		return "", errBadResult
	}
	if err := s.store.PutIntegration(ctx, j.UserID, strengthLevelProvider, []byte(j.Payload)); err != nil {
		return "", err
	}
	s.limiter.Reset(ctx, key)
	s.resumeSchedule(ctx, j.UserID, kindStrengthLevelSync)
	s.auditBackground(j.UserID, auditSLLoginSaved, "")
	return string(result), nil
}

// applyStrengthLevelResult keeps what a worker reports for an export that stores nothing here.
func applyStrengthLevelResult(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error) {
	var res workerapi.ExportResult
	if err := json.Unmarshal(result, &res); err != nil {
		return "", errBadResult
	}
	return string(result), nil
}

func applyStrengthLevelSync(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error) {
	var res workerapi.ExportResult
	if err := json.Unmarshal(result, &res); err != nil {
		return "", errBadResult
	}
	var in slSyncPayload
	_ = json.Unmarshal([]byte(j.Payload), &in)
	if len(res.FailedMessages) > 0 {
		log.Warn().Int64("user", j.UserID).Int("failed", res.Failed).Strs("messages", res.FailedMessages).Msg("strengthlevel sync")
	}
	b, err := json.Marshal(slSyncSummary{ExportResult: res, Days: in.Days, Update: in.Update})
	return string(b), err
}

// slJobViewOf describes the user's latest sync for the settings page, or nil when there has been none.
func (s *Backend) slJobViewOf(ctx context.Context, userID int64) (*slJobView, error) {
	latest, err := s.store.ListJobs(ctx, models.JobFilter{UserID: userID, Kinds: []string{kindStrengthLevelSync}, Limit: 1})
	if err != nil || len(latest) == 0 {
		return nil, err
	}
	j, err := s.store.GetJob(ctx, latest[0].PublicID)
	if err != nil {
		return nil, err
	}
	v := &slJobView{Running: models.JobActive(j.State)}
	switch j.State {
	case models.JobQueued:
		v.Text = "Sync waiting for a worker…"
	case models.JobRunning:
		var p workerapi.ExportProgress
		_ = json.Unmarshal([]byte(j.Progress), &p)
		v.Done, v.Total = p.Done, p.Total
		if p.Total == 0 {
			v.Text = "Sync running: signing in…"
		} else {
			v.Text = fmt.Sprintf("Sync running: %d of %d days checked. You can leave this page.", p.Done, p.Total)
		}
	case models.JobCanceled:
		v.Text = "The last sync was canceled."
	case models.JobFailed, models.JobExpired:
		v.Text = "The last sync stopped: " + j.Error
	default:
		var sum slSyncSummary
		_ = json.Unmarshal([]byte(j.Result), &sum)
		finished := j.UpdatedAt
		if j.FinishedAt != nil {
			finished = *j.FinishedAt
		}
		v.Text = fmt.Sprintf("Last sync (%s UTC, last %d days%s): added %d, updated %d, %d already up to date",
			finished.UTC().Format("2006-01-02 15:04"), sum.Days, map[bool]string{true: ", updating existing days"}[sum.Update],
			sum.Added, sum.Updated, sum.Unchanged)
		if sum.Failed > 0 {
			v.Text += fmt.Sprintf(", %d could not be written", sum.Failed)
		}
		v.Text += "."
	}
	return v, nil
}

// stopStrengthLevel ends what is queued or running for the user at Strength Level, and forgets the
// schedule: the login or the permission is gone.
func (s *Backend) stopStrengthLevel(ctx context.Context, userID int64, kinds ...string) {
	if len(kinds) == 0 {
		kinds = []string{kindStrengthLevelCheckLogin, kindStrengthLevelDay, kindStrengthLevelSync}
	}
	if err := s.store.CancelJobs(ctx, userID, kinds, time.Now()); err != nil {
		log.Warn().Err(err).Msg("stop the Strength Level jobs")
	}
	s.resumeSchedule(ctx, userID, kindStrengthLevelSync)
}
