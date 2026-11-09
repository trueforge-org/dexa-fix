// Package runner is the loop every worker runs: register with the backend, lease jobs, run the
// handler of the job's kind while renewing the lease, and report how it ended. It knows nothing
// about providers; a provider package registers handlers for its kinds with Handle.
//
// The fail-safes a worker contributes: the attempt runs under the backend's time limit; a lost
// lease or a cancel stops the handler; a handler that panics or returns an unclassified error
// ends the attempt as a retry (the backend bounds the retries); a worker that is shutting down
// gives its jobs back instead of leaving them to time out. A worker has no database: everything
// it learns or must remember goes through the backend.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
)

// Job is what a handler works on.
type Job struct {
	workerapi.Job
	// Config is the administrator's configuration of the job's kind, as it was when the job started.
	Config workerapi.KindConfig

	run *run
}

// Progress reports how far the job is (any JSON-encodable value the person can be shown). It is
// sent with the next heartbeat.
func (j *Job) Progress(v any) {
	if b, err := json.Marshal(v); err == nil {
		j.run.setProgress(b)
	}
}

// SaveCredentials hands the backend credentials the job renewed (a refreshed OAuth token pair)
// and returns once they are stored. Do it before using them: a refresh token works once.
func (j *Job) SaveCredentials(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return j.run.r.client.SaveCredentials(ctx, j.ID, j.run.token, b)
}

// Handler runs one job. It returns the result (anything JSON-encodable, or nil) or an error. Return
// Permanent(err) for a problem trying again cannot fix (a wrong password, a revoked token, a bad
// request), Retryable(err, after) for a temporary one with a wait the provider asked for; any other
// error counts as temporary. The error text reaches the administrator and sometimes the person:
// never put a credential in it. Stop when ctx ends.
type Handler func(ctx context.Context, job *Job) (result any, err error)

type classified struct {
	err   error
	retry bool
	after time.Duration
}

func (c *classified) Error() string { return c.err.Error() }
func (c *classified) Unwrap() error { return c.err }

// Permanent marks err as one that no retry can fix.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &classified{err: err}
}

// Retryable marks err as temporary; after is the wait the provider asked for (0: none).
func Retryable(err error, after time.Duration) error {
	if err == nil {
		return nil
	}
	return &classified{err: err, retry: true, after: after}
}

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool {
	var c *classified
	return errors.As(err, &c) && !c.retry
}

// IsRetryable reports whether err was marked with Retryable, and the wait it asked for.
func IsRetryable(err error) (bool, time.Duration) {
	var c *classified
	if errors.As(err, &c) && c.retry {
		return true, c.after
	}
	return false, 0
}

// Options configure a Runner.
type Options struct {
	ID          string // unique per process; empty: random
	Version     string
	Concurrency int // jobs run at once by this process (default 2)
	// Poll is how long a lease request waits for a job (default workerapi.MaxWaitSeconds).
	Poll time.Duration
	// ConfigEvery is how often the configuration is re-read (default 15 s).
	ConfigEvery time.Duration
}

// Runner is one worker process.
type Runner struct {
	client   *workerapi.Client
	opt      Options
	handlers map[string]Handler
	cfg      atomic.Pointer[workerapi.Config]
}

// New returns a Runner. Register the handlers with Handle before Run.
func New(client *workerapi.Client, opt Options) *Runner {
	if opt.ID == "" {
		opt.ID = fmt.Sprintf("worker-%08x", rand.Uint32())
	}
	if opt.Concurrency < 1 {
		opt.Concurrency = 2
	}
	if opt.Poll <= 0 || opt.Poll > workerapi.MaxWaitSeconds*time.Second {
		opt.Poll = workerapi.MaxWaitSeconds * time.Second
	}
	if opt.ConfigEvery <= 0 {
		opt.ConfigEvery = 15 * time.Second
	}
	return &Runner{client: client, opt: opt, handlers: map[string]Handler{}}
}

// Handle registers the handler of a kind of job.
func (r *Runner) Handle(kind string, h Handler) { r.handlers[kind] = h }

// ID is the name this process registered under.
func (r *Runner) ID() string { return r.opt.ID }

func (r *Runner) kinds() []string {
	out := make([]string, 0, len(r.handlers))
	for k := range r.handlers {
		out = append(out, k)
	}
	return out
}

// backoff waits d (with jitter) or until ctx ends; it reports whether to go on.
func backoff(ctx context.Context, d time.Duration) bool {
	d += time.Duration(rand.Int64N(int64(d/4) + 1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run works until ctx ends. It returns only an error that no waiting will fix.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.handlers) == 0 {
		return errors.New("no handlers registered")
	}
	// The backend may still be starting: wait for it, with growing pauses.
	wait := time.Second
	for {
		cfg, err := r.client.Register(ctx, workerapi.RegisterRequest{ID: r.opt.ID, Version: r.opt.Version, Kinds: r.kinds()})
		if err == nil {
			r.cfg.Store(&cfg)
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		var e *apiclient.Error
		if errors.As(err, &e) && (e.Status == 401 || e.Status == 403) {
			return fmt.Errorf("the backend refused the worker secret: %w", err)
		}
		log.Warn().Err(err).Msg("backend not reachable, waiting")
		if !backoff(ctx, wait) {
			return nil
		}
		if wait < 30*time.Second {
			wait *= 2
		}
	}
	log.Info().Str("id", r.opt.ID).Strs("kinds", r.kinds()).Int("concurrency", r.opt.Concurrency).Msg("worker registered")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.followConfig(ctx) }()
	for i := 0; i < r.opt.Concurrency; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.loop(ctx) }()
	}
	wg.Wait()
	return nil
}

func (r *Runner) followConfig(ctx context.Context) {
	t := time.NewTicker(r.opt.ConfigEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg, err := r.client.Config(ctx)
		if err != nil {
			continue // keep the last configuration
		}
		if old := r.cfg.Load(); old == nil || cfg.Generation != old.Generation {
			r.cfg.Store(&cfg)
		}
	}
}

// enabledKinds are the registered kinds the administrator has not turned off.
func (r *Runner) enabledKinds() []string {
	cfg := r.cfg.Load()
	var out []string
	for k := range r.handlers {
		if cfg != nil && cfg.Kinds[k].Enabled {
			out = append(out, k)
		}
	}
	return out
}

func (r *Runner) loop(ctx context.Context) {
	wait := time.Second
	for ctx.Err() == nil {
		kinds := r.enabledKinds()
		if len(kinds) == 0 {
			if !backoff(ctx, 5*time.Second) {
				return
			}
			continue
		}
		lease, err := r.client.Lease(ctx, workerapi.LeaseRequest{Worker: r.opt.ID, Kinds: kinds, WaitSeconds: int(r.opt.Poll / time.Second)})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn().Err(err).Msg("could not ask for a job")
			if !backoff(ctx, wait) {
				return
			}
			if wait < 30*time.Second {
				wait *= 2
			}
			continue
		}
		wait = time.Second
		if lease == nil {
			continue
		}
		r.execute(ctx, lease)
	}
}

// run is the state of one attempt.
type run struct {
	r     *Runner
	token string

	mu       sync.Mutex
	progress json.RawMessage
	sent     bool
}

func (a *run) setProgress(b json.RawMessage) {
	a.mu.Lock()
	a.progress, a.sent = b, false
	a.mu.Unlock()
}

func (a *run) takeProgress() json.RawMessage {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sent {
		return nil
	}
	a.sent = true
	return a.progress
}

var (
	errLost     = errors.New("lease lost")
	errCanceled = errors.New("canceled")
)

func (r *Runner) execute(parent context.Context, lease *workerapi.Lease) {
	job := lease.Job
	l := log.With().Str("job", job.ID).Str("kind", job.Kind).Int64("user", job.UserID).Int("attempt", job.Attempt).Logger()
	h := r.handlers[job.Kind]
	if h == nil { // the backend only leases kinds we asked for, but do not trust it blindly
		l.Error().Msg("no handler for this kind")
		r.report(job.ID, lease.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "this worker cannot run jobs of kind " + job.Kind})
		return
	}
	timeout := time.Duration(lease.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeoutCause(parent, timeout, errors.New("the attempt took longer than its time limit"))
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	a := &run{r: r, token: lease.Token}
	cfg := workerapi.KindConfig{}
	if c := r.cfg.Load(); c != nil {
		cfg = c.Kinds[job.Kind]
	}

	// Keep the lease alive; stop the handler when the job is no longer ours.
	leaseFor := time.Duration(lease.LeaseSeconds) * time.Second
	if leaseFor <= 0 {
		leaseFor = time.Minute
	}
	beat := leaseFor / 3
	if beat < time.Second {
		beat = time.Second
	}
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(beat)
		defer t.Stop()
		lastOK := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			cancelled, err := r.client.Heartbeat(ctx, job.ID, lease.Token, a.takeProgress())
			switch {
			case err == nil:
				lastOK = time.Now()
				if cancelled {
					stop(errCanceled)
					return
				}
			case errors.Is(err, workerapi.ErrLeaseLost):
				stop(errLost)
				return
			case ctx.Err() != nil:
				return
			default:
				// The backend is unreachable. Once the lease would have run out, it belongs to someone
				// else, so carrying on only wastes the provider's time.
				if time.Since(lastOK) > leaseFor {
					stop(errLost)
					return
				}
			}
		}
	}()

	result, err := r.call(ctx, h, &Job{Job: job, Config: cfg, run: a}, l)
	stop(nil)
	<-hbDone

	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errLost):
		l.Warn().Msg("the lease was lost: the result is dropped")
		return
	case parent.Err() != nil:
		// Shutting down: give the job back without costing an attempt.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
		defer rcancel()
		if err := r.client.Release(rctx, job.ID, lease.Token); err != nil && !errors.Is(err, workerapi.ErrLeaseLost) {
			l.Warn().Err(err).Msg("could not give the job back")
		}
		return
	}

	comp := workerapi.Completion{Outcome: models.OutcomeOK}
	switch {
	case errors.Is(cause, errCanceled):
		comp = workerapi.Completion{Outcome: models.OutcomeRetry, Error: "canceled"}
	case err != nil:
		var c *classified
		comp = workerapi.Completion{Outcome: models.OutcomeRetry, Error: err.Error()}
		if errors.As(err, &c) {
			if !c.retry {
				comp.Outcome = models.OutcomeFail
			}
			comp.RetryAfterSeconds = int(c.after / time.Second)
		}
		if errors.Is(err, context.DeadlineExceeded) && cause != nil {
			comp.Error = cause.Error()
		}
		l.Warn().Err(err).Str("outcome", comp.Outcome).Msg("job failed")
	default:
		if result != nil {
			b, merr := json.Marshal(result)
			if merr != nil {
				comp = workerapi.Completion{Outcome: models.OutcomeFail, Error: "the result could not be encoded"}
			} else {
				comp.Result = b
			}
		}
	}
	r.report(job.ID, lease.Token, comp)
}

// call runs the handler and turns a panic into an error (a retry: the backend bounds them). The
// stack is logged, the payload never.
func (r *Runner) call(ctx context.Context, h Handler, job *Job, l zerolog.Logger) (result any, err error) {
	defer func() {
		if v := recover(); v != nil {
			l.Error().Str("panic", fmt.Sprint(v)).Bytes("stack", debug.Stack()).Msg("the handler panicked")
			result, err = nil, errors.New("internal error in the worker")
		}
	}()
	return h(ctx, job)
}

// report sends the end of an attempt, with a few tries: the result of a long job is worth keeping.
func (r *Runner) report(id, token string, c workerapi.Completion) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	wait := time.Second
	for try := 0; try < 5; try++ {
		err := r.client.Complete(ctx, id, token, c)
		if err == nil || errors.Is(err, workerapi.ErrLeaseLost) {
			return
		}
		var e *apiclient.Error
		if errors.As(err, &e) { // refused for good (not a hiccup)
			log.Error().Err(err).Str("job", id).Msg("the backend refused the report")
			return
		}
		if !backoff(ctx, wait) {
			break
		}
		wait *= 2
	}
	log.Error().Str("job", id).Msg("could not report the end of the attempt; the backend will time the job out")
}
