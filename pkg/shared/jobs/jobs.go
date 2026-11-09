// Package jobs tracks the background jobs of one half of the application.
package jobs

import (
	"context"
	"sync"
)

// Group tracks background jobs. Stopping it cancels their context and waits for them.
type Group struct {
	// Ctx is the parent of the jobs; Stop cancels it.
	Ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex // guards done and the Add in Start against Stop's Wait
	done   bool
}

// New returns a running group.
func New() *Group {
	g := &Group{}
	g.Ctx, g.cancel = context.WithCancel(context.Background())
	return g
}

// Start runs f in the background, tracked by Stop. It reports false, without running f, once the
// group is stopping.
func (g *Group) Start(f func()) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.done {
		return false
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		f()
	}()
	return true
}

// Stop cancels the jobs and waits for them to finish.
func (g *Group) Stop() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
	g.cancel()
	g.wg.Wait()
}
