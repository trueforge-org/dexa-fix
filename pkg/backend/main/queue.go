package mainbackend

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
)

// workerLease is how long a lease lasts without a heartbeat; the worker renews at a third of it.
// workerSeen is how long after its last request a worker still counts as running.
//
// They are variables so tests can shorten them. Workers run in their own goroutines and read them
// all the time, so they are atomic: changing one never races with a worker.
var (
	workerLease    = newTunable(60 * time.Second)
	workerSeen     = newTunable(90 * time.Second)
	leasePollEvery = newTunable(time.Second)
	maintainEvery  = newTunable(5 * time.Second) // the least time between two sweeps for expired leases
)

// tunable is a duration that can be changed while others read it.
type tunable struct{ ns atomic.Int64 }

func newTunable(d time.Duration) *tunable {
	t := &tunable{}
	t.Set(d)
	return t
}

func (t *tunable) Get() time.Duration  { return time.Duration(t.ns.Load()) }
func (t *tunable) Set(d time.Duration) { t.ns.Store(int64(d)) }

// enqueueSpec describes a job to queue.
type enqueueSpec struct {
	UserID      int64
	Kind        string
	Origin      string
	Interactive bool   // somebody waits for it: it needs a running worker, and goes first
	Dedupe      string // at most one active job per key; empty: a key made from the kind and the user
	Payload     any    // JSON-encoded parameters
	Expires     time.Duration
	// Timeout raises the hard limit of an attempt above the kind's setting (a sync of many days
	// needs more time); it never lowers it, and never goes past the longest an administrator may set.
	Timeout time.Duration
}

// enqueueJob queues a job under the settings of its kind. When an active job with the same key
// exists, that one is returned and created is false. It refuses with errKindPaused when the kind is
// off, and with errNoWorker for an interactive job when no worker of the kind is running, so a
// person is told at once instead of waiting for nothing.
func (s *Backend) enqueueJob(ctx context.Context, e enqueueSpec) (job models.Job, created bool, err error) {
	spec, ok := s.kinds[e.Kind]
	if !ok {
		return models.Job{}, false, fmt.Errorf("unknown kind of job %q", e.Kind)
	}
	ks := s.cfg().workers.Kinds[e.Kind]
	if !ks.Enabled {
		return models.Job{}, false, errKindPaused
	}
	if e.Interactive {
		online, err := s.workerOnline(ctx, e.Kind)
		if err != nil {
			return models.Job{}, false, err
		}
		if !online {
			return models.Job{}, false, errNoWorker
		}
	}
	payload, err := encodeJSON(e.Payload)
	if err != nil {
		return models.Job{}, false, err
	}
	key := e.Dedupe
	if key == "" {
		key = fmt.Sprintf("%s:%d", spec.Kind, e.UserID)
	}
	n := models.NewJob{
		UserID: e.UserID, Kind: e.Kind, Origin: e.Origin, Interactive: e.Interactive, DedupeKey: key,
		Payload: payload, MaxAttempts: ks.MaxAttempts, Timeout: ks.Timeout,
	}
	if e.Timeout > n.Timeout {
		n.Timeout = min(e.Timeout, maxJobTimeoutSeconds*time.Second)
	}
	if e.Expires > 0 {
		t := time.Now().Add(e.Expires)
		n.ExpiresAt = &t
	}
	return s.store.EnqueueJob(ctx, n)
}

// workerOnline reports whether a worker that runs the kind asked for work recently.
func (s *Backend) workerOnline(ctx context.Context, kind string) (bool, error) {
	ws, err := s.store.ListWorkers(ctx, time.Now().Add(-workerSeen.Get()))
	if err != nil {
		return false, err
	}
	for _, w := range ws {
		for _, k := range w.Kinds {
			if k == kind {
				return true, nil
			}
		}
	}
	return false, nil
}

// waitJob waits up to wait for a job to end and returns it as it is then (still active when the wait
// ran out). It reads the database, so it works whichever backend a worker reported to.
func (s *Backend) waitJob(ctx context.Context, publicID string, wait time.Duration) (models.Job, error) {
	deadline := time.Now().Add(wait)
	for {
		j, err := s.store.GetJob(ctx, publicID)
		if err != nil || !models.JobActive(j.State) || !time.Now().Before(deadline) {
			return j, err
		}
		select {
		case <-ctx.Done():
			return j, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// kindBackoff is the wait before a job of its kind runs again after attempts failed attempts.
func (s *Backend) kindBackoff(j models.Job) time.Duration {
	return s.cfg().workers.Kinds[j.Kind].backoff(j.Attempts)
}

// maintainQueue ends leases that ran out and queued jobs that waited too long. Every worker request
// that waits for work calls it, so it happens even when no maintenance backend runs; at most one
// sweep per maintainEvery is done in this process.
func (s *Backend) maintainQueue(ctx context.Context) {
	last := s.lastSweep.Load()
	now := time.Now()
	if now.UnixNano()-last < int64(maintainEvery.Get()) || !s.lastSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	reaped, err := s.store.ReapExpiredLeases(ctx, now, s.kindBackoff)
	if err != nil {
		if ctx.Err() == nil {
			log.Error().Err(err).Msg("end expired leases")
		}
		return
	}
	for _, j := range reaped {
		log.Warn().Str("job", j.PublicID).Str("kind", j.Kind).Int("attempt", j.Attempts).Str("state", j.State).Str("reason", j.Error).Msg("a worker lost its job")
		s.afterJob(ctx, j)
	}
	if _, err := s.store.ExpireQueuedJobs(ctx, now); err != nil && ctx.Err() == nil {
		log.Error().Err(err).Msg("expire queued jobs")
	}
}

// clip shortens text from a worker before it is stored or shown.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// createUser and createUserWithIdentity create an account and give it the default job priority an
// administrator set, if that differs from the built-in one.
func (s *Backend) createUser(ctx context.Context, name string) (models.User, error) {
	u, err := s.store.CreateUser(ctx, name)
	if err == nil {
		s.applyDefaultPriority(ctx, &u)
	}
	return u, err
}

func (s *Backend) createUserWithIdentity(ctx context.Context, name, email string, id models.Identity) (models.User, error) {
	u, err := s.store.CreateUserWithIdentity(ctx, name, email, id)
	if err == nil {
		s.applyDefaultPriority(ctx, &u)
	}
	return u, err
}

func (s *Backend) applyDefaultPriority(ctx context.Context, u *models.User) {
	p := s.cfg().workers.DefaultPriority
	if p == 0 || p == u.JobPriority {
		return
	}
	if err := s.store.SetUserJobPriority(ctx, u.ID, p); err != nil {
		log.Warn().Err(err).Msg("could not give the new account the default job priority")
		return
	}
	u.JobPriority = p
}
