package mainbackend

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// slJob is the state of one user's Strength Level sync, which runs in the background. Only the
// latest job of a user is kept, in memory, so it is gone after a restart.
type slJob struct {
	running           bool
	started, finished time.Time
	days              int
	update            bool
	done, total       int
	added, updated    int
	unchanged, failed int
	err               string
	canceled          bool
	cancel            context.CancelFunc
}

// slJobs holds the latest sync job of each user.
type slJobs struct {
	mu   sync.Mutex
	jobs map[int64]*slJob
}

// begin registers a new running job, or reports false when the user already has one running.
func (j *slJobs) begin(userID int64, days int, update bool, cancel context.CancelFunc) (*slJob, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if old := j.jobs[userID]; old != nil && old.running {
		return nil, false
	}
	if j.jobs == nil {
		j.jobs = map[int64]*slJob{}
	}
	job := &slJob{running: true, started: time.Now().UTC(), days: days, update: update, cancel: cancel}
	j.jobs[userID] = job
	return job, true
}

func (j *slJobs) progress(job *slJob, done, total int) {
	j.mu.Lock()
	job.done, job.total = done, total
	j.mu.Unlock()
}

// finish records the outcome of a job and marks it as no longer running.
func (j *slJobs) finish(job *slJob, f func(*slJob)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f(job)
	job.running = false
	job.finished = time.Now().UTC()
	job.cancel()
}

// stop cancels the user's running job, if any.
func (j *slJobs) stop(userID int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job := j.jobs[userID]; job != nil && job.running {
		job.canceled = true
		job.cancel()
	}
}

// view describes the user's latest job, or nil when there has been none.
func (j *slJobs) view(userID int64) *slJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	job := j.jobs[userID]
	if job == nil {
		return nil
	}
	v := &slJobView{Running: job.running, Done: job.done, Total: job.total}
	switch {
	case job.running && job.total == 0:
		v.Text = "Sync running: signing in…"
	case job.running:
		v.Text = fmt.Sprintf("Sync running: %d of %d days checked. You can leave this page.", job.done, job.total)
	case job.canceled:
		v.Text = "The last sync was canceled."
	case job.err != "":
		v.Text = "The last sync stopped: " + job.err
	default:
		v.Text = fmt.Sprintf("Last sync (%s UTC, last %d days%s): added %d, updated %d, %d already up to date",
			job.finished.Format("2006-01-02 15:04"), job.days, map[bool]string{true: ", updating existing days"}[job.update],
			job.added, job.updated, job.unchanged)
		if job.failed > 0 {
			v.Text += fmt.Sprintf(", %d could not be written", job.failed)
		}
		v.Text += "."
	}
	return v
}
