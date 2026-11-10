package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
)

// faultBackend is a backend whose every endpoint can be replaced by a failing one. over maps an
// endpoint name (register, config, lease, heartbeat, credentials, complete, release) to a handler
// that is told which call this is (1 for the first).
type faultBackend struct {
	mu       sync.Mutex
	cfg      workerapi.Config
	lease    *workerapi.Lease // handed out once
	over     map[string]func(w http.ResponseWriter, r *http.Request, n int)
	hits     map[string]int
	progress []string
	done     chan workerapi.Completion
}

func newFault(t *testing.T, kind string) (*faultBackend, *workerapi.Client) {
	t.Helper()
	f := &faultBackend{
		cfg: workerapi.Config{Generation: 1, LeaseSeconds: 3, Kinds: map[string]workerapi.KindConfig{kind: {Enabled: true}}},
		lease: &workerapi.Lease{
			Job:            workerapi.Job{ID: "job1", Kind: kind, UserID: 7, Attempt: 1, MaxAttempts: 3},
			Token:          "lease-token",
			TimeoutSeconds: 30,
			LeaseSeconds:   3,
		},
		over: map[string]func(http.ResponseWriter, *http.Request, int){},
		hits: map[string]int{},
		done: make(chan workerapi.Completion, 8),
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	c, err := workerapi.New(srv.URL, nil, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func (f *faultBackend) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, workerapi.Prefix+"/"), "/")
	key := parts[len(parts)-1]
	f.mu.Lock()
	f.hits[key]++
	n := f.hits[key]
	over := f.over[key]
	cfg := f.cfg
	lease := f.lease
	if key == "lease" {
		f.lease = nil
	}
	f.mu.Unlock()
	if over != nil {
		over(w, r, n)
		return
	}
	switch key {
	case "register", "config":
		json.NewEncoder(w).Encode(cfg)
	case "lease":
		if lease == nil {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(lease)
	case "heartbeat":
		var hb workerapi.Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		if len(hb.Progress) > 0 {
			f.mu.Lock()
			f.progress = append(f.progress, string(hb.Progress))
			f.mu.Unlock()
		}
		json.NewEncoder(w).Encode(workerapi.HeartbeatAnswer{})
	case "complete":
		var c workerapi.Completion
		json.NewDecoder(r.Body).Decode(&c)
		f.done <- c
		w.WriteHeader(http.StatusNoContent)
	default: // credentials, release
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *faultBackend) hitCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[key]
}

func (f *faultBackend) fail(key string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.over[key] = func(w http.ResponseWriter, r *http.Request, n int) {
		http.Error(w, "refused", status)
	}
}

func (f *faultBackend) completion(t *testing.T) workerapi.Completion {
	t.Helper()
	select {
	case c := <-f.done:
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("no completion reported")
		return workerapi.Completion{}
	}
}

// startRunner runs a Runner until stop is called (or the test ends).
func startRunner(t *testing.T, c *workerapi.Client, opt Options, kind string, h Handler) (r *Runner, stop func()) {
	t.Helper()
	if opt.Poll == 0 {
		opt.Poll = 100 * time.Millisecond
	}
	if opt.ConfigEvery == 0 {
		opt.ConfigEvery = time.Hour
	}
	opt.ID, opt.Concurrency = "w1", 1
	r = New(c, opt)
	r.Handle(kind, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return r, stop
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// logCapture replaces the global logger for one test (do not combine with t.Parallel).
type logCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logCapture) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.b.String(), s)
}

// captureLog must be called before the runner starts, so that it is restored after the runner stopped.
func captureLog(t *testing.T) *logCapture {
	t.Helper()
	lc := &logCapture{}
	old := log.Logger
	log.Logger = zerolog.New(lc)
	t.Cleanup(func() { log.Logger = old })
	return lc
}

func idle(context.Context, *Job) (any, error) { return nil, nil }

func TestErrorClassifiersAnswerForUnmarkedAndNilErrors(t *testing.T) {
	if Permanent(nil) != nil || Retryable(nil, time.Second) != nil {
		t.Error("marking a nil error must stay nil")
	}
	plain := errors.New("plain")
	if IsPermanent(plain) {
		t.Error("an unmarked error is not permanent")
	}
	if ok, after := IsRetryable(plain); ok || after != 0 {
		t.Errorf("an unmarked error is not marked retryable: %v %v", ok, after)
	}
	if ok, after := IsRetryable(Retryable(plain, 3*time.Second)); !ok || after != 3*time.Second {
		t.Errorf("retryable: %v %v", ok, after)
	}
	if IsPermanent(Retryable(plain, 0)) {
		t.Error("a retryable error is not permanent")
	}
}

func TestRunnerReportsTheNameItRegisteredUnder(t *testing.T) {
	_, c := newFault(t, "demo.import")
	if got := New(c, Options{ID: "worker-a"}).ID(); got != "worker-a" {
		t.Fatalf("ID = %q", got)
	}
}

func TestBackoffEndsAtOnceWhenTheContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if backoff(ctx, time.Minute) {
		t.Fatal("a cancelled context must stop the wait")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait was not cut short")
	}
}

func TestJobRefusesCredentialsThatCannotBeEncoded(t *testing.T) {
	f, c := newFault(t, "demo.import")
	startRunner(t, c, Options{}, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		if err := j.SaveCredentials(ctx, make(chan int)); err == nil {
			t.Error("credentials that cannot be encoded must be refused")
		}
		return nil, nil
	})
	if got := f.completion(t); got.Outcome != models.OutcomeOK {
		t.Fatalf("completion: %+v", got)
	}
	if n := f.hitCount("credentials"); n != 0 {
		t.Fatalf("nothing should have been sent to the backend, got %d calls", n)
	}
}

func TestRunnerWaitsForABackendThatIsStillStarting(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	f.over["register"] = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(f.cfg)
	}
	r, _ := startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "the worker to register on its second try", func() bool { return r.Ready(time.Minute) == nil })
	if n := f.hitCount("register"); n != 2 {
		t.Fatalf("register was called %d times, want 2", n)
	}
}

func TestRunnerStopsWaitingForTheBackendWhenItIsShutDown(t *testing.T) {
	lc := captureLog(t)
	f, c := newFault(t, "demo.import")
	f.fail("register", http.StatusServiceUnavailable)
	r := New(c, Options{})
	r.Handle("demo.import", idle)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	t.Cleanup(cancel)
	waitUntil(t, "the worker to say it is waiting", func() bool { return lc.has("backend not reachable") })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a shutdown while waiting is not an error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker kept waiting after the shutdown")
	}
}

func TestRunnerStopsAskingForJobsWhenItIsShutDownAfterAFailedAsk(t *testing.T) {
	lc := captureLog(t)
	f, c := newFault(t, "demo.import")
	f.fail("lease", http.StatusInternalServerError)
	_, stop := startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "the failed ask to be logged", func() bool { return lc.has("could not ask for a job") })
	stop() // returns only when the loop ended, so the backoff was cut short
}

func TestRunnerIdlesWhileEveryKindIsOffAndStopsOnShutdown(t *testing.T) {
	f, c := newFault(t, "demo.import")
	f.cfg.Kinds["demo.import"] = workerapi.KindConfig{Enabled: false}
	_, stop := startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "the worker to register", func() bool { return f.hitCount("register") == 1 })
	stop()
	if n := f.hitCount("lease"); n != 0 {
		t.Fatalf("a worker with every kind off asked for %d jobs", n)
	}
}

func TestRunnerAsksAgainAfterAFailedAsk(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	f.over["lease"] = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "a second ask after the pause", func() bool { return f.hitCount("lease") >= 2 })
}

func TestRunnerThatIsShutDownBeforeTheBackendAnswersEndsWithoutError(t *testing.T) {
	f, c := newFault(t, "demo.import")
	f.fail("register", http.StatusServiceUnavailable)
	r := New(c, Options{})
	r.Handle("demo.import", idle)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("a shutdown is not an error: %v", err)
	}
}

func TestRunnerStartsWorkingWhenTheAdministratorTurnsAKindOn(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	f.cfg.Kinds["demo.import"] = workerapi.KindConfig{Enabled: false}
	f.over["config"] = func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 { // a hiccup: the last configuration stays
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		cfg := workerapi.Config{Generation: 2, Kinds: map[string]workerapi.KindConfig{"demo.import": {Enabled: true}}}
		json.NewEncoder(w).Encode(cfg)
	}
	startRunner(t, c, Options{ConfigEvery: 50 * time.Millisecond}, "demo.import", idle)
	if got := f.completion(t); got.Outcome != models.OutcomeOK {
		t.Fatalf("completion: %+v", got)
	}
	if n := f.hitCount("config"); n < 2 {
		t.Fatalf("the configuration was read %d times", n)
	}
}

func TestRunnerRefusesAJobOfAKindItDoesNotHandle(t *testing.T) {
	f, c := newFault(t, "demo.import")
	f.lease.Job.Kind = "other.kind"
	startRunner(t, c, Options{}, "demo.import", func(context.Context, *Job) (any, error) {
		t.Error("the handler must not run for another kind")
		return nil, nil
	})
	got := f.completion(t)
	if got.Outcome != models.OutcomeFail || got.Error != "this worker cannot run jobs of kind other.kind" {
		t.Fatalf("completion: %+v", got)
	}
}

func TestRunnerCopesWithALeaseThatNamesNoTimings(t *testing.T) {
	for name, set := range map[string]func(*workerapi.Lease){
		"no time limit and no lease length": func(l *workerapi.Lease) { l.TimeoutSeconds, l.LeaseSeconds = 0, 0 },
		"a very short lease":                func(l *workerapi.Lease) { l.LeaseSeconds = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			f, c := newFault(t, "demo.import")
			set(f.lease)
			startRunner(t, c, Options{}, "demo.import", idle)
			if got := f.completion(t); got.Outcome != models.OutcomeOK {
				t.Fatalf("completion: %+v", got)
			}
		})
	}
}

func TestRunnerSendsProgressOnlyOnceUntilItChanges(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	startRunner(t, c, Options{}, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		j.Progress(map[string]int{"done": 1})
		deadline := time.Now().Add(10 * time.Second)
		for f.hitCount("heartbeat") < 2 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		return nil, nil
	})
	f.completion(t)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.progress) != 1 || f.progress[0] != `{"done":1}` {
		t.Fatalf("progress sent: %v", f.progress)
	}
}

func TestRunnerGivesUpAJobWhenTheBackendStaysUnreachableForALeaseLength(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	f.lease.LeaseSeconds = 1
	f.fail("heartbeat", http.StatusServiceUnavailable)
	stopped := make(chan struct{})
	_, stop := startRunner(t, c, Options{}, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		<-ctx.Done()
		close(stopped)
		return "late result", nil
	})
	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the handler was never stopped")
	}
	stop()
	if n := f.hitCount("complete"); n != 0 {
		t.Fatalf("a job whose lease may be gone was reported %d times", n)
	}
}

func TestRunnerLogsWhenTheBackendWillNotTakeAJobBackOnShutdown(t *testing.T) {
	lc := captureLog(t)
	f, c := newFault(t, "demo.import")
	f.fail("release", http.StatusInternalServerError)
	started := make(chan struct{})
	_, stop := startRunner(t, c, Options{}, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	stop()
	if !lc.has("could not give the job back") {
		t.Fatal("the failed release was not logged")
	}
}

func TestRunnerFailsAJobWhoseResultCannotBeEncoded(t *testing.T) {
	f, c := newFault(t, "demo.import")
	startRunner(t, c, Options{}, "demo.import", func(context.Context, *Job) (any, error) { return make(chan int), nil })
	got := f.completion(t)
	if got.Outcome != models.OutcomeFail || got.Error != "the result could not be encoded" {
		t.Fatalf("completion: %+v", got)
	}
}

func TestRunnerDoesNotRepeatAReportTheBackendRefused(t *testing.T) {
	lc := captureLog(t)
	f, c := newFault(t, "demo.import")
	f.fail("complete", http.StatusForbidden)
	_, stop := startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "the refusal to be logged", func() bool { return lc.has("the backend refused the report") })
	stop()
	if n := f.hitCount("complete"); n != 1 {
		t.Fatalf("a refused report was sent %d times", n)
	}
}

func TestRunnerTriesAReportFiveTimesAndThenLeavesItToTheBackendTimeout(t *testing.T) {
	t.Parallel()
	f, c := newFault(t, "demo.import")
	f.fail("complete", http.StatusServiceUnavailable)
	startRunner(t, c, Options{}, "demo.import", idle)
	waitUntil(t, "five report attempts", func() bool { return f.hitCount("complete") >= 5 })
	time.Sleep(200 * time.Millisecond)
	if n := f.hitCount("complete"); n != 5 {
		t.Fatalf("the report was tried %d times, want 5", n)
	}
}
