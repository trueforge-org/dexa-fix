package mainbackend

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

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

const (
	testWorkerSecret = "worker-secret-0123456789abcdef"
	demoKind         = "demo.import"
)

// demoProvider is a provider that does nothing outside the process, to test the framework.
type demoProvider struct {
	mu       sync.Mutex
	applied  []string
	applyErr error
	leaseErr error
}

func (d *demoProvider) provider() workerProvider {
	return workerProvider{Name: "demo", Label: "Demo", Kinds: []kindSpec{{
		Kind: demoKind, Label: "Demo import",
		Defaults: kindDefaults{MaxAttempts: 3, RetryBase: time.Second, Timeout: time.Minute, PauseAfter: 2},
		Options:  []optionField{{Name: "delay_ms", Label: "Pause between requests", Default: 100, Min: 0, Max: 5000, Millis: true}},
		ScheduleFor: func(ctx context.Context, s *Backend) ([]scheduleTarget, error) {
			ids, err := s.store.ListUsersWithIntegration(ctx, "demo")
			var out []scheduleTarget
			for _, id := range ids {
				out = append(out, scheduleTarget{UserID: id, Payload: map[string]string{"why": "schedule"}})
			}
			return out, err
		},
		Lease: func(ctx context.Context, s *Backend, j models.Job) (any, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.leaseErr != nil {
				return nil, d.leaseErr
			}
			return map[string]any{"user": j.UserID, "attempt": j.Attempts, "payload": j.Payload}, nil
		},
		Apply: func(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.applyErr != nil {
				return "", d.applyErr
			}
			d.applied = append(d.applied, string(result))
			return string(result), nil
		},
		Credentials: func(ctx context.Context, s *Backend, j models.Job, data json.RawMessage) error {
			return s.store.PutIntegration(ctx, j.UserID, "demo", data)
		},
	}}}
}

type workerFixture struct {
	b      *Backend
	st     *sqlstore.Store
	srv    *httptest.Server
	client *workerapi.Client
	d      *demoProvider
	admin  models.User
	user   models.User
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &demoProvider{}
	b, err := NewBackend(st, testBody(st), withWorkerProvider(d.provider()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.SetServiceSecrets("service-secret-0123456789abcdef")
	b.SetWorkerSecrets(testWorkerSecret, "older-worker-secret-0123456789")
	srv := httptest.NewServer(b.InternalHandler())
	t.Cleanup(srv.Close)
	c, err := workerapi.New(srv.URL, nil, testWorkerSecret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin, err := st.CreateUser(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return &workerFixture{b: b, st: st, srv: srv, client: c, d: d, admin: admin, user: user}
}

func (f *workerFixture) enqueue(t *testing.T, userID int64, origin string) models.Job {
	t.Helper()
	j, created, err := f.b.enqueueJob(context.Background(), enqueueSpec{UserID: userID, Kind: demoKind, Origin: origin, Payload: map[string]string{"secret": "s3cret"}, Dedupe: "k" + time.Now().String()})
	if err != nil || !created {
		t.Fatalf("enqueue: created=%v err=%v", created, err)
	}
	return j
}

func (f *workerFixture) lease(t *testing.T) *workerapi.Lease {
	t.Helper()
	l, err := f.client.Lease(context.Background(), workerapi.LeaseRequest{Worker: "w1", Kinds: []string{demoKind}, WaitSeconds: 0})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return l
}

func (f *workerFixture) job(t *testing.T, id string) models.Job {
	t.Helper()
	j, err := f.st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// setSetting writes a setting and puts it in force, as the Admin page does.
func (f *workerFixture) setSetting(t *testing.T, name, value string) {
	t.Helper()
	if err := f.st.PutSetting(context.Background(), name, value); err != nil {
		t.Fatal(err)
	}
	if err := f.b.reloadSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerEndpointsHaveTheirOwnSecret(t *testing.T) {
	f := newWorkerFixture(t)
	call := func(path string, hdr map[string]string, body string) int {
		method := "POST"
		if strings.HasSuffix(path, "/config") || strings.HasSuffix(path, "/session") {
			method = "GET"
		}
		req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	lease := workerapi.Prefix + "/lease"
	body := `{"worker":"w","kinds":["` + demoKind + `"]}`
	for name, tc := range map[string]struct {
		path string
		hdr  map[string]string
		want int
	}{
		"no secret":                        {lease, nil, 401},
		"wrong secret":                     {lease, map[string]string{workerapi.HeaderWorker: "nope"}, 401},
		"the frontend's service secret":    {lease, map[string]string{apiclient.HeaderService: "service-secret-0123456789abcdef"}, 401},
		"service secret in the worker key": {lease, map[string]string{workerapi.HeaderWorker: "service-secret-0123456789abcdef"}, 401},
		"the worker secret":                {lease, map[string]string{workerapi.HeaderWorker: testWorkerSecret}, 204},
		"the rotated-out secret":           {lease, map[string]string{workerapi.HeaderWorker: "older-worker-secret-0123456789"}, 204},
		"the worker secret on the pages":   {apiclient.Prefix + "/session", map[string]string{apiclient.HeaderService: testWorkerSecret}, 401},
		"the worker secret, other header":  {apiclient.Prefix + "/session", map[string]string{workerapi.HeaderWorker: testWorkerSecret}, 401},
	} {
		if got := call(tc.path, tc.hdr, body); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}

	// Without any worker secret configured the endpoints are closed, whatever is presented.
	f.b.SetWorkerSecrets()
	if got := call(lease, map[string]string{workerapi.HeaderWorker: testWorkerSecret}, body); got != 401 {
		t.Errorf("no secrets configured: status %d", got)
	}
}

func TestWorkerJobLifecycle(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	if cfg, err := f.client.Register(ctx, workerapi.RegisterRequest{ID: "w1", Version: "t", Kinds: []string{demoKind, "bogus.kind"}}); err != nil ||
		!cfg.Kinds[demoKind].Enabled || cfg.Kinds[demoKind].Options["delay_ms"] != "100" || cfg.LeaseSeconds != 60 || cfg.Generation == 0 {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	if ws, _ := f.st.ListWorkers(ctx, time.Now().Add(-time.Minute)); len(ws) != 1 || len(ws[0].Kinds) != 1 {
		t.Fatalf("the worker should be known with the kinds it runs: %+v", ws)
	}
	if l := f.lease(t); l != nil {
		t.Fatalf("leased from an empty queue: %+v", l)
	}
	j := f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)
	if l == nil || l.Job.ID != j.PublicID || l.Job.UserID != f.user.ID || l.Job.Attempt != 1 || l.Token == "" || l.TimeoutSeconds != 60 {
		t.Fatalf("lease: %+v", l)
	}
	var payload struct {
		User    int64
		Attempt int
		Payload string
	}
	if err := json.Unmarshal(l.Job.Payload, &payload); err != nil || payload.User != f.user.ID || !strings.Contains(payload.Payload, "s3cret") {
		t.Fatalf("payload: %s %v", l.Job.Payload, err)
	}

	// Another token cannot do anything with the job.
	if _, err := f.client.Heartbeat(ctx, l.Job.ID, "wrong-token", nil); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("heartbeat with a wrong token: %v", err)
	}
	if err := f.client.Complete(ctx, l.Job.ID, "wrong-token", workerapi.Completion{Outcome: models.OutcomeOK}); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("complete with a wrong token: %v", err)
	}

	cancel, err := f.client.Heartbeat(ctx, l.Job.ID, l.Token, json.RawMessage(`{"done":2}`))
	if err != nil || cancel {
		t.Fatalf("heartbeat: %v %v", cancel, err)
	}
	if got := f.job(t, l.Job.ID).Progress; got != `{"done":2}` {
		t.Fatalf("progress: %q", got)
	}
	if _, err := f.client.Heartbeat(ctx, l.Job.ID, l.Token, json.RawMessage(`{not json`)); err == nil {
		t.Fatal("invalid progress accepted")
	}
	if err := f.client.SaveCredentials(ctx, l.Job.ID, l.Token, json.RawMessage(`{"token":"renewed"}`)); err != nil {
		t.Fatal(err)
	}
	if got, err := f.st.GetIntegration(ctx, f.user.ID, "demo"); err != nil || string(got) != `{"token":"renewed"}` {
		t.Fatalf("stored credentials: %q %v", got, err)
	}

	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK, Result: json.RawMessage(`{"n":3}`)}); err != nil {
		t.Fatal(err)
	}
	done := f.job(t, l.Job.ID)
	if done.State != models.JobSucceeded || done.Result != `{"n":3}` || done.Payload != "" {
		t.Fatalf("job after completion: %+v", done)
	}
	if len(f.d.applied) != 1 || f.d.applied[0] != `{"n":3}` {
		t.Fatalf("applied: %v", f.d.applied)
	}
	// The attempt is over: nothing more can be reported or renewed.
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK}); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("second completion: %v", err)
	}
	if _, err := f.client.Heartbeat(ctx, l.Job.ID, l.Token, nil); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("heartbeat after the end: %v", err)
	}
	if err := f.client.SaveCredentials(ctx, l.Job.ID, l.Token, json.RawMessage(`{}`)); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("credentials after the end: %v", err)
	}
}

func TestWorkerRetriesAreBoundedAndBackedOff(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	j := f.enqueue(t, f.user.ID, models.JobOriginUser)

	// A provider's own Retry-After is honoured, up to a cap.
	l := f.lease(t)
	before := time.Now()
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeRetry, Error: "rate limited", RetryAfterSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	got := f.job(t, j.PublicID)
	if got.State != models.JobQueued || got.Error != "rate limited" || got.RunAfter.Before(before.Add(110*time.Second)) {
		t.Fatalf("after a retry with a hint: %+v", got)
	}
	if f.lease(t) != nil {
		t.Fatal("the job came back before its time")
	}

	// A silly hint is capped.
	capped := f.enqueue(t, f.admin.ID, models.JobOriginUser)
	l = f.lease(t)
	if l == nil || l.Job.ID != capped.PublicID {
		t.Fatalf("lease: %+v", l)
	}
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeRetry, Error: "x", RetryAfterSeconds: 30 * 24 * 3600}); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, capped.PublicID); got.RunAfter.After(time.Now().Add(maxRetryDelay + time.Minute)) {
		t.Fatalf("a retry was put off for %v", time.Until(got.RunAfter))
	}

	// Without a hint the wait grows, and after the last attempt the job fails for good.
	k := f.enqueue(t, f.admin.ID, models.JobOriginUser)
	for attempt := 1; attempt <= 3; attempt++ {
		var l *workerapi.Lease
		deadline := time.Now().Add(10 * time.Second)
		for l == nil && time.Now().Before(deadline) {
			if l = f.lease(t); l == nil {
				time.Sleep(100 * time.Millisecond)
			}
		}
		if l == nil || l.Job.ID != k.PublicID || l.Job.Attempt != attempt {
			t.Fatalf("attempt %d: %+v", attempt, l)
		}
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeRetry, Error: "connection reset"}); err != nil {
			t.Fatal(err)
		}
	}
	last := f.job(t, k.PublicID)
	if last.State != models.JobFailed || !strings.Contains(last.Error, "gave up after 3 attempts") || last.Attempts != 3 {
		t.Fatalf("after the last attempt: %+v", last)
	}
}

func TestWorkerApplyFailures(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)

	// Our own trouble must not lose the result: the worker is told to report again.
	f.d.applyErr = errors.New("database unavailable")
	err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK, Result: json.RawMessage(`{}`)})
	if !errors.Is(err, apiclient.ErrUnavailable) {
		t.Fatalf("complete while the backend is troubled: %v", err)
	}
	if got := f.job(t, l.Job.ID); got.State != models.JobRunning {
		t.Fatalf("the job left the running state: %+v", got)
	}
	f.d.applyErr = nil
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if got := f.job(t, l.Job.ID); got.State != models.JobSucceeded {
		t.Fatalf("job: %+v", got)
	}

	// A result that can never be used ends the job without retries.
	f.enqueue(t, f.user.ID, models.JobOriginUser)
	l = f.lease(t)
	f.d.applyErr = errBadResult
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got := f.job(t, l.Job.ID); got.State != models.JobFailed || got.Attempts != 1 {
		t.Fatalf("job with a bad result: %+v", got)
	}
}

func TestWorkerLeaseFailures(t *testing.T) {
	f := newWorkerFixture(t)
	f.d.leaseErr = permanent(errors.New("Demo is not connected"))
	j := f.enqueue(t, f.user.ID, models.JobOriginUser)
	if l := f.lease(t); l != nil {
		t.Fatalf("a job that cannot be prepared was handed out: %+v", l)
	}
	if got := f.job(t, j.PublicID); got.State != models.JobFailed || got.Error != "Demo is not connected" {
		t.Fatalf("permanent: %+v", got)
	}

	// Our own trouble gives the job back without costing an attempt.
	f.d.leaseErr = errors.New("database unavailable")
	k := f.enqueue(t, f.user.ID, models.JobOriginUser)
	if _, err := f.client.Lease(context.Background(), workerapi.LeaseRequest{Worker: "w1", Kinds: []string{demoKind}}); !errors.Is(err, apiclient.ErrUnavailable) {
		t.Fatalf("lease while the backend is troubled: %v", err)
	}
	if got := f.job(t, k.PublicID); got.State != models.JobQueued || got.Attempts != 0 {
		t.Fatalf("transient: %+v", got)
	}
}

func TestEnqueuePolicy(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	spec := func(interactive bool) enqueueSpec {
		return enqueueSpec{UserID: f.user.ID, Kind: demoKind, Origin: models.JobOriginUser, Interactive: interactive}
	}
	if _, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: "nope.kind", Origin: "user"}); err == nil {
		t.Fatal("an unknown kind was queued")
	}
	// Somebody waits for an interactive job: say at once that nothing is running.
	if _, _, err := f.b.enqueueJob(ctx, spec(true)); !errors.Is(err, errNoWorker) {
		t.Fatalf("interactive without a worker: %v", err)
	}
	f.client.Register(ctx, workerapi.RegisterRequest{ID: "w1", Kinds: []string{demoKind}})
	first, created, err := f.b.enqueueJob(ctx, spec(true))
	if err != nil || !created {
		t.Fatalf("interactive with a worker: %v %v", created, err)
	}
	// The same user and kind: one active job.
	again, created, err := f.b.enqueueJob(ctx, spec(false))
	if err != nil || created || again.PublicID != first.PublicID {
		t.Fatalf("duplicate: %+v %v %v", again, created, err)
	}
	// A paused kind takes nothing, and workers are not given work of it.
	f.setSetting(t, kindSettingName(demoKind, fieldEnabled), "false")
	if _, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.admin.ID, Kind: demoKind, Origin: "user"}); !errors.Is(err, errKindPaused) {
		t.Fatalf("paused kind: %v", err)
	}
	if cfg := f.b.workerConfigNow(); cfg.Kinds[demoKind].Enabled {
		t.Fatal("the workers' configuration still says enabled")
	}
	if l := f.lease(t); l != nil {
		t.Fatalf("leased a job of a paused kind: %+v", l)
	}
}

func TestWorkerThatVanishesLosesItsJob(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	oldLease, oldEvery := workerLease.Get(), maintainEvery.Get()
	workerLease.Set(time.Second)
	maintainEvery.Set(0)
	t.Cleanup(func() { workerLease.Set(oldLease); maintainEvery.Set(oldEvery) })

	j := f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)
	if l == nil || l.LeaseSeconds != 1 {
		t.Fatalf("lease: %+v", l)
	}
	time.Sleep(1300 * time.Millisecond) // no heartbeat
	f.b.maintainQueue(ctx)
	got := f.job(t, j.PublicID)
	if got.State != models.JobQueued || got.Attempts != 1 || !strings.Contains(got.Error, "stopped responding") {
		t.Fatalf("after the lease ran out: %+v", got)
	}
	// The worker that wakes up later cannot report or renew.
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeOK}); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("late completion: %v", err)
	}
	if _, err := f.client.Heartbeat(ctx, l.Job.ID, l.Token, nil); !errors.Is(err, workerapi.ErrLeaseLost) {
		t.Fatalf("late heartbeat: %v", err)
	}
}

func TestCircuitBreakerPausesAKindThatKeepsFailing(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	f.setSetting(t, kindSettingName(demoKind, fieldBreaker), "2")
	for i := 0; i < 2; i++ {
		f.enqueue(t, f.user.ID, models.JobOriginUser)
		l := f.lease(t)
		if l == nil {
			t.Fatalf("lease %d", i)
		}
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "service down"}); err != nil {
			t.Fatal(err)
		}
	}
	ks := f.b.cfg().workers.Kinds[demoKind]
	if ks.Enabled || !strings.Contains(ks.PausedReason, "2 jobs failed in a row") {
		t.Fatalf("the kind was not paused: %+v", ks)
	}
	if _, _, err := f.b.enqueueJob(ctx, enqueueSpec{UserID: f.user.ID, Kind: demoKind, Origin: "user"}); !errors.Is(err, errKindPaused) {
		t.Fatalf("enqueue after the breaker tripped: %v", err)
	}

	// After an administrator turned it on again, the failures from before do not count.
	if err := f.b.enableKind(ctx, demoKind); err != nil {
		t.Fatal(err)
	}
	if err := f.b.reloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)
	if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "still down"}); err != nil {
		t.Fatal(err)
	}
	if !f.b.cfg().workers.Kinds[demoKind].Enabled {
		t.Fatal("one failure after the resume paused the kind again")
	}
}

func TestSchedulerEnqueuesOncePerIntervalAndSkipsPausedUsers(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	f.setSetting(t, kindSettingName(demoKind, fieldScheduleHours), "1")
	third, err := f.st.CreateUser(ctx, "carol")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []models.User{f.user, f.admin, third} {
		if err := f.st.PutIntegration(ctx, u.ID, "demo", []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}
	// carol's automatic runs were paused after failures.
	if err := f.st.PutJobSchedule(ctx, models.JobSchedule{UserID: third.ID, Kind: demoKind, ConsecutiveFailures: 2, PausedReason: "paused"}); err != nil {
		t.Fatal(err)
	}

	f.b.runScheduler(ctx)
	f.b.runScheduler(ctx) // nothing is due the second time
	jobs, err := f.st.ListJobs(ctx, models.JobFilter{Kinds: []string{demoKind}})
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs: %d %v", len(jobs), err)
	}
	for _, j := range jobs {
		if j.Origin != models.JobOriginSchedule || j.UserID == third.ID || j.ExpiresAt == nil || j.Interactive {
			t.Fatalf("scheduled job: %+v", j)
		}
	}
	// Once the user fixes their connection, they are scheduled again.
	f.b.resumeSchedule(ctx, third.ID, demoKind)
	f.b.runScheduler(ctx)
	if jobs, _ = f.st.ListJobs(ctx, models.JobFilter{Kinds: []string{demoKind}}); len(jobs) != 3 {
		t.Fatalf("after the resume: %d jobs", len(jobs))
	}
}

func TestScheduleIsPausedAfterRepeatedFailuresAndRecoversOnSuccess(t *testing.T) {
	f := newWorkerFixture(t) // PauseAfter is 2
	ctx := context.Background()
	run := func(outcome string) {
		t.Helper()
		f.enqueue(t, f.user.ID, models.JobOriginSchedule)
		l := f.lease(t)
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: outcome, Error: "bad login", Result: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	run(models.OutcomeFail)
	if sc, _ := f.st.GetJobSchedule(ctx, f.user.ID, demoKind); sc.ConsecutiveFailures != 1 || sc.PausedReason != "" {
		t.Fatalf("after one failure: %+v", sc)
	}
	run(models.OutcomeOK) // a success starts the count afresh
	if sc, _ := f.st.GetJobSchedule(ctx, f.user.ID, demoKind); sc.ConsecutiveFailures != 0 {
		t.Fatalf("after a success: %+v", sc)
	}
	run(models.OutcomeFail)
	run(models.OutcomeFail)
	sc, err := f.st.GetJobSchedule(ctx, f.user.ID, demoKind)
	if err != nil || sc.PausedReason == "" || !strings.Contains(sc.PausedReason, "bad login") {
		t.Fatalf("the schedule should be paused with the reason: %+v %v", sc, err)
	}
	// A manual job by the same user is not counted against the schedule.
	f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)
	f.client.Complete(ctx, l.Job.ID, l.Token, workerapi.Completion{Outcome: models.OutcomeFail, Error: "x"})
	if after, _ := f.st.GetJobSchedule(ctx, f.user.ID, demoKind); after.ConsecutiveFailures != sc.ConsecutiveFailures {
		t.Fatalf("a manual job changed the schedule: %+v", after)
	}
	f.b.resumeSchedule(ctx, f.user.ID)
	if _, err := f.st.GetJobSchedule(ctx, f.user.ID, demoKind); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the schedule should be forgotten: %v", err)
	}
}

func TestNewAccountsGetTheDefaultPriority(t *testing.T) {
	f := newWorkerFixture(t)
	ctx := context.Background()
	f.setSetting(t, settingWorkersDefaultPriority, "20")
	u, err := f.b.createUser(ctx, "dave")
	if err != nil || u.JobPriority != 20 {
		t.Fatalf("new user: %+v %v", u, err)
	}
	if got, _ := f.st.GetUser(ctx, u.ID); got.JobPriority != 20 {
		t.Fatalf("stored priority %d", got.JobPriority)
	}
}

func TestAdminWorkerEndpoints(t *testing.T) {
	d := &demoProvider{}
	bs, srv, rootSession := newInternalServer(t, withWorkerProvider(d.provider()))
	ctx := context.Background()
	api := splitClient(t, bs)
	visitor := func(cookie string) context.Context {
		return apiclient.WithRequest(ctx, apiclient.Request{Cookie: cookie, ClientIP: "203.0.113.7", Host: "dexa.example.com", Secure: true})
	}
	root := visitor(sessionCookie + "=" + rootSession.Value)

	// A user who is not an administrator sees and changes nothing.
	bob, err := srv.store.CreateUserWithIdentity(ctx, "bob", "bob@example.com", models.Identity{Provider: "idp", Subject: "bob-1"})
	if err != nil {
		t.Fatal(err)
	}
	token := "bob-session-token-0123456789abcdef"
	if err := srv.store.CreateSession(ctx, hashToken(token), bob.ID, "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	asBob := visitor(sessionCookie + "=" + token)
	if _, err := api.AdminWorkers(asBob); !apiclient.IsCode(err, apiclient.CodeForbidden) {
		t.Fatalf("workers page as a user: %v", err)
	}
	if _, err := api.AdminSetJobPriority(asBob, bob.ID, 100); !apiclient.IsCode(err, apiclient.CodeForbidden) {
		t.Fatalf("priority as a user: %v", err)
	}
	if _, err := api.AdminWorkers(visitor("")); !apiclient.IsCode(err, apiclient.CodeUnauthorized) {
		t.Fatalf("workers page anonymous: %v", err)
	}
	if got, _ := srv.store.GetUser(ctx, bob.ID); got.JobPriority != models.DefaultJobPriority {
		t.Fatalf("a user changed their own priority: %d", got.JobPriority)
	}

	page, err := api.AdminWorkers(root)
	if err != nil || len(page.Providers) < 2 {
		t.Fatalf("page: %+v %v", page, err)
	}
	demo := func(p *apiclient.WorkersPage) apiclient.WorkerKindView {
		for _, pv := range p.Providers {
			if pv.Name == "demo" {
				return pv.Kinds[0]
			}
		}
		t.Fatal("the demo provider is not listed")
		return apiclient.WorkerKindView{}
	}
	k := demo(page)
	if !k.Enabled || k.MaxAttempts != 3 || k.RetryBaseSeconds != 1 || k.TimeoutSeconds != 60 || !k.Schedulable || len(k.Options) != 1 || k.Options[0].Value != 100 || page.DefaultPriority != 50 {
		t.Fatalf("defaults: %+v", k)
	}

	// Saving: valid values apply at once; an out-of-range one is refused with a message and changes nothing.
	form := apiclient.WorkerKindForm{Enabled: true, MaxRunning: 2, MaxAttempts: 5, RetryBaseSeconds: 30, TimeoutSeconds: 120, ScheduleHours: 6, PauseAfter: 4, Breaker: 8, Options: map[string]int{"delay_ms": 250}}
	if res, err := api.AdminSaveWorkerKind(root, demoKind, form); err != nil || res.Message != "Demo import settings saved." {
		t.Fatalf("save: %+v %v", res, err)
	}
	ks := srv.Backend.cfg().workers.Kinds[demoKind]
	if ks.MaxRunning != 2 || ks.MaxAttempts != 5 || ks.RetryBase != 30*time.Second || ks.Timeout != 2*time.Minute || ks.ScheduleEvery != 6*time.Hour || ks.PauseAfter != 4 || ks.Breaker != 8 || ks.Options["delay_ms"] != 250 {
		t.Fatalf("settings in force: %+v", ks)
	}
	for name, bad := range map[string]func(*apiclient.WorkerKindForm){
		"attempts": func(f *apiclient.WorkerKindForm) { f.MaxAttempts = 99 },
		"retry":    func(f *apiclient.WorkerKindForm) { f.RetryBaseSeconds = 0 },
		"timeout":  func(f *apiclient.WorkerKindForm) { f.TimeoutSeconds = 1 },
		"schedule": func(f *apiclient.WorkerKindForm) { f.ScheduleHours = -1 },
		"option":   func(f *apiclient.WorkerKindForm) { f.Options = map[string]int{"delay_ms": 999999} },
	} {
		f := form
		bad(&f)
		res, err := api.AdminSaveWorkerKind(root, demoKind, f)
		if err != nil || !strings.Contains(res.Message, "must be a number from") {
			t.Fatalf("%s: %+v %v", name, res, err)
		}
	}
	if got := srv.Backend.cfg().workers.Kinds[demoKind]; got.MaxAttempts != 5 || got.Options["delay_ms"] != 250 {
		t.Fatalf("a refused save changed settings: %+v", got)
	}
	if _, err := api.AdminSaveWorkerKind(root, "nope.kind", form); !apiclient.IsCode(err, apiclient.CodeNotFound) {
		t.Fatalf("unknown kind: %v", err)
	}

	// Priorities: 1 to 100, for an existing user.
	if res, err := api.AdminSetJobPriority(root, bob.ID, 80); err != nil || !strings.Contains(res.Message, "80") {
		t.Fatalf("set priority: %+v %v", res, err)
	}
	if got, _ := srv.store.GetUser(ctx, bob.ID); got.JobPriority != 80 {
		t.Fatalf("priority %d", got.JobPriority)
	}
	for _, bad := range []int{0, 101, -5} {
		if res, err := api.AdminSetJobPriority(root, bob.ID, bad); err != nil || !strings.Contains(res.Message, "from 1 to 100") {
			t.Fatalf("priority %d: %+v %v", bad, res, err)
		}
	}
	if got, _ := srv.store.GetUser(ctx, bob.ID); got.JobPriority != 80 {
		t.Fatalf("a refused priority changed it: %d", got.JobPriority)
	}
	if _, err := api.AdminSetJobPriority(root, 9999, 10); !apiclient.IsCode(err, apiclient.CodeNotFound) {
		t.Fatalf("unknown user: %v", err)
	}

	// Global settings.
	if res, err := api.AdminSaveWorkersGlobal(root, apiclient.WorkersGlobalForm{DefaultPriority: 0, KeepHistoryDays: 10}); err != nil || !strings.Contains(res.Message, "from 1 to 100") {
		t.Fatalf("global refused: %+v %v", res, err)
	}
	if _, err := api.AdminSaveWorkersGlobal(root, apiclient.WorkersGlobalForm{DefaultPriority: 30, KeepHistoryDays: 10}); err != nil {
		t.Fatal(err)
	}
	if g := srv.Backend.cfg().workers; g.DefaultPriority != 30 || g.KeepHistory != 10*24*time.Hour {
		t.Fatalf("global settings: %+v", g)
	}

	// The queue: the page lists jobs with the owner's priority, and an administrator can cancel one.
	job, _, err := srv.Backend.enqueueJob(ctx, enqueueSpec{UserID: bob.ID, Kind: demoKind, Origin: models.JobOriginUser})
	if err != nil {
		t.Fatal(err)
	}
	page, _ = api.AdminWorkers(root)
	if len(page.Jobs) != 1 || page.Jobs[0].ID != job.PublicID || page.Jobs[0].Priority != 80 || page.Jobs[0].User != "bob" || !page.Jobs[0].Active || demo(page).Queued != 1 {
		t.Fatalf("jobs: %+v", page.Jobs)
	}
	if res, err := api.AdminCancelJob(root, job.PublicID); err != nil || res.Message != "The job was canceled." {
		t.Fatalf("cancel: %+v %v", res, err)
	}
	if got, _ := srv.store.GetJob(ctx, job.PublicID); got.State != models.JobCanceled {
		t.Fatalf("job: %+v", got)
	}
	if _, err := api.AdminCancelJob(root, "no-such-job"); !apiclient.IsCode(err, apiclient.CodeNotFound) {
		t.Fatalf("cancel unknown: %v", err)
	}

	// A paused kind is resumed from the page, and a paused schedule too.
	if _, err := api.AdminSaveWorkerKind(root, demoKind, apiclient.WorkerKindForm{Enabled: false, MaxAttempts: 3, RetryBaseSeconds: 1, TimeoutSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if srv.Backend.cfg().workers.Kinds[demoKind].Enabled {
		t.Fatal("the kind was not paused")
	}
	if _, err := api.AdminResumeWorkerKind(root, demoKind); err != nil {
		t.Fatal(err)
	}
	if !srv.Backend.cfg().workers.Kinds[demoKind].Enabled {
		t.Fatal("the kind was not resumed")
	}
	srv.store.PutJobSchedule(ctx, models.JobSchedule{UserID: bob.ID, Kind: demoKind, ConsecutiveFailures: 3, PausedReason: "paused"})
	if _, err := api.AdminResumeSchedule(root, bob.ID, demoKind); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.GetJobSchedule(ctx, bob.ID, demoKind); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("schedule after the resume: %v", err)
	}
}
