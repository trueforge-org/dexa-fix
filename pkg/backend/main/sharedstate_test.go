package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// The state that matters lives in the database, so several backends on one database agree. These
// tests build two Backends on one store.

func twoBackends(t *testing.T, optsB ...Option) (a, b *Backend, st *sqlstore.Store) {
	t.Helper()
	st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return twoBackendsOn(t, st, optsB...)
}

func twoBackendsOn(t *testing.T, st *sqlstore.Store, optsB ...Option) (a, b *Backend, _ *sqlstore.Store) {
	t.Helper()
	a, err := NewBackend(st, testBody(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	b, err = NewBackend(st, append([]Option{testBody(st)}, optsB...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	return a, b, st
}

func TestSealedValueCanBeSpentOnlyOnceAcrossBackends(t *testing.T) {
	a, b, _ := twoBackends(t)
	ctx := context.Background()
	sealed, _, err := a.sealValue("test", map[string]string{"x": "y"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	nonceA, expires, ok := a.openValue("test", sealed, &v)
	if !ok {
		t.Fatal("A cannot open its own value")
	}
	nonceB, _, ok := b.openValue("test", sealed, &v) // the key is in the database: B opens it too
	if !ok || nonceA != nonceB {
		t.Fatal("B cannot open the value A made")
	}
	if b.nonces.seen(ctx, nonceB) {
		t.Fatal("seen before use")
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*Backend{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.nonces.use(ctx, nonceA, expires) {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d spenders won, want 1", wins.Load())
	}
	if !a.nonces.seen(ctx, nonceA) || !b.nonces.seen(ctx, nonceA) {
		t.Fatal("a spent nonce is not seen by both backends")
	}
}

func TestSecondStepCookieIsNotReplayableOnAnotherBackend(t *testing.T) {
	skipInSplit(t)
	ts, c, st := newAuthServer(t)
	srv := serverOf(ts)
	signup(t, ts, c, "alice")
	_, codes := tfEnable(t, srv, 1)

	srv2, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv2.Close)
	ts2 := httptest.NewServer(srv2.Handler())
	t.Cleanup(ts2.Close)

	mfa := tfMFACookie(t, tfPasswordLogin(t, noRedirectClient(), ts.URL, "alice@example.com"))
	if r, _ := tfSecondStep(t, noRedirectClient(), ts.URL, mfa, tfCode(t, srv, 1)); r.StatusCode != 303 || r.Header.Get("Location") != "/u/1" {
		t.Fatalf("first completion: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	r, _ := tfSecondStep(t, noRedirectClient(), ts2.URL, mfa, codes[0])
	if r.StatusCode != 303 || r.Header.Get("Location") != "/" || tfHasSession(r) {
		t.Fatalf("cookie replayed on the other backend: %d %s, session %v", r.StatusCode, r.Header.Get("Location"), tfHasSession(r))
	}
}

func TestNonceExpiresAndDatabaseErrorsFailClosed(t *testing.T) {
	a, _, st := twoBackends(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Second)
	if !a.nonces.use(ctx, "gone", past) {
		t.Fatal("first use refused")
	}
	if a.nonces.seen(ctx, "gone") {
		t.Error("an expired nonce is still seen")
	}
	if !a.nonces.use(ctx, "gone", time.Now().Add(time.Minute)) {
		t.Error("the nonce of an expired value stayed spent")
	}
	if n, err := st.PurgeNonces(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Errorf("purged %d (%v), want 1", n, err)
	}

	st.Close() // every call fails now
	if a.nonces.use(ctx, "x", time.Now().Add(time.Minute)) {
		t.Error("use succeeded without a database")
	}
	if !a.nonces.seen(ctx, "x") {
		t.Error("seen reported false without a database")
	}
	if !a.limiter.Blocked(ctx, "ip:1", 1000) {
		t.Error("the limiter did not block without a database")
	}
}

func TestLockOutIsSharedAcrossBackends(t *testing.T) {
	a, b, _ := twoBackends(t)
	ctx := context.Background()
	for range maxLoginFail - 1 {
		a.limiter.Fail(ctx, loginKey("Alice@example.com"))
	}
	if b.limiter.Blocked(ctx, loginKey("alice@example.com"), maxLoginFail) {
		t.Fatal("blocked one failure early")
	}
	b.limiter.Fail(ctx, loginKey("alice@example.com"))
	if !a.limiter.Blocked(ctx, loginKey("alice@example.com"), maxLoginFail) {
		t.Fatal("A does not see the failures of B")
	}
	a.limiter.Reset(ctx, loginKey("alice@example.com"))
	if b.limiter.Blocked(ctx, loginKey("alice@example.com"), maxLoginFail) {
		t.Fatal("B still blocks after A reset the key")
	}
}

func TestLockOutLeavesTheWindow(t *testing.T) {
	a, _, st := twoBackends(t)
	ctx := context.Background()
	for range 3 {
		a.limiter.Fail(ctx, "ip:old")
	}
	if !a.limiter.Blocked(ctx, "ip:old", 3) {
		t.Fatal("not blocked")
	}
	a.limiter.now = func() time.Time { return time.Now().Add(failWindow + time.Minute) }
	if a.limiter.Blocked(ctx, "ip:old", 3) {
		t.Fatal("still blocked after the window")
	}
	a.purgeState(ctx) // the loop's purge uses the real clock: the rows are fresh, so they stay
	if n, _ := st.CountFailures(ctx, "ip:old", time.Now().Add(-time.Hour)); n != 3 {
		t.Fatalf("%d failures after a purge of fresh rows, want 3", n)
	}
}

// The state of a sync is a job in the database, so every backend shows the same: whichever one
// the page happens to ask, and whichever one a worker reported to.
func TestStrengthLevelStatusIsVisibleFromAnotherBackend(t *testing.T) {
	a, b, st := twoBackends(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetUserStrengthLevelAuto(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if v, err := b.slJobViewOf(ctx, u.ID); v != nil || err != nil {
		t.Fatalf("view before any sync: %v %v", v, err)
	}
	job, created, err := a.enqueueJob(ctx, enqueueSpec{UserID: u.ID, Kind: kindStrengthLevelSync, Origin: models.JobOriginUser, Payload: slSyncPayload{Days: 30}})
	if err != nil || !created {
		t.Fatalf("enqueue: %v %v", created, err)
	}
	// A second sync is not started while one waits or runs, from whichever backend it is asked.
	if _, created, err := b.enqueueJob(ctx, enqueueSpec{UserID: u.ID, Kind: kindStrengthLevelSync, Origin: models.JobOriginUser, Payload: slSyncPayload{Days: 7}}); err != nil || created {
		t.Fatalf("B queued a second sync: created=%v err=%v", created, err)
	}
	if v, _ := b.slJobViewOf(ctx, u.ID); v == nil || !v.Running || !strings.Contains(v.Text, "waiting for a worker") {
		t.Fatalf("B sees %+v of a queued sync", v)
	}
	leased, ok, err := st.LeaseJob(ctx, models.LeaseRequest{Kinds: []string{kindStrengthLevelSync}, Owner: "w", TokenHash: "t", LeaseFor: time.Minute}, time.Now())
	if err != nil || !ok || leased.PublicID != job.PublicID {
		t.Fatalf("lease: %v %v", ok, err)
	}
	if v, _ := b.slJobViewOf(ctx, u.ID); v == nil || !v.Running || !strings.Contains(v.Text, "signing in") {
		t.Fatalf("B sees %+v", v)
	}
	progress := `{"done":3,"total":30}`
	if _, err := st.HeartbeatJob(ctx, job.PublicID, "t", time.Minute, &progress, time.Now()); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.slJobViewOf(ctx, u.ID); v == nil || v.Done != 3 || v.Total != 30 || !strings.Contains(v.Text, "3 of 30 days") {
		t.Fatalf("B sees %+v after progress", v)
	}
	result := `{"added":2,"updated":0,"unchanged":1,"failed":0,"days":30}`
	if _, err := st.CompleteJob(ctx, job.PublicID, "t", models.Completion{Outcome: models.OutcomeOK, Result: result}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.slJobViewOf(ctx, u.ID); v == nil || v.Running || !strings.Contains(v.Text, "added 2") || !strings.Contains(v.Text, "last 30 days") {
		t.Fatalf("B sees %+v after finish", v)
	}
}

// A worker that dies without a word does not leave a sync "running" for good: its lease runs out,
// and whichever backend notices puts the job back in the queue, or ends it when it ran out of tries.
func TestSyncOfAVanishedWorkerIsPutBack(t *testing.T) {
	a, b, st := twoBackends(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, "alice")
	job, _, err := a.enqueueJob(ctx, enqueueSpec{UserID: u.ID, Kind: kindStrengthLevelSync, Origin: models.JobOriginUser, Payload: slSyncPayload{Days: 30}})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC()
	if _, ok, err := st.LeaseJob(ctx, models.LeaseRequest{Kinds: []string{kindStrengthLevelSync}, Owner: "w", TokenHash: "t", LeaseFor: time.Minute}, t0); !ok || err != nil {
		t.Fatalf("lease: %v %v", ok, err)
	}
	// The worker is gone: no heartbeat for two minutes. Another backend's sweep finds it.
	b.lastSweep.Store(0)
	reaped, err := st.ReapExpiredLeases(ctx, t0.Add(2*time.Minute), b.kindBackoff)
	if err != nil || len(reaped) != 1 || reaped[0].PublicID != job.PublicID || reaped[0].State != models.JobQueued {
		t.Fatalf("reap: %+v %v", reaped, err)
	}
	if v, _ := a.slJobViewOf(ctx, u.ID); v == nil || !v.Running || !strings.Contains(v.Text, "waiting for a worker") {
		t.Fatalf("A sees %+v", v)
	}
}

func TestSelfSignedCertificateIsMadeOnlyByTheBackendThatMayGenerate(t *testing.T) {
	ctx := context.Background()
	st, err := sqlstore.Open(ctx, sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.PutSetting(ctx, settingTLSMode, apiclient.TLSSelfSigned); err != nil {
		t.Fatal(err)
	}
	restricted, err := NewBackend(st, testBody(st), WithoutCertificateGeneration())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restricted.Close)
	if restricted.cfg().tlsCert != nil {
		t.Fatal("the restricted backend made a certificate")
	}
	if c, _ := st.GetSetting(ctx, settingTLSCert); c != "" {
		t.Fatal("a certificate was stored by the restricted backend")
	}
	restricted.renewCertificate(ctx) // maintenance does not run on it either
	if restricted.cfg().tlsCert != nil {
		t.Fatal("the restricted backend generated in its maintenance round")
	}

	maintainer, err := NewBackend(st, testBody(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(maintainer.Close)
	if maintainer.cfg().tlsInfo == nil {
		t.Fatal("the backend that may generate has no certificate")
	}
	if err := restricted.ReloadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	if restricted.cfg().tlsInfo == nil || restricted.cfg().tlsInfo.Fingerprint != maintainer.cfg().tlsInfo.Fingerprint {
		t.Fatal("the restricted backend did not load the stored certificate")
	}

	// An administrator's explicit change still generates, wherever it is served.
	body := `{"Mode":"` + apiclient.TLSSelfSigned + `","Names":"changed.example.com"}`
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	restricted.internalAdminSaveTLS(w, r, models.User{})
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	info := restricted.cfg().tlsInfo
	if info == nil || !sameNames(info.Names, []string{"changed.example.com"}) || info.Fingerprint == maintainer.cfg().tlsInfo.Fingerprint {
		t.Fatalf("the admin save did not generate a new certificate: %+v", info)
	}
}
