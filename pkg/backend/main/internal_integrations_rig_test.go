package mainbackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/workerapi"
	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

func (f *flakyStore) GetJob(ctx context.Context, id string) (models.Job, error) {
	if f.fails("GetJob") {
		return models.Job{}, errFlaky
	}
	return f.Store.GetJob(ctx, id)
}

func (f *flakyStore) EnqueueJob(ctx context.Context, n models.NewJob) (models.Job, bool, error) {
	if f.fails("EnqueueJob") {
		return models.Job{}, false, errFlaky
	}
	return f.Store.EnqueueJob(ctx, n)
}

func (f *flakyStore) DeleteIntegration(ctx context.Context, userID int64, provider string) error {
	if f.fails("DeleteIntegration") {
		return errFlaky
	}
	return f.Store.DeleteIntegration(ctx, userID, provider)
}

// jobRig is a backend with no workers of its own, a user ("alice", id 2, signed in) and an
// administrator ("root", id 1), reached the way the web process reaches it. A test plays the
// worker itself, so that it decides how a job ends and when.
type jobRig struct {
	t      *testing.T
	st     *sqlstore.Store
	flaky  *flakyStore
	b      *Backend
	api    *apiclient.Client
	worker *workerapi.Client
	alice  context.Context // a request of alice's, for the apiclient
	root   context.Context // a request of the administrator's
	cookie string          // alice's session cookie, as "name=value"
	pub    *httptest.Server
	token  string // alice's access token for the public API
}

func newJobRig(t *testing.T) *jobRig {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	flaky := &flakyStore{Store: st}
	b, err := NewBackend(flaky, testBody(flaky))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	b.SetServiceSecrets("service-secret-0123456789abcdef")
	b.SetWorkerSecrets(testWorkerSecret)
	srv := httptest.NewServer(b.InternalHandler())
	t.Cleanup(srv.Close)
	api, err := apiclient.New(srv.URL, nil, "service-secret-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	wc, err := workerapi.New(srv.URL, nil, testWorkerSecret)
	if err != nil {
		t.Fatal(err)
	}
	r := &jobRig{t: t, st: st, flaky: flaky, b: b, api: api, worker: wc}
	ctx := context.Background()
	for i, name := range []string{"root", "alice"} {
		u, err := st.CreateUser(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		token := name + "-session-token-0123456789abcdef"
		if err := st.CreateSession(ctx, hashToken(token), u.ID, "test", time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			r.cookie = sessionCookie + "=" + token
		}
		visit := apiclient.WithRequest(ctx, apiclient.Request{Cookie: sessionCookie + "=" + token, ClientIP: "203.0.113.7", Host: "dexa.example.com", Secure: true})
		if i == 0 {
			r.root = visit
		} else {
			r.alice = visit
		}
	}
	r.pub = httptest.NewServer(b.PublicHandler())
	t.Cleanup(r.pub.Close)
	r.token = "dxf_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := st.CreateAccessToken(ctx, &models.AccessToken{UserID: 2, Name: "t", Prefix: "dxf_", Hash: hashToken(r.token)}); err != nil {
		t.Fatal(err)
	}
	return r
}

// call makes a public API request as alice and returns the status, the body and the headers.
func (r *jobRig) call(method, path, payload string) (int, string, http.Header) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.pub.URL+"/api/v1/users/2"+path, strings.NewReader(payload))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// connectWithings sets up the Withings application and links alice to it.
func (r *jobRig) connectWithings() {
	r.t.Helper()
	setWithingsApp(r.t, r.st)
	if err := r.st.PutIntegration(context.Background(), 2, "withings", []byte(`{}`)); err != nil {
		r.t.Fatal(err)
	}
}

// allowStrengthLevel gives alice the administrator's permission to use Strength Level.
func (r *jobRig) allowStrengthLevel(multiDay bool) {
	r.t.Helper()
	ctx := context.Background()
	if err := r.st.SetUserStrengthLevelAuto(ctx, 2, true); err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.SetUserStrengthLevelBulk(ctx, 2, multiDay); err != nil {
		r.t.Fatal(err)
	}
}

// saveStrengthLevelLogin stores a login as a finished check would have.
func (r *jobRig) saveStrengthLevelLogin() {
	r.t.Helper()
	data, _ := json.Marshal(slLogin{Email: "a@b.c", Password: "pw"})
	if err := r.st.PutIntegration(context.Background(), 2, strengthLevelProvider, data); err != nil {
		r.t.Fatal(err)
	}
}

// answer is how the test worker ends a job: nil leaves it running (the worker is busy with it).
type answer func(workerapi.Lease) *workerapi.Completion

func succeeds(result string) answer {
	return func(workerapi.Lease) *workerapi.Completion {
		return &workerapi.Completion{Outcome: models.OutcomeOK, Result: json.RawMessage(result)}
	}
}

func failsWith(message string) answer {
	return func(workerapi.Lease) *workerapi.Completion {
		return &workerapi.Completion{Outcome: models.OutcomeFail, Error: message}
	}
}

func stillBusy() answer { return func(workerapi.Lease) *workerapi.Completion { return nil } }

// workerFor starts a worker that runs the kinds and ends each job it leases as answer says. It has
// introduced itself when this returns, so interactive jobs are queued for it.
func (r *jobRig) workerFor(a answer, kinds ...string) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := r.worker.Register(ctx, workerapi.RegisterRequest{ID: "test-worker", Version: "t", Kinds: kinds}); err != nil {
		r.t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			l, err := r.worker.Lease(ctx, workerapi.LeaseRequest{Worker: "test-worker", Kinds: kinds})
			if err != nil || l == nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if c := a(*l); c != nil {
				r.worker.Complete(ctx, l.Job.ID, l.Token, *c)
			}
		}
	}()
	r.t.Cleanup(func() { cancel(); wg.Wait() })
}

// workerIdle makes a worker known for the kinds that never asks for a job: what is queued stays queued.
func (r *jobRig) workerIdle(kinds ...string) {
	r.t.Helper()
	if _, err := r.worker.Register(context.Background(), workerapi.RegisterRequest{ID: "idle-worker", Version: "t", Kinds: kinds}); err != nil {
		r.t.Fatal(err)
	}
}

// pauseKind turns a kind of job off, as an administrator does on the workers page.
func (r *jobRig) pauseKind(kind string) {
	r.t.Helper()
	if err := r.st.PutSetting(context.Background(), kindSettingName(kind, fieldEnabled), "false"); err != nil {
		r.t.Fatal(err)
	}
	if err := r.b.reloadSettings(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

// impatient makes a page give up waiting for a job quickly.
func impatient(t *testing.T) {
	t.Helper()
	old := importWait
	importWait = 400 * time.Millisecond
	t.Cleanup(func() { importWait = old })
}
