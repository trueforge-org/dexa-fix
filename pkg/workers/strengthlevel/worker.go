package strengthlevel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

// Provider is the provider's name: the first part of its kinds.
const Provider = workerapi.ProviderStrengthLevel

// The kinds this worker runs. It plays the exporter role (see pkg/shared/workerapi).
const (
	KindCheckLogin = Provider + "." + workerapi.ActionCheckLogin
	KindDay        = Provider + "." + workerapi.ActionDay
	KindSync       = Provider + "." + workerapi.ActionSync
)

// defaultDelay is the pause between two writes when the administrator set none.
const defaultDelay = 300 * time.Millisecond

// Config holds what only a test changes.
type Config struct {
	// Base is the address of Strength Level; empty means the real one.
	Base string
}

// Worker runs the Strength Level kinds of job. The user's login comes with each job and is used for
// that job only.
type Worker struct {
	cfg  Config
	http *http.Client
}

// New returns a worker. A nil client gets a default one.
func New(cfg Config, client *http.Client) *Worker { return &Worker{cfg: cfg, http: client} }

func (w *Worker) client(delay time.Duration) *Client {
	c := newClient(w.cfg.Base, w.http)
	c.Delay = delay
	return c
}

// Handlers returns the handler of each kind, for runner.Runner.Handle.
func (w *Worker) Handlers() map[string]runner.Handler {
	return map[string]runner.Handler{
		KindCheckLogin: func(ctx context.Context, job *runner.Job) (any, error) {
			var p workerapi.CheckLoginPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return nil, runner.Permanent(errors.New("unreadable job"))
			}
			return w.checkLogin(ctx, p)
		},
		KindDay: func(ctx context.Context, job *runner.Job) (any, error) {
			var p workerapi.ExportPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return nil, runner.Permanent(errors.New("unreadable job"))
			}
			return w.sendDay(ctx, p)
		},
		KindSync: func(ctx context.Context, job *runner.Job) (any, error) {
			var p workerapi.ExportPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				return nil, runner.Permanent(errors.New("unreadable job"))
			}
			delay := job.Config.OptionMillis("request_delay_ms", defaultDelay)
			return w.sync(ctx, p, delay, func(pr workerapi.ExportProgress) { job.Progress(pr) })
		},
	}
}

// describe says what went wrong in words for the person.
func describe(err error) string {
	switch {
	case errors.Is(err, ErrLogin):
		return "the e-mail address or password was refused."
	case errors.Is(err, ErrPage):
		log.Warn().Err(err).Msg("strengthlevel page changed")
		return "Strength Level looks different than expected, so it may have changed."
	}
	log.Warn().Err(err).Msg("strengthlevel request failed")
	return "Strength Level could not be reached."
}

// failure classifies an error for the runner. A refused login and a changed site are not fixed by
// trying again; an unreachable site may be. prefix starts the sentence the person reads.
func failure(err error, prefix string) error {
	e := errors.New(prefix + describe(err))
	if errors.Is(err, ErrLogin) || errors.Is(err, ErrPage) {
		return runner.Permanent(e)
	}
	return runner.Retryable(e, 0)
}

// checkLogin signs in once. A refused login is an answer, not a failed job.
func (w *Worker) checkLogin(ctx context.Context, p workerapi.CheckLoginPayload) (any, error) {
	err := w.client(0).Login(ctx, p.Login.Email, p.Login.Password)
	switch {
	case err == nil:
		return workerapi.CheckLoginResult{LoginOK: true}, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case errors.Is(err, ErrLogin):
		return workerapi.CheckLoginResult{Message: describe(err)}, nil
	}
	return nil, failure(err, "")
}

// sendDay sends one day: it adds it, or overwrites the entry that exists.
func (w *Worker) sendDay(ctx context.Context, p workerapi.ExportPayload) (any, error) {
	if len(p.Entries) != 1 {
		return nil, runner.Permanent(errors.New("there is no day to send"))
	}
	e := p.Entries[0]
	c := w.client(0)
	if err := c.Login(ctx, p.Login.Email, p.Login.Password); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(err, "Could not sign in to Strength Level: ")
	}
	out, err := c.Upsert(ctx, e)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(err, "Could not send "+e.Day+": ")
	}
	var res workerapi.ExportResult
	switch out {
	case Added:
		res.Added = 1
	case Updated:
		res.Updated = 1
	default:
		res.Unchanged = 1
	}
	return res, nil
}

// sync sends many days, oldest first, reporting progress as it goes.
func (w *Worker) sync(ctx context.Context, p workerapi.ExportPayload, delay time.Duration, progress func(workerapi.ExportProgress)) (any, error) {
	c := w.client(delay)
	if err := c.Login(ctx, p.Login.Email, p.Login.Password); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(err, "could not sign in: ")
	}
	res, err := c.Sync(ctx, p.Entries, p.Since, Options{
		Update:   p.Update,
		Progress: func(done, total int) { progress(workerapi.ExportProgress{Done: done, Total: total}) },
	})
	if len(res.Failed) > 0 {
		log.Warn().Strs("failed", res.Failed).Msg("strengthlevel sync")
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, failure(err, "")
	}
	out := workerapi.ExportResult{Added: res.Added, Updated: res.Updated, Unchanged: res.Skipped, Failed: len(res.Failed)}
	out.FailedMessages = res.Failed[:min(len(res.Failed), 5)]
	return out, nil
}
