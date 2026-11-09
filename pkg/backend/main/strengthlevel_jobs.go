package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

const (
	slWriteEvery = time.Second      // progress is written to the database at most this often
	slHeartbeat  = 30 * time.Second // a running job refreshes its row at least this often
	slStale      = 2 * time.Minute  // a running state not refreshed for this long belongs to a dead process
)

// slState is the state of one user's Strength Level sync, which runs in the background. It is
// stored in the database as JSON, so any backend can show it. Only the latest job of a user is kept.
type slState struct {
	Running           bool
	Started, Finished time.Time
	Days              int
	Update            bool
	Done, Total       int
	Added, Updated    int
	Unchanged, Failed int
	Err               string
	Canceled          bool
}

// slJob is a sync that runs on this backend: its state plus what only the running process has.
type slJob struct {
	slState
	userID    int64
	cancel    context.CancelFunc
	lastWrite time.Time
}

// slJobs holds the sync jobs this backend runs and writes their state to the database.
type slJobs struct {
	store store.Store
	clock func() time.Time // the time, for tests; nil means time.Now
	mu    sync.Mutex
	jobs  map[int64]*slJob
}

// write stores the job's state. Callers hold the lock. A failure is logged: the sync goes on.
func (j *slJobs) write(job *slJob) {
	data, err := json.Marshal(job.slState)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err = j.store.PutStrengthLevelJob(ctx, job.userID, string(data))
	}
	if err != nil {
		log.Warn().Err(err).Int64("user", job.userID).Msg("store the Strength Level sync status")
	}
	job.lastWrite = time.Now()
}

// begin registers a new running job, or reports false when the user already has one running, on
// this backend or on another one (its state is fresh in the database).
func (j *slJobs) begin(ctx context.Context, userID int64, days int, update bool, cancel context.CancelFunc) (*slJob, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if old := j.jobs[userID]; old != nil && old.Running {
		return nil, false, nil
	}
	if st, err := j.read(ctx, userID); err != nil {
		return nil, false, err
	} else if st != nil && st.Running {
		return nil, false, nil
	}
	if j.jobs == nil {
		j.jobs = map[int64]*slJob{}
	}
	job := &slJob{slState: slState{Running: true, Started: time.Now().UTC(), Days: days, Update: update}, userID: userID, cancel: cancel}
	j.jobs[userID] = job
	j.write(job)
	return job, true, nil
}

func (j *slJobs) progress(job *slJob, done, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	first := job.Total == 0 && total > 0 // the page switches from "signing in" at once
	job.Done, job.Total = done, total
	if first || time.Since(job.lastWrite) >= slWriteEvery {
		j.write(job)
	}
}

// keepAlive refreshes the stored state of a running job until ctx ends or the job is finished, so
// that a long wait between two progress reports is not taken for a dead process.
func (j *slJobs) keepAlive(ctx context.Context, job *slJob) {
	t := time.NewTicker(slHeartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.mu.Lock()
			running := job.Running
			if running {
				j.write(job)
			}
			j.mu.Unlock()
			if !running {
				return
			}
		}
	}
}

// finish records the outcome of a job and marks it as no longer running.
func (j *slJobs) finish(job *slJob, f func(*slState)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f(&job.slState)
	job.Running = false
	job.Finished = time.Now().UTC()
	j.write(job)
	job.cancel()
}

// stop cancels the user's job if it runs on this backend.
func (j *slJobs) stop(userID int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job := j.jobs[userID]; job != nil && job.Running {
		job.Canceled = true
		job.cancel()
	}
}

// read returns the stored state of the user's latest job, or nil when there has been none. A
// running state that was not refreshed for slStale is reported as interrupted: the backend that
// ran it is gone.
func (j *slJobs) read(ctx context.Context, userID int64) (*slState, error) {
	raw, at, err := j.store.GetStrengthLevelJob(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st slState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return nil, fmt.Errorf("stored Strength Level sync status: %w", err)
	}
	now := time.Now
	if j.clock != nil {
		now = j.clock
	}
	if st.Running && now().Sub(at) > slStale {
		st.Running = false
		st.Err = "the server that ran it stopped before it finished. Start the sync again"
		st.Finished = at
	}
	return &st, nil
}

// view describes the user's latest job, or nil when there has been none.
func (j *slJobs) view(ctx context.Context, userID int64) (*slJobView, error) {
	st, err := j.read(ctx, userID)
	if st == nil || err != nil {
		return nil, err
	}
	v := &slJobView{Running: st.Running, Done: st.Done, Total: st.Total}
	switch {
	case st.Running && st.Total == 0:
		v.Text = "Sync running: signing in…"
	case st.Running:
		v.Text = fmt.Sprintf("Sync running: %d of %d days checked. You can leave this page.", st.Done, st.Total)
	case st.Canceled:
		v.Text = "The last sync was canceled."
	case st.Err != "":
		v.Text = "The last sync stopped: " + st.Err
	default:
		v.Text = fmt.Sprintf("Last sync (%s UTC, last %d days%s): added %d, updated %d, %d already up to date",
			st.Finished.Format("2006-01-02 15:04"), st.Days, map[bool]string{true: ", updating existing days"}[st.Update],
			st.Added, st.Updated, st.Unchanged)
		if st.Failed > 0 {
			v.Text += fmt.Sprintf(", %d could not be written", st.Failed)
		}
		v.Text += "."
	}
	return v, nil
}
