package withings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/workers/runner"
)

// rawWorker is a worker against a fake Withings made of one handler; it counts the calls.
func rawWorker(t *testing.T, h http.HandlerFunc) (*Worker, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	backoffBase = time.Millisecond
	t.Cleanup(func() { backoffBase = 500 * time.Millisecond })
	return New(Config{APIBase: srv.URL}, nil), &hits
}

func freshImport(t *testing.T) workerapi.ImportPayload {
	return workerapi.ImportPayload{App: app, Credentials: creds(t, fresh())}
}

func noSave(context.Context, any) error { return errors.New("no tokens expected") }

func TestTheRealWithingsIsTheDefaultAndATrailingSlashIsIgnored(t *testing.T) {
	if got := (Config{}).apiBase(); got != defaultAPIBase {
		t.Fatalf("default = %q", got)
	}
	if got := (Config{APIBase: "http://x.test/"}).apiBase(); got != "http://x.test" {
		t.Fatalf("trimmed = %q", got)
	}
}

func TestClassifyLeavesNoErrorAlone(t *testing.T) {
	if classify(nil) != nil {
		t.Fatal("no error must stay no error")
	}
}

func TestEveryMeasurementTypeLandsInItsOwnField(t *testing.T) {
	at := ts("2026-03-01T07:00").Unix()
	g := measureGroup{GrpID: 9, Category: 1, Date: at, Measures: []measure{
		{Value: 80500, Type: 1, Unit: -3},
		{Value: 6500, Type: 5, Unit: -2},
		{Value: 195, Type: 6, Unit: -1},
		{Value: 15, Type: 8, Unit: 0},
		{Value: 61, Type: 76, Unit: 0},
		{Value: 45, Type: 77, Unit: 0},
		{Value: 3, Type: 88, Unit: 0},
		{Value: 7, Type: 170, Unit: 0},
		{Value: 1234, Type: 999, Unit: 0}, // a type DexaFix does not keep
	}}
	rs, _ := mapGroups([]measureGroup{g})
	if len(rs) != 1 {
		t.Fatalf("readings: %+v", rs)
	}
	r := rs[0]
	for name, got := range map[string]*float64{
		"weight": r.WeightKg, "fat-free mass": r.FatFreeMassKg, "fat ratio": r.FatRatioPct, "fat mass": r.FatMassKg,
		"muscle": r.MuscleMassKg, "hydration": r.HydrationKg, "bone": r.BoneMassKg, "visceral": r.VisceralFatIndex,
	} {
		if got == nil {
			t.Errorf("%s was not mapped", name)
		}
	}
	if *r.WeightKg != 80.5 || *r.FatFreeMassKg != 65 || *r.FatRatioPct != 19.5 || *r.VisceralFatIndex != 7 {
		t.Fatalf("values: weight %v ffm %v fat %v visceral %v", *r.WeightKg, *r.FatFreeMassKg, *r.FatRatioPct, *r.VisceralFatIndex)
	}
}

func TestWithingsAnswersThatGoWrongAreClassified(t *testing.T) {
	for _, tc := range []struct {
		name      string
		handler   http.HandlerFunc
		retryable bool
		calls     int32  // how often it was asked
		text      string // part of the message
	}{
		{"the connection is dropped", func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) },
			true, int32(maxAttempts), "withings request"},
		{"the answer is cut short", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.Write([]byte("{"))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}, false, 1, "withings response"},
		{"Withings is down and says nothing useful", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", http.StatusBadGateway) },
			true, int32(maxAttempts), "HTTP 502"},
		{"the answer is not JSON", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusBadRequest) },
			false, 1, "not JSON"},
		{"Withings asks to slow down by HTTP status", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"status":2555,"error":"slow"}`))
		}, true, int32(maxAttempts), "status 2555"},
		{"the measurements have the wrong shape", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"status":0,"body":{"measuregrps":"none"}}`))
		}, false, 1, "cannot unmarshal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, hits := rawWorker(t, tc.handler)
			_, err := w.importReadings(context.Background(), freshImport(t), noSave)
			if err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("got %v, want text %q", err, tc.text)
			}
			if ok, _ := runner.IsRetryable(err); ok != tc.retryable || runner.IsPermanent(err) == tc.retryable {
				t.Fatalf("retryable = %v, want %v (%v)", ok, tc.retryable, err)
			}
			if got := hits.Load(); got != tc.calls {
				t.Fatalf("Withings was asked %d times, want %d", got, tc.calls)
			}
		})
	}
}

func TestAMalformedWithingsAddressFailsTheJobForGood(t *testing.T) {
	w := New(Config{APIBase: "http://a b"}, nil)
	_, err := w.importReadings(context.Background(), freshImport(t), noSave)
	if !runner.IsPermanent(err) {
		t.Fatalf("got %v", err)
	}
}

func TestACancelledJobDoesNotCallWithings(t *testing.T) {
	w, hits := rawWorker(t, func(http.ResponseWriter, *http.Request) {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.importReadings(ctx, freshImport(t), noSave)
	if err == nil || !errors.Is(err, context.Canceled) || hits.Load() != 0 {
		t.Fatalf("got %v after %d calls", err, hits.Load())
	}
	if ok, _ := runner.IsRetryable(err); ok {
		t.Fatal("a cancel is not a temporary failure of Withings")
	}
}

// answering 500 and cancelling in the same breath, without a network in between.
type cancelAfterAnswer struct{ cancel context.CancelFunc }

func (c cancelAfterAnswer) RoundTrip(*http.Request) (*http.Response, error) {
	c.cancel()
	return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("down")), Header: http.Header{}}, nil
}

func TestAJobCancelledWhileWaitingToRetryStopsAtOnce(t *testing.T) {
	backoffBase = time.Hour
	t.Cleanup(func() { backoffBase = 500 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := New(Config{APIBase: "http://withings.test"}, &http.Client{Transport: cancelAfterAnswer{cancel}})
	done := make(chan error, 1)
	go func() { _, err := w.importReadings(ctx, freshImport(t), noSave); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the retry pause ignored the cancel")
	}
}

func TestATokenAnswerWithoutTokensFailsTheConnect(t *testing.T) {
	w, _ := rawWorker(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":0,"body":{"userid":1}}`)) })
	err := w.connect(context.Background(), workerapi.ConnectPayload{App: app, Code: "c"}, noSave)
	if !runner.IsPermanent(err) || !strings.Contains(err.Error(), "missing tokens") {
		t.Fatalf("got %v", err)
	}
}

func TestAnEndlessListOfPagesIsStopped(t *testing.T) {
	w, hits := rawWorker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":0,"body":{"measuregrps":[],"more":1,"offset":5}}`))
	})
	_, err := w.importReadings(context.Background(), freshImport(t), noSave)
	if !runner.IsPermanent(err) || !strings.Contains(err.Error(), "too many pages") || hits.Load() != 1000 {
		t.Fatalf("got %v after %d calls", err, hits.Load())
	}
}

func TestHandingOverTokensGivesUpWhenTheBackendHangsForTheWholeLimit(t *testing.T) {
	t.Parallel()
	start := time.Now()
	err := hand(context.Background(), func(ctx context.Context, v any) error {
		<-ctx.Done() // the backend never answers
		return ctx.Err()
	}, tokens{AccessToken: "a", RefreshToken: "r"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) < 10*time.Second {
		t.Fatalf("got %v after %v", err, time.Since(start))
	}
}

// runJob runs one job of the given kind through a real runner against a fake backend.
func runJob(t *testing.T, w *Worker, kind string, payload []byte) (workerapi.Completion, *atomic.Int32) {
	t.Helper()
	done := make(chan workerapi.Completion, 1)
	var saved atomic.Int32
	lease := &workerapi.Lease{
		Job:            workerapi.Job{ID: "job1", Kind: kind, UserID: 7, Attempt: 1, MaxAttempts: 3, Payload: payload},
		Token:          "tok",
		TimeoutSeconds: 30,
		LeaseSeconds:   30,
	}
	cfg := workerapi.Config{Generation: 1, Kinds: map[string]workerapi.KindConfig{kind: {Enabled: true}}}
	var handed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		switch parts[len(parts)-1] {
		case "register", "config":
			json.NewEncoder(rw).Encode(cfg)
		case "lease":
			if handed.Swap(true) {
				time.Sleep(20 * time.Millisecond)
				rw.WriteHeader(http.StatusNoContent)
				return
			}
			json.NewEncoder(rw).Encode(lease)
		case "heartbeat":
			json.NewEncoder(rw).Encode(workerapi.HeartbeatAnswer{})
		case "credentials":
			saved.Add(1)
			rw.WriteHeader(http.StatusNoContent)
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
		return c, &saved
	case <-time.After(15 * time.Second):
		t.Fatal("no completion reported")
		return workerapi.Completion{}, nil
	}
}

func TestHandlersRunConnectAndImportThroughTheRunner(t *testing.T) {
	enc := func(v any) []byte { b, _ := json.Marshal(v); return b }
	t.Run("connect stores the tokens with the backend", func(t *testing.T) {
		w, _, _ := setup(t)
		c, saved := runJob(t, w, KindConnect, enc(workerapi.ConnectPayload{App: app, Code: "the-code"}))
		if c.Outcome != models.OutcomeOK || saved.Load() != 1 {
			t.Fatalf("completion %+v, credentials stored %d times", c, saved.Load())
		}
	})
	t.Run("import returns the readings", func(t *testing.T) {
		w, f, _ := setup(t)
		f.groups = []measureGroup{grp(1, "2026-03-01T07:00", 80500)}
		c, _ := runJob(t, w, KindImport, enc(freshImport(t)))
		var res workerapi.ImportResult
		json.Unmarshal(c.Result, &res)
		if c.Outcome != models.OutcomeOK || len(res.Readings) != 1 || len(res.Seen) != 1 {
			t.Fatalf("completion %+v result %+v", c, res)
		}
	})
	t.Run("a refused import fails the job for good", func(t *testing.T) {
		w, _, _ := setup(t)
		c, _ := runJob(t, w, KindImport, enc(workerapi.ImportPayload{}))
		if c.Outcome != models.OutcomeFail || c.Error != "withings is not configured" {
			t.Fatalf("completion %+v", c)
		}
	})
	for _, kind := range []string{KindConnect, KindImport} {
		t.Run("an unreadable payload for "+kind, func(t *testing.T) {
			w, _, _ := setup(t)
			c, _ := runJob(t, w, kind, []byte(`"not an object"`))
			if c.Outcome != models.OutcomeFail || c.Error != "unreadable job" {
				t.Fatalf("completion %+v", c)
			}
		})
	}
}

func TestARefusedRefreshFailsTheImportForGoodAndSavesNothing(t *testing.T) {
	w, f, rec := setup(t)
	f.status = 293 // the refresh token was revoked
	expired := tokens{AccessToken: "old", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour)}
	_, err := w.importReadings(context.Background(), workerapi.ImportPayload{App: app, Credentials: creds(t, expired)}, rec.save)
	if !runner.IsPermanent(err) || !strings.Contains(err.Error(), "reconnect") || len(rec.saved) != 0 || len(f.meas) != 0 {
		t.Fatalf("got %v, saved %d, measure calls %d", err, len(rec.saved), len(f.meas))
	}
}
