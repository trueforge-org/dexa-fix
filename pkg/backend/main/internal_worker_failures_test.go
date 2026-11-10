package mainbackend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

const (
	scriptKind = "script.run"
	plainKind  = "script.plain"
)

// scripted is a provider whose behaviour each test sets: what a lease holds, what applying a
// result does, what renewed credentials do and whom the scheduler should queue for.
type scripted struct {
	mu    sync.Mutex
	lease func(ctx context.Context, s *Backend, j models.Job) (any, error)
	apply func(ctx context.Context, s *Backend, j models.Job, result json.RawMessage) (string, error)
	cred  func(ctx context.Context, s *Backend, j models.Job, data json.RawMessage) error
	sched func(ctx context.Context, s *Backend) ([]scheduleTarget, error)
}

func (sc *scripted) set(f func(*scripted)) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	f(sc)
}

func (sc *scripted) provider() workerProvider {
	defaults := kindDefaults{MaxAttempts: 3, RetryBase: time.Second, Timeout: time.Minute, PauseAfter: 2}
	get := func() *scripted {
		sc.mu.Lock()
		defer sc.mu.Unlock()
		return &scripted{lease: sc.lease, apply: sc.apply, cred: sc.cred, sched: sc.sched}
	}
	return workerProvider{Name: "script", Label: "Script", Kinds: []kindSpec{
		{
			Kind: scriptKind, Label: "Script run", Defaults: defaults,
			ScheduleFor: func(ctx context.Context, s *Backend) ([]scheduleTarget, error) {
				if f := get().sched; f != nil {
					return f(ctx, s)
				}
				return nil, nil
			},
			Lease: func(ctx context.Context, s *Backend, j models.Job) (any, error) {
				if f := get().lease; f != nil {
					return f(ctx, s, j)
				}
				return map[string]string{"ok": "yes"}, nil
			},
			Apply: func(ctx context.Context, s *Backend, j models.Job, r json.RawMessage) (string, error) {
				if f := get().apply; f != nil {
					return f(ctx, s, j, r)
				}
				return "{}", nil
			},
			Credentials: func(ctx context.Context, s *Backend, j models.Job, d json.RawMessage) error {
				if f := get().cred; f != nil {
					return f(ctx, s, j, d)
				}
				return nil
			},
		},
		{
			Kind: plainKind, Label: "Script plain", Defaults: defaults,
			Lease: func(ctx context.Context, s *Backend, j models.Job) (any, error) { return nil, nil },
			Apply: func(ctx context.Context, s *Backend, j models.Job, r json.RawMessage) (string, error) { return "", nil },
		},
	}}
}

// workerFlakyStore is a store that fails the calls a test asks it to fail.
type workerFlakyStore struct {
	store.Store
	putFails       sync.Map // setting names whose PutSetting fails
	enqueue        atomic.Bool
	lastFinished   atomic.Bool
	expireQueued   atomic.Bool
	release        atomic.Bool
	generation     atomic.Bool
	userMissing    atomic.Bool
	userBroken     atomic.Bool
	jobBroken      atomic.Bool
	completeLost   atomic.Bool
	failedCalls    atomic.Int64
	enqueueErr     error
	lastFinishedEr error
}

func (f *workerFlakyStore) Unwrap() store.Store { return f.Store }

// CompleteJob reports a lost lease, as when the lease ran out between two calls, when a test asks for it.
func (f *workerFlakyStore) CompleteJob(ctx context.Context, id, hash string, c models.Completion, now time.Time) (models.Job, error) {
	if f.completeLost.Load() {
		return models.Job{}, store.ErrLeaseLost
	}
	return f.Store.CompleteJob(ctx, id, hash, c, now)
}

// GetJob fails when a test asks for it.
func (f *workerFlakyStore) GetJob(ctx context.Context, id string) (models.Job, error) {
	if f.jobBroken.Load() {
		return models.Job{}, errWorkerFlaky
	}
	return f.Store.GetJob(ctx, id)
}

// GetUser answers "no such user" or fails, when a test asks for it.
func (f *workerFlakyStore) GetUser(ctx context.Context, id int64) (models.User, error) {
	if f.userMissing.Load() {
		return models.User{}, store.ErrNotFound
	}
	if f.userBroken.Load() {
		return models.User{}, errWorkerFlaky
	}
	return f.Store.GetUser(ctx, id)
}

var errWorkerFlaky = errors.New("the store failed on purpose")

func (f *workerFlakyStore) PutSetting(ctx context.Context, name, value string) error {
	if _, fail := f.putFails.Load(name); fail {
		return errWorkerFlaky
	}
	return f.Store.PutSetting(ctx, name, value)
}

func (f *workerFlakyStore) EnqueueJob(ctx context.Context, n models.NewJob) (models.Job, bool, error) {
	if f.enqueue.Load() {
		return models.Job{}, false, errWorkerFlaky
	}
	return f.Store.EnqueueJob(ctx, n)
}

func (f *workerFlakyStore) LastFinishedStates(ctx context.Context, kind string, n int, since time.Time) ([]string, error) {
	if f.lastFinished.Load() {
		return nil, errWorkerFlaky
	}
	return f.Store.LastFinishedStates(ctx, kind, n, since)
}

func (f *workerFlakyStore) ExpireQueuedJobs(ctx context.Context, now time.Time) (int64, error) {
	if f.expireQueued.Load() {
		return 0, errWorkerFlaky
	}
	return f.Store.ExpireQueuedJobs(ctx, now)
}

func (f *workerFlakyStore) ReleaseJob(ctx context.Context, id, hash string, now time.Time) error {
	if f.release.Load() {
		return errWorkerFlaky
	}
	return f.Store.ReleaseJob(ctx, id, hash, now)
}

func (f *workerFlakyStore) SettingsGeneration(ctx context.Context) (int64, error) {
	if f.generation.Load() {
		return 0, errWorkerFlaky
	}
	return f.Store.SettingsGeneration(ctx)
}

// scriptedFixture is the worker fixture with the scripted provider next to the demo one, on a
// store that can be made to fail.
type scriptedFixture struct {
	*workerFixture
	sc    *scripted
	flaky *workerFlakyStore
}

func newScriptedFixture(t *testing.T) *scriptedFixture {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fl := &workerFlakyStore{Store: st}
	d := &demoProvider{}
	sc := &scripted{}
	b, err := NewBackend(fl, testBody(fl), withWorkerProvider(d.provider()), withWorkerProvider(sc.provider()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.SetServiceSecrets("service-secret-0123456789abcdef")
	b.SetWorkerSecrets(testWorkerSecret)
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
	return &scriptedFixture{workerFixture: &workerFixture{b: b, st: st, srv: srv, client: c, d: d, admin: admin, user: user}, sc: sc, flaky: fl}
}

// queue queues a job of the kind for the user and returns it.
func (f *scriptedFixture) queue(t *testing.T, kind, origin string) models.Job {
	t.Helper()
	j, created, err := f.b.enqueueJob(context.Background(), enqueueSpec{UserID: f.user.ID, Kind: kind, Origin: origin, Dedupe: kind + time.Now().String()})
	if err != nil || !created {
		t.Fatalf("queue: created=%v err=%v", created, err)
	}
	return j
}

// leaseOf leases the next job of the kind.
func (f *scriptedFixture) leaseKind(t *testing.T, kind string) *workerapi.Lease {
	t.Helper()
	l, err := f.client.Lease(context.Background(), workerapi.LeaseRequest{Worker: "w1", Kinds: []string{kind}})
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return l
}

// raw sends a request to the worker endpoints as a worker would, with the given extra headers.
func (f *workerFixture) raw(method, path string, hdr map[string]string, body string) (int, string) {
	req, _ := http.NewRequest(method, f.srv.URL+workerapi.Prefix+path, strings.NewReader(body))
	req.Header.Set(workerapi.HeaderWorker, testWorkerSecret)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, sb.String()
}

func TestTheWorkerEndpointsRefuseWhatAWorkerMustNotDo(t *testing.T) {
	f := newWorkerFixture(t)
	f.enqueue(t, f.user.ID, models.JobOriginUser)
	l := f.lease(t)
	f.enqueue(t, f.admin.ID, models.JobOriginUser)
	other := f.lease(t) // a second job, leased to the same worker
	job := "/jobs/" + l.Job.ID
	lease := map[string]string{workerapi.HeaderLease: l.Token}

	t.Run("the health check answers and unknown paths are not found", func(t *testing.T) {
		if code, body := f.raw("GET", "/healthz", nil, ""); code != 200 || body != "ok" {
			t.Fatalf("healthz: %d %s", code, body)
		}
		if code, body := f.raw("GET", "/nothing-here", nil, ""); code != 404 || !strings.Contains(body, apiclient.CodeNotFound) {
			t.Fatalf("unknown path: %d %s", code, body)
		}
	})
	t.Run("a garbled body is refused by every endpoint that reads one", func(t *testing.T) {
		for _, path := range []string{"/register", "/lease", job + "/heartbeat", job + "/credentials", job + "/complete"} {
			if code, _ := f.raw("POST", path, lease, "nonsense"); code != 400 {
				t.Errorf("%s: %d", path, code)
			}
		}
	})
	t.Run("a worker without a name or a known kind is refused", func(t *testing.T) {
		for name, body := range map[string]string{
			"no id":              `{"id":""}`,
			"an id too long":     `{"id":"` + strings.Repeat("x", 65) + `"}`,
			"a version too long": `{"id":"w","version":"` + strings.Repeat("v", 65) + `"}`,
		} {
			if code, _ := f.raw("POST", "/register", nil, body); code != 400 {
				t.Errorf("register, %s: %d", name, code)
			}
		}
		for name, body := range map[string]string{
			"no kinds":               `{"worker":"w"}`,
			"only unknown kinds":     `{"worker":"w","kinds":["bogus.kind"]}`,
			"no worker":              `{"kinds":["` + demoKind + `"]}`,
			"a worker name too long": `{"worker":"` + strings.Repeat("x", 65) + `","kinds":["` + demoKind + `"]}`,
		} {
			if code, _ := f.raw("POST", "/lease", nil, body); code != 400 {
				t.Errorf("lease, %s: %d", name, code)
			}
		}
	})
	t.Run("too many workers waiting at once are told to come back", func(t *testing.T) {
		for i := 0; i < cap(f.b.leasePolls); i++ {
			f.b.leasePolls <- struct{}{}
		}
		code, body := f.raw("POST", "/lease", nil, `{"worker":"w","kinds":["`+demoKind+`"]}`)
		for i := 0; i < cap(f.b.leasePolls); i++ {
			<-f.b.leasePolls
		}
		if code != http.StatusServiceUnavailable || !strings.Contains(body, apiclient.CodeUnavailable) {
			t.Fatalf("%d %s", code, body)
		}
	})
	t.Run("progress that is too big is refused", func(t *testing.T) {
		big, _ := json.Marshal(map[string]string{"x": strings.Repeat("y", workerMaxProgress)})
		if code, _ := f.raw("POST", job+"/heartbeat", lease, `{"progress":`+string(big)+`}`); code != 400 {
			t.Errorf("too big: %d", code)
		}
	})
	t.Run("credentials that are empty, not JSON or too big are refused", func(t *testing.T) {
		big := `"` + strings.Repeat("y", workerMaxCredential) + `"`
		for name, body := range map[string]string{"empty": `{}`, "not json": `{"data":"x"`, "invalid": `{"data":nope}`, "too big": `{"data":` + big + `}`} {
			if code, _ := f.raw("POST", job+"/credentials", lease, body); code != 400 {
				t.Errorf("%s: %d", name, code)
			}
		}
	})
	t.Run("a call without the lease, with a wrong one or with another job's is told it lost the job", func(t *testing.T) {
		calls := map[string]string{
			"/heartbeat":   `{}`,
			"/credentials": `{"data":{"a":1}}`,
			"/complete":    `{"outcome":"ok","result":{}}`,
			"/release":     `{}`,
		}
		for suffix, body := range calls {
			for name, hdr := range map[string]map[string]string{
				"no lease":            {workerapi.HeaderLease: ""},
				"a wrong lease":       {workerapi.HeaderLease: "not-the-token"},
				"another job's lease": {workerapi.HeaderLease: other.Token},
			} {
				if code, _ := f.raw("POST", job+suffix, hdr, body); code != http.StatusConflict {
					t.Errorf("%s with %s: %d", suffix, name, code)
				}
			}
		}
		if got := f.job(t, l.Job.ID); got.State != models.JobRunning {
			t.Fatalf("a refused call changed the job: %+v", got)
		}
	})
	t.Run("the frontend's service secret opens none of them", func(t *testing.T) {
		hdr := map[string]string{workerapi.HeaderWorker: "", apiclient.HeaderService: "service-secret-0123456789abcdef", workerapi.HeaderLease: l.Token}
		for _, path := range []string{"/register", "/lease", job + "/heartbeat", job + "/credentials", job + "/complete", job + "/release"} {
			if code, _ := f.raw("POST", path, hdr, `{}`); code != http.StatusUnauthorized {
				t.Errorf("%s: %d", path, code)
			}
		}
	})
	t.Run("an outcome nobody knows is refused and leaves the job running", func(t *testing.T) {
		if code, _ := f.raw("POST", job+"/complete", lease, `{"outcome":"shrug"}`); code != 400 {
			t.Errorf("%d", code)
		}
		if got := f.job(t, l.Job.ID); got.State != models.JobRunning {
			t.Fatalf("%+v", got)
		}
	})
}

func TestTheWorkerEndpointsFailCleanlyWhenTheStoreLetsThemDown(t *testing.T) {
	setup := func(t *testing.T) (*workerFixture, *workerapi.Lease, map[string]string) {
		f := newWorkerFixture(t)
		f.enqueue(t, f.user.ID, models.JobOriginUser)
		l := f.lease(t)
		return f, l, map[string]string{workerapi.HeaderLease: l.Token}
	}
	t.Run("the heartbeat cannot be recorded", func(t *testing.T) {
		f, l, lease := setup(t)
		breakTable(t, f.st, "worker_jobs")
		if code, _ := f.raw("POST", "/jobs/"+l.Job.ID+"/heartbeat", lease, `{}`); code != 500 {
			t.Fatalf("%d", code)
		}
	})
	t.Run("the job cannot be looked up for credentials or its result", func(t *testing.T) {
		f, l, lease := setup(t)
		breakTable(t, f.st, "worker_jobs")
		if code, _ := f.raw("POST", "/jobs/"+l.Job.ID+"/credentials", lease, `{"data":{"a":1}}`); code != 500 {
			t.Errorf("credentials: %d", code)
		}
		if code, _ := f.raw("POST", "/jobs/"+l.Job.ID+"/complete", lease, `{"outcome":"ok","result":{}}`); code != 500 {
			t.Errorf("complete: %d", code)
		}
	})
	t.Run("the job cannot be given back", func(t *testing.T) {
		f, l, lease := setup(t)
		breakTable(t, f.st, "worker_jobs")
		if code, _ := f.raw("POST", "/jobs/"+l.Job.ID+"/release", lease, `{}`); code != 500 {
			t.Fatalf("%d", code)
		}
	})
	t.Run("the end of the job cannot be recorded", func(t *testing.T) {
		f, l, lease := setup(t)
		rejectWritesOn(t, f.st, "worker_jobs", "UPDATE")
		if code, _ := f.raw("POST", "/jobs/"+l.Job.ID+"/complete", lease, `{"outcome":"fail","error":"x"}`); code != 500 {
			t.Fatalf("%d", code)
		}
	})
	t.Run("the queue cannot be read for a lease", func(t *testing.T) {
		f := newWorkerFixture(t)
		breakTable(t, f.st, "worker_jobs")
		if code, _ := f.raw("POST", "/lease", nil, `{"worker":"w","kinds":["`+demoKind+`"]}`); code != 500 {
			t.Fatalf("%d", code)
		}
	})
	t.Run("the worker cannot be recorded, but the lease goes on", func(t *testing.T) {
		f := newWorkerFixture(t)
		breakTable(t, f.st, "worker_instances")
		logs := captureLog(t)
		code, _ := f.raw("POST", "/lease", nil, `{"worker":"w","kinds":["`+demoKind+`"]}`)
		if code != http.StatusNoContent || !strings.Contains(logs.String(), "record the worker") {
			t.Fatalf("%d, log %s", code, logs)
		}
	})
	t.Run("a worker that cannot register is told so", func(t *testing.T) {
		f := newWorkerFixture(t)
		breakTable(t, f.st, "worker_instances")
		if code, _ := f.raw("POST", "/register", nil, `{"id":"w","kinds":["`+demoKind+`"]}`); code != 500 {
			t.Fatalf("%d", code)
		}
	})
}

func TestAJobIsPreparedForAWorkerOrEndedHonestly(t *testing.T) {
	t.Run("a payload that is raw JSON goes out as it is, an empty one as an empty object", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.sc.set(func(s *scripted) {
			s.lease = func(context.Context, *Backend, models.Job) (any, error) { return json.RawMessage(`{"raw":1}`), nil }
		})
		f.queue(t, scriptKind, models.JobOriginUser)
		if l := f.leaseKind(t, scriptKind); l == nil || string(l.Job.Payload) != `{"raw":1}` {
			t.Fatalf("lease %+v", l)
		}
		f.sc.set(func(s *scripted) {
			s.lease = func(context.Context, *Backend, models.Job) (any, error) { return json.RawMessage(``), nil }
		})
		f.queue(t, scriptKind, models.JobOriginSchedule)
		if l := f.leaseKind(t, scriptKind); l == nil || string(l.Job.Payload) != `{}` {
			t.Fatalf("lease %+v", l)
		}
	})
	t.Run("a payload that cannot be written is given back", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.sc.set(func(s *scripted) {
			s.lease = func(context.Context, *Backend, models.Job) (any, error) { return make(chan int), nil }
		})
		j := f.queue(t, scriptKind, models.JobOriginUser)
		_, err := f.client.Lease(context.Background(), workerapi.LeaseRequest{Worker: "w1", Kinds: []string{scriptKind}})
		if !errors.Is(err, apiclient.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
		if got := f.job(t, j.PublicID); got.State != models.JobQueued {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("a job that cannot even be given back is logged", func(t *testing.T) {
		f := newScriptedFixture(t)
		f.sc.set(func(s *scripted) {
			s.lease = func(context.Context, *Backend, models.Job) (any, error) {
				return nil, errors.New("the database is troubled")
			}
		})
		f.flaky.release.Store(true)
		f.queue(t, scriptKind, models.JobOriginUser)
		logs := captureLog(t)
		_, err := f.client.Lease(context.Background(), workerapi.LeaseRequest{Worker: "w1", Kinds: []string{scriptKind}})
		if !errors.Is(err, apiclient.ErrUnavailable) || !strings.Contains(logs.String(), "could not give the job back") {
			t.Fatalf("%v, log %s", err, logs)
		}
	})
}

func TestRenewedCredentialsAreOnlyTakenForKindsThatHaveThem(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	send := func(l *workerapi.Lease) error {
		return f.client.SaveCredentials(ctx, l.Job.ID, l.Token, json.RawMessage(`{"token":"new"}`))
	}
	t.Run("a kind without credentials refuses them", func(t *testing.T) {
		f.queue(t, plainKind, models.JobOriginUser)
		l := f.leaseKind(t, plainKind)
		if err := send(l); err == nil || !strings.Contains(err.Error(), "does not take credentials") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("credentials the kind cannot use are refused as invalid", func(t *testing.T) {
		f.sc.set(func(s *scripted) {
			s.cred = func(context.Context, *Backend, models.Job, json.RawMessage) error { return errBadResult }
		})
		f.queue(t, scriptKind, models.JobOriginUser)
		l := f.leaseKind(t, scriptKind)
		if err := send(l); err == nil || !strings.Contains(err.Error(), "invalid credentials") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("credentials the store cannot keep are a server error the worker may repeat", func(t *testing.T) {
		f.sc.set(func(s *scripted) {
			s.cred = func(context.Context, *Backend, models.Job, json.RawMessage) error {
				return errors.New("database unavailable")
			}
		})
		l := f.leaseKind(t, scriptKind)
		if l == nil { // the job of the previous scenario is still leased: queue another
			f.b.enqueueJob(ctx, enqueueSpec{UserID: f.admin.ID, Kind: scriptKind, Origin: models.JobOriginUser})
			l = f.leaseKind(t, scriptKind)
		}
		if err := send(l); !errors.Is(err, apiclient.ErrUnavailable) {
			t.Fatalf("%v", err)
		}
	})
}

func TestAReportWithoutAnErrorTextGetsOneAndALongOneIsClipped(t *testing.T) {
	f := newScriptedFixture(t)
	ctx := context.Background()
	run := func(c workerapi.Completion) models.Job {
		t.Helper()
		f.queue(t, scriptKind, models.JobOriginUser)
		l := f.leaseKind(t, scriptKind)
		if err := f.client.Complete(ctx, l.Job.ID, l.Token, c); err != nil {
			t.Fatal(err)
		}
		return f.job(t, l.Job.ID)
	}
	if got := run(workerapi.Completion{Outcome: models.OutcomeRetry}); got.Error != "a temporary problem" || got.State != models.JobQueued {
		t.Fatalf("retry without a text: %+v", got)
	}
	if got := run(workerapi.Completion{Outcome: models.OutcomeFail}); got.Error != "failed" || got.State != models.JobFailed {
		t.Fatalf("fail without a text: %+v", got)
	}
	long := strings.Repeat("é", 3*workerMaxErrorText)
	if got := run(workerapi.Completion{Outcome: models.OutcomeFail, Error: long}); len([]rune(got.Error)) != workerMaxErrorText+1 || !strings.HasSuffix(got.Error, "…") {
		t.Fatalf("clipped error has %d characters", len([]rune(got.Error)))
	}
}
