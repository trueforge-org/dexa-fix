package web

import (
	"context"
	"sync"
)

// jobGroup tracks the background jobs of one half of the application. Stopping it cancels their
// context and waits for them.
type jobGroup struct {
	jobCtx    context.Context // parent of the jobs; cancelled by stopJobs
	cancelJob context.CancelFunc
	jobs      sync.WaitGroup
	jobsMu    sync.Mutex // guards jobsDone and the Add in startJob against stopJobs' Wait
	jobsDone  bool
}

func newJobGroup() *jobGroup {
	g := &jobGroup{}
	g.jobCtx, g.cancelJob = context.WithCancel(context.Background())
	return g
}

// startJob runs f in the background, tracked by stopJobs. It reports false, without running f,
// once the group is stopping.
func (g *jobGroup) startJob(f func()) bool {
	g.jobsMu.Lock()
	defer g.jobsMu.Unlock()
	if g.jobsDone {
		return false
	}
	g.jobs.Add(1)
	go func() {
		defer g.jobs.Done()
		f()
	}()
	return true
}

// stopJobs cancels the jobs and waits for them to finish.
func (g *jobGroup) stopJobs() {
	g.jobsMu.Lock()
	g.jobsDone = true
	g.jobsMu.Unlock()
	g.cancelJob()
	g.jobs.Wait()
}
