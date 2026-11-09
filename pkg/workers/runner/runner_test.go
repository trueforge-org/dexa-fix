package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/workerapi"
)

// fakeBackend serves the worker endpoints with one job to hand out.
type fakeBackend struct {
	t *testing.T

	mu         sync.Mutex
	job        *workerapi.Lease // handed out once
	cancel     bool             // heartbeats answer cancel
	lost       bool             // heartbeats answer 409
	completed  chan workerapi.Completion
	released   chan struct{}
	heartbeats int
	progress   []string
	creds      []string
	badSecret  bool
}

func newFake(t *testing.T, kind string) (*fakeBackend, *workerapi.Client) {
	f := &fakeBackend{t: t, completed: make(chan workerapi.Completion, 4), released: make(chan struct{}, 4)}
	f.job = &workerapi.Lease{
		Job:            workerapi.Job{ID: "job1", Kind: kind, UserID: 7, Attempt: 1, MaxAttempts: 3, Payload: json.RawMessage(`{"x":1}`)},
		Token:          "lease-token",
		TimeoutSeconds: 30,
		LeaseSeconds:   3, // heartbeat every second
	}
	mux := http.NewServeMux()
	p := workerapi.Prefix
	cfg := workerapi.Config{Generation: 1, LeaseSeconds: 3, Kinds: map[string]workerapi.KindConfig{kind: {Enabled: true, Options: map[string]string{"delay_ms": "40"}}}}
	mux.HandleFunc("POST "+p+"/register", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(cfg) })
	mux.HandleFunc("GET "+p+"/config", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(cfg) })
	mux.HandleFunc("POST "+p+"/lease", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		j := f.job
		f.job = nil
		f.mu.Unlock()
		if j == nil {
			time.Sleep(50 * time.Millisecond)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		json.NewEncoder(w).Encode(j)
	})
	token := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get(workerapi.HeaderLease) != "lease-token" {
			http.Error(w, "wrong token", http.StatusConflict)
			return false
		}
		return true
	}
	mux.HandleFunc("POST "+p+"/jobs/{id}/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		if !token(w, r) {
			return
		}
		var hb workerapi.Heartbeat
		json.NewDecoder(r.Body).Decode(&hb)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.heartbeats++
		if len(hb.Progress) > 0 {
			f.progress = append(f.progress, string(hb.Progress))
		}
		if f.lost {
			http.Error(w, "lost", http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(workerapi.HeartbeatAnswer{Cancel: f.cancel})
	})
	mux.HandleFunc("POST "+p+"/jobs/{id}/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !token(w, r) {
			return
		}
		var c workerapi.Credentials
		json.NewDecoder(r.Body).Decode(&c)
		f.mu.Lock()
		f.creds = append(f.creds, string(c.Data))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+p+"/jobs/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		if !token(w, r) {
			return
		}
		var c workerapi.Completion
		json.NewDecoder(r.Body).Decode(&c)
		f.completed <- c
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+p+"/jobs/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		if !token(w, r) {
			return
		}
		f.released <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(workerapi.HeaderWorker) != "secret" {
			http.Error(w, `{"status":401,"code":"unauthorized","message":"no"}`, http.StatusUnauthorized)
			return
		}
		if r.URL.Path == p+"/healthz" {
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := workerapi.New(srv.URL, nil, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

// start runs a Runner with one handler until the test ends or stop is called.
func start(t *testing.T, c *workerapi.Client, kind string, h Handler) (stop func()) {
	t.Helper()
	r := New(c, Options{ID: "w1", Concurrency: 1, Poll: 100 * time.Millisecond, ConfigEvery: time.Hour})
	r.Handle(kind, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return stop
}

func waitComp(t *testing.T, f *fakeBackend) workerapi.Completion {
	t.Helper()
	select {
	case c := <-f.completed:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no completion reported")
		return workerapi.Completion{}
	}
}

func TestRunnerSuccess(t *testing.T) {
	f, c := newFake(t, "demo.import")
	start(t, c, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		if j.UserID != 7 || j.Attempt != 1 || string(j.Payload) != `{"x":1}` {
			t.Errorf("job: %+v", j.Job)
		}
		if d := j.Config.OptionMillis("delay_ms", 0); d != 40*time.Millisecond {
			t.Errorf("option: %v", d)
		}
		if err := j.SaveCredentials(ctx, map[string]string{"tok": "new"}); err != nil {
			t.Errorf("credentials: %v", err)
		}
		j.Progress(map[string]int{"done": 2})
		time.Sleep(1300 * time.Millisecond) // long enough for a heartbeat to carry the progress
		return map[string]int{"n": 5}, nil
	})
	got := waitComp(t, f)
	if got.Outcome != models.OutcomeOK || string(got.Result) != `{"n":5}` {
		t.Fatalf("completion: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.creds) != 1 || f.creds[0] != `{"tok":"new"}` {
		t.Fatalf("credentials: %v", f.creds)
	}
	if f.heartbeats == 0 || len(f.progress) == 0 || f.progress[0] != `{"done":2}` {
		t.Fatalf("heartbeats %d progress %v", f.heartbeats, f.progress)
	}
}

func TestRunnerClassifiesErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		outcome string
		after   int
		text    string
	}{
		{"permanent", Permanent(errors.New("wrong password")), models.OutcomeFail, 0, "wrong password"},
		{"retry after", Retryable(errors.New("slow down"), 90*time.Second), models.OutcomeRetry, 90, "slow down"},
		{"plain error is temporary", errors.New("connection reset"), models.OutcomeRetry, 0, "connection reset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFake(t, "demo.import")
			start(t, c, "demo.import", func(context.Context, *Job) (any, error) { return nil, tc.err })
			got := waitComp(t, f)
			if got.Outcome != tc.outcome || got.RetryAfterSeconds != tc.after || got.Error != tc.text {
				t.Fatalf("completion: %+v", got)
			}
		})
	}
}

func TestRunnerPanicIsARetryWithoutLeakingTheMessage(t *testing.T) {
	f, c := newFake(t, "demo.import")
	start(t, c, "demo.import", func(context.Context, *Job) (any, error) { panic("password=hunter2") })
	got := waitComp(t, f)
	if got.Outcome != models.OutcomeRetry || strings.Contains(got.Error, "hunter2") || got.Error == "" {
		t.Fatalf("completion: %+v", got)
	}
}

func TestRunnerStopsOnCancel(t *testing.T) {
	f, c := newFake(t, "demo.import")
	f.cancel = true
	start(t, c, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		<-ctx.Done() // runs until the heartbeat says stop
		return nil, ctx.Err()
	})
	got := waitComp(t, f)
	if got.Outcome != models.OutcomeRetry || got.Error != "canceled" {
		t.Fatalf("completion: %+v", got)
	}
}

func TestRunnerDropsTheResultWhenTheLeaseIsLost(t *testing.T) {
	f, c := newFake(t, "demo.import")
	f.lost = true
	started := make(chan struct{})
	start(t, c, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		close(started)
		<-ctx.Done()
		return "late result", nil
	})
	<-started
	select {
	case got := <-f.completed:
		t.Fatalf("a worker that lost its lease reported %+v", got)
	case <-time.After(2500 * time.Millisecond):
	}
}

func TestRunnerEnforcesTheTimeLimit(t *testing.T) {
	f, c := newFake(t, "demo.import")
	f.job.TimeoutSeconds = 1
	start(t, c, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	got := waitComp(t, f)
	if got.Outcome != models.OutcomeRetry || !strings.Contains(got.Error, "time limit") {
		t.Fatalf("completion: %+v", got)
	}
}

func TestRunnerReleasesTheJobOnShutdown(t *testing.T) {
	f, c := newFake(t, "demo.import")
	started := make(chan struct{})
	stop := start(t, c, "demo.import", func(ctx context.Context, j *Job) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	<-started
	stop()
	select {
	case <-f.released:
	case <-time.After(5 * time.Second):
		t.Fatal("the job was not given back")
	}
	select {
	case got := <-f.completed:
		t.Fatalf("a shutdown was reported as %+v", got)
	default:
	}
}

func TestRunnerRefusedSecret(t *testing.T) {
	_, c := newFake(t, "demo.import")
	bad, _ := workerapi.New("http://127.0.0.1:1", nil, "x")
	_ = bad
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"status":401,"code":"unauthorized","message":"no"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	wc, _ := workerapi.New(srv.URL, nil, "wrong")
	r := New(wc, Options{})
	r.Handle("demo.import", func(context.Context, *Job) (any, error) { return nil, nil })
	done := make(chan error, 1)
	go func() { done <- r.Run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refused secret should stop the worker with an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker kept retrying a refused secret")
	}
	_ = c
}

func TestRunnerNeedsHandlers(t *testing.T) {
	_, c := newFake(t, "demo.import")
	if err := New(c, Options{}).Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
}
