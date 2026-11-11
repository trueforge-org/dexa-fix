package strengthlevel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

// runJob runs one job of the given kind through a real runner against a fake backend and returns
// how the attempt ended.
func runJob(t *testing.T, w *Worker, kind string, payload []byte) workerapi.Completion {
	t.Helper()
	done := make(chan workerapi.Completion, 1)
	lease := &workerapi.Lease{
		Job:            workerapi.Job{ID: "job1", Kind: kind, UserID: 7, Attempt: 1, MaxAttempts: 3, Payload: payload},
		Token:          "tok",
		TimeoutSeconds: 30,
		LeaseSeconds:   30,
	}
	cfg := workerapi.Config{Generation: 1, Kinds: map[string]workerapi.KindConfig{
		kind: {Enabled: true, Options: map[string]string{"request_delay_ms": "1"}},
	}}
	handed := false
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		switch parts[len(parts)-1] {
		case "register", "config":
			json.NewEncoder(rw).Encode(cfg)
		case "lease":
			if handed {
				time.Sleep(20 * time.Millisecond)
				rw.WriteHeader(http.StatusNoContent)
				return
			}
			handed = true
			json.NewEncoder(rw).Encode(lease)
		case "heartbeat":
			json.NewEncoder(rw).Encode(workerapi.HeartbeatAnswer{})
		case "complete":
			var c workerapi.Completion
			json.NewDecoder(r.Body).Decode(&c)
			done <- c
			rw.WriteHeader(http.StatusNoContent)
		default:
			rw.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := workerapi.New(srv.URL, nil, "secret")
	if err != nil {
		t.Fatal(err)
	}
	r := runner.New(client, runner.Options{ID: "w1", Concurrency: 1, Poll: 50 * time.Millisecond, ConfigEvery: time.Hour})
	for k, h := range w.Handlers() {
		r.Handle(k, h)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); r.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	select {
	case c := <-done:
		return c
	case <-time.After(15 * time.Second):
		t.Fatal("no completion reported")
		return workerapi.Completion{}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandlersRunEachKindThroughTheRunner(t *testing.T) {
	entries := []workerapi.ExportEntry{{Day: "2026-11-08", WeightKg: 74}, {Day: "2026-11-07", WeightKg: 73}}
	for _, tc := range []struct {
		name    string
		kind    string
		payload any
		check   func(t *testing.T, c workerapi.Completion)
	}{
		{"checking a login", KindCheckLogin, workerapi.CheckLoginPayload{Login: goodLogin}, func(t *testing.T, c workerapi.Completion) {
			if string(c.Result) != `{"login_ok":true}` {
				t.Fatalf("result %s", c.Result)
			}
		}},
		{"sending one day", KindDay, workerapi.ExportPayload{Login: goodLogin, Entries: entries[:1]}, func(t *testing.T, c workerapi.Completion) {
			var res workerapi.ExportResult
			json.Unmarshal(c.Result, &res)
			if res.Added != 1 {
				t.Fatalf("result %s", c.Result)
			}
		}},
		{"syncing two days", KindSync, workerapi.ExportPayload{Login: goodLogin, Entries: entries}, func(t *testing.T, c workerapi.Completion) {
			var res workerapi.ExportResult
			json.Unmarshal(c.Result, &res)
			if res.Added != 2 {
				t.Fatalf("result %s", c.Result)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, _ := newWorker(t)
			c := runJob(t, w, tc.kind, mustJSON(t, tc.payload))
			if c.Outcome != models.OutcomeOK {
				t.Fatalf("completion: %+v", c)
			}
			tc.check(t, c)
		})
	}
}

func TestHandlersRefuseAnUnreadablePayloadForGood(t *testing.T) {
	for _, kind := range []string{KindCheckLogin, KindDay, KindSync} {
		t.Run(kind, func(t *testing.T) {
			w, _, _ := newWorker(t)
			c := runJob(t, w, kind, []byte(`"not an object"`))
			if c.Outcome != models.OutcomeFail || c.Error != "unreadable job" {
				t.Fatalf("completion: %+v", c)
			}
		})
	}
}

// flaky wraps the fake site and breaks the requests fail names.
func flaky(t *testing.T, fail func(r *http.Request) bool, after func()) *Worker {
	t.Helper()
	f := &fake{entries: map[string]map[string]any{}}
	inner := f.handler()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail(r) {
			if after != nil {
				after()
			}
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return New(Config{Base: ts.URL}, nil)
}

func isList(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/api/bodyweights"
}
func isWrite(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/api/bodyweights"
}

func TestWorkerStopsQuietlyWhenTheJobEndsMidway(t *testing.T) {
	entry := []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}}
	payload := workerapi.ExportPayload{Login: goodLogin, Entries: entry}
	cancelled := func() (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, cancel
	}
	t.Run("while checking a login", func(t *testing.T) {
		w, _, _ := newWorker(t)
		ctx, _ := cancelled()
		if _, err := w.checkLogin(ctx, workerapi.CheckLoginPayload{Login: goodLogin}); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("while signing in to send a day", func(t *testing.T) {
		w, _, _ := newWorker(t)
		ctx, _ := cancelled()
		if _, err := w.sendDay(ctx, payload); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("while signing in to sync", func(t *testing.T) {
		w, _, _ := newWorker(t)
		ctx, _ := cancelled()
		if _, err := w.sync(ctx, payload, 0, func(workerapi.ExportProgress) {}); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("while reading the days to send one", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		w := flaky(t, isList, cancel)
		if _, err := w.sendDay(ctx, payload); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("while reading the days to sync", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		w := flaky(t, isList, cancel)
		if _, err := w.sync(ctx, payload, 0, func(workerapi.ExportProgress) {}); err != context.Canceled {
			t.Fatalf("got %v", err)
		}
	})
}

func TestWorkerRetriesWhenTheSiteBreaksMidway(t *testing.T) {
	entry := []workerapi.ExportEntry{{Day: "2026-11-07", WeightKg: 73}}
	payload := workerapi.ExportPayload{Login: goodLogin, Entries: entry}
	t.Run("sending a day", func(t *testing.T) {
		_, err := flaky(t, isList, nil).sendDay(context.Background(), payload)
		if ok, _ := runner.IsRetryable(err); !ok || err.Error() != "Could not send 2026-11-07: Strength Level could not be reached." {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("syncing", func(t *testing.T) {
		_, err := flaky(t, isList, nil).sync(context.Background(), payload, 0, func(workerapi.ExportProgress) {})
		if ok, _ := runner.IsRetryable(err); !ok || err.Error() != "Strength Level could not be reached." {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a day that cannot be written is counted and listed", func(t *testing.T) {
		res, err := flaky(t, isWrite, nil).sync(context.Background(), payload, 0, func(workerapi.ExportProgress) {})
		if err != nil {
			t.Fatal(err)
		}
		got := res.(workerapi.ExportResult)
		if got.Failed != 1 || len(got.FailedMessages) != 1 || !strings.Contains(got.FailedMessages[0], "500") {
			t.Fatalf("result %+v", got)
		}
	})
}
