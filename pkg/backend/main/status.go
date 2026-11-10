package mainbackend

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
)

// The status of the deployment as the backend sees it: itself, the database, the body service and
// the workers. The web process adds itself. Everybody may see the summary through the frontend's
// /status page, so it holds a state per component and nothing else; administrators get the detail
// (worker names, versions, the reason a part is down) from a separate endpoint.

// statusKeep is how long an answer is kept, so that a flood of /status requests costs one round of
// checks per few seconds, not one per request. Tests set it to zero.
var statusKeep = 5 * time.Second

const (
	// statusTimeout bounds each check: a part that hangs is down.
	statusTimeout = 2 * time.Second
	// workersHistory is how long a worker that has stopped asking is still known; after that the
	// workers of a provider count as not in use rather than as down.
	workersHistory = 24 * time.Hour
)

// pinger is what a store offers to say that it answers. The SQL store has it.
type pinger interface{ Ping(context.Context) error }

// pingDB checks the database with a bound on the time it may take.
func (s *Backend) pingDB(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	if p, ok := s.store.(pinger); ok {
		return p.Ping(ctx)
	}
	_, err := s.store.HasUsers(ctx)
	return err
}

// Ready reports whether the backend can do its work: the database answers. It is the readiness
// check of the health listener. The body service and the workers are not part of it: the pages say
// so themselves when the body service is down, and everything else keeps working.
func (s *Backend) Ready(ctx context.Context) error { return s.pingDB(ctx) }

type statusSnapshot struct {
	at         time.Time
	components []apiclient.StatusComponent
	notes      map[string]string
	workers    []apiclient.StatusWorker
}

type statusCache struct {
	mu   sync.Mutex
	snap *statusSnapshot
}

// statusNow returns the current snapshot, checking again if the last one is older than statusKeep.
// Checks run while the lock is held, so concurrent requests share one round.
func (s *Backend) statusNow(ctx context.Context) *statusSnapshot {
	s.statusCache.mu.Lock()
	defer s.statusCache.mu.Unlock()
	if sn := s.statusCache.snap; sn != nil && time.Since(sn.at) < statusKeep {
		return sn
	}
	s.statusCache.snap = s.checkStatus(ctx)
	return s.statusCache.snap
}

func (s *Backend) checkStatus(ctx context.Context) *statusSnapshot {
	now := time.Now()
	sn := &statusSnapshot{at: now, notes: map[string]string{}}
	add := func(name, label, state, note string) {
		sn.components = append(sn.components, apiclient.StatusComponent{Name: name, Label: label, State: state})
		if note != "" {
			sn.notes[name] = note
		}
	}
	add("api", "API", apiclient.StateUp, "")

	if err := s.pingDB(ctx); err != nil {
		add("database", "Database", apiclient.StateDown, err.Error())
	} else {
		add("database", "Database", apiclient.StateUp, "")
	}

	if s.state != nil {
		// Down means sign-ins are refused: the counters fail closed. Pages that need no sign-in work.
		sctx, cancel := context.WithTimeout(ctx, statusTimeout)
		if err := s.state.Ping(sctx); err != nil {
			add("valkey", "Valkey (sign-in counters, single-use values)", apiclient.StateDown, err.Error())
		} else {
			add("valkey", "Valkey (sign-in counters, single-use values)", apiclient.StateUp, "")
		}
		cancel()
	}

	if s.body.c != nil {
		bctx, cancel := context.WithTimeout(ctx, statusTimeout)
		if err := s.body.health(bctx); err != nil {
			add("body", "Body service (readings, scans, estimates)", apiclient.StateDown, err.Error())
		} else {
			add("body", "Body service (readings, scans, estimates)", apiclient.StateUp, "")
		}
		cancel()
	}

	instances, err := s.store.ListWorkers(ctx, now.Add(-workersHistory))
	if err != nil {
		add("workers", "Workers", apiclient.StateDown, err.Error())
		return sn
	}
	liveSince := now.Add(-workerSeen.Get())
	for _, in := range instances {
		sn.workers = append(sn.workers, apiclient.StatusWorker{
			ID: in.ID, Version: in.Version, Kinds: in.Kinds,
			LastSeen: in.LastSeen.UTC().Format("2006-01-02 15:04:05 UTC"), Live: !in.LastSeen.Before(liveSince),
		})
	}
	sort.Slice(sn.workers, func(i, j int) bool { return sn.workers[i].ID < sn.workers[j].ID })
	for _, p := range s.providers {
		kinds := map[string]bool{}
		for _, k := range p.Kinds {
			kinds[k.Kind] = true
		}
		// A worker process gets a new random name at every start, so the ones of containers that
		// were replaced stay on record for a day. They are not a fault, which is why "some of them
		// are silent" is not a state: a provider's workers are up when one asks for work.
		var known, live int
		for _, in := range instances {
			for _, k := range in.Kinds {
				if kinds[k] {
					known++
					if !in.LastSeen.Before(liveSince) {
						live++
					}
					break
				}
			}
		}
		name, label := "workers."+p.Name, p.Label+" workers"
		switch {
		case live > 0:
			add(name, label, apiclient.StateUp, "")
		case known > 0:
			add(name, label, apiclient.StateDown, "workers of this provider registered, but none asked for work lately")
		default:
			add(name, label, apiclient.StateOff, "no worker of this provider has registered in the last day")
		}
	}
	return sn
}

func (sn *statusSnapshot) summary() apiclient.Status {
	return apiclient.Status{CheckedAt: sn.at.UTC().Format(time.RFC3339), Components: append([]apiclient.StatusComponent(nil), sn.components...)}
}

func (s *Backend) internalStatusRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+apiclient.Prefix+"/status", s.internalStatus)
	mux.HandleFunc("GET "+apiclient.Prefix+"/admin/status", s.internalAdmin(s.internalAdminStatus))
}

// internalStatus is the summary: the same for everybody, whoever is signed in.
func (s *Backend) internalStatus(w http.ResponseWriter, r *http.Request) {
	internalJSON(w, http.StatusOK, s.statusNow(r.Context()).summary())
}

// internalAdminStatus adds the detail for administrators.
func (s *Backend) internalAdminStatus(w http.ResponseWriter, r *http.Request, _ models.User) {
	sn := s.statusNow(r.Context())
	d := apiclient.StatusDetail{Status: sn.summary(), Notes: map[string]string{}, Workers: append([]apiclient.StatusWorker(nil), sn.workers...)}
	for k, v := range sn.notes {
		d.Notes[k] = v
	}
	internalJSON(w, http.StatusOK, d)
}
