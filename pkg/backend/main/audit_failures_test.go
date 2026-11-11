package mainbackend

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func (f *flakyStore) PurgeAuditEvents(ctx context.Context, before time.Time) (int64, error) {
	if f.fails("PurgeAuditEvents") {
		return 0, errFlaky
	}
	return f.Store.PurgeAuditEvents(ctx, before)
}

func (f *flakyStore) PurgeNonces(ctx context.Context, before time.Time) (int64, error) {
	if f.fails("PurgeNonces") {
		return 0, errFlaky
	}
	return f.Store.PurgeNonces(ctx, before)
}

func (f *flakyStore) PurgeFailures(ctx context.Context, before time.Time) (int64, error) {
	if f.fails("PurgeFailures") {
		return 0, errFlaky
	}
	return f.Store.PurgeFailures(ctx, before)
}

// logSink is where the log of the whole test binary goes (see TestMain): standard error, or the
// buffer of the test that captures it.
var logSink = &swapSink{w: os.Stderr}

type swapSink struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *swapSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// set makes w the destination and returns the one it replaced.
func (s *swapSink) set(w io.Writer) io.Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.w
	s.w = w
	return old
}

// logLines is what the server logged, for tests that look for the line an operator would read.
type logLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logLines) has(text string) bool {
	return strings.Contains(l.String(), text)
}

// captureLog sends the log to the returned buffer until the test ends. Call it before building the
// backend, so that the backend's goroutines are stopped before the log is put back.
func captureLog(t *testing.T) *logLines {
	t.Helper()
	l := &logLines{}
	old := logSink.set(l)
	t.Cleanup(func() { logSink.set(old) })
	return l
}

func (l *logLines) waitFor(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !l.has(text) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never said %q", text)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTheMaintenanceLoopPurgesAndSaysWhatFails(t *testing.T) {
	t.Run("old audit events are purged and the purge is logged", func(t *testing.T) {
		lines := captureLog(t)
		r := newJobRig(t)
		ctx := context.Background()
		if err := r.st.PutSetting(ctx, settingAuditRetention, "1"); err != nil {
			t.Fatal(err)
		}
		if err := r.b.reloadSettings(ctx); err != nil {
			t.Fatal(err)
		}
		r.b.audit(httptest.NewRequest("GET", "/", nil), 0, auditSettingChanged, "old")
		if err := r.st.ExecForTests(ctx, "UPDATE audit_log SET created_at = '2000-01-01 00:00:00'"); err != nil {
			t.Fatal(err)
		}
		r.b.StartMaintenance()
		lines.waitFor(t, "purged old audit events")
		if events, _ := r.st.ListAuditEvents(ctx, store.AuditFilter{}); len(events) != 0 {
			t.Fatalf("%d events are left", len(events))
		}
	})
	t.Run("a purge that fails is logged", func(t *testing.T) {
		lines := captureLog(t)
		r := newJobRig(t)
		if err := r.st.PutSetting(context.Background(), settingAuditRetention, "1"); err != nil {
			t.Fatal(err)
		}
		if err := r.b.reloadSettings(context.Background()); err != nil {
			t.Fatal(err)
		}
		r.flaky.failOn("PurgeAuditEvents", "PurgeNonces", "PurgeFailures")
		r.b.StartMaintenance()
		lines.waitFor(t, "purge audit log")
		lines.waitFor(t, "purge used nonces")
		lines.waitFor(t, "purge failed attempts")
	})
	t.Run("a sweep for orphaned readings that fails is logged", func(t *testing.T) {
		lines := captureLog(t)
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyWithOrphan(t, 500)}
		r.flaky.failOn("ListUsers")
		r.b.StartMaintenance()
		lines.waitFor(t, "look for readings and scans of deleted users")
	})
	t.Run("a round is run again when the interval has passed", func(t *testing.T) {
		old := auditPurgeEvery
		auditPurgeEvery = 20 * time.Millisecond
		t.Cleanup(func() { auditPurgeEvery = old })
		r := newJobRig(t)
		r.b.StartMaintenance()
		deadline := time.Now().Add(10 * time.Second)
		for r.flaky.called("PurgeNonces") < 3 {
			if time.Now().After(deadline) {
				t.Fatal("the maintenance loop did not run again")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// bodyWithOrphan is a body service that holds the data of user 5, who does not exist, and answers
// the delete of it with the given status.
func bodyWithOrphan(t *testing.T, deleteStatus int) *bodyapi.Client {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			w.Write([]byte(`[5]`))
			return
		}
		w.WriteHeader(deleteStatus)
		w.Write([]byte(`{"code":"internal","message":"no"}`))
	}))
	t.Cleanup(ts.Close)
	_, _, priv := bodyapi.GenerateKey()
	c, err := bodyapi.New(ts.URL, bodyapi.HTTPClient(nil), "a-body-secret-of-this-test-0123456789", bodyapi.NewSigner(priv))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheSweepForOrphanedReadings(t *testing.T) {
	ctx := context.Background()
	t.Run("a body service that is down is reported", func(t *testing.T) {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyDown(t)}
		if err := r.b.sweepBodyOrphans(ctx); err == nil {
			t.Fatal("a sweep without the body service succeeded")
		}
	})
	t.Run("a body service without data has nothing to sweep", func(t *testing.T) {
		r := newJobRig(t)
		if err := r.b.sweepBodyOrphans(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a user list that cannot be read is reported", func(t *testing.T) {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyWithOrphan(t, 204)}
		r.flaky.failOn("ListUsers")
		if err := r.b.sweepBodyOrphans(ctx); err == nil {
			t.Fatal("a sweep without the user list succeeded")
		}
	})
	t.Run("the data of a user who is gone is removed, and a delete that fails is reported", func(t *testing.T) {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyWithOrphan(t, 204)}
		if err := r.b.sweepBodyOrphans(ctx); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		r.b.body = bodyService{c: bodyWithOrphan(t, 500)}
		if err := r.b.sweepBodyOrphans(ctx); err == nil {
			t.Fatal("a delete that failed was not reported")
		}
	})
	t.Run("the data of a user who exists is kept", func(t *testing.T) {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyWithOrphan(t, 500)} // a delete would fail: it must not be tried
		for _, name := range []string{"cc", "dd", "ee"} { // ids 3, 4 and 5
			if _, err := r.st.CreateUser(ctx, name); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.b.sweepBodyOrphans(ctx); err != nil {
			t.Fatalf("a user's data was swept: %v", err)
		}
	})
}

func TestTheAuditLogNeverFailsTheRequestThatCausedAnEntry(t *testing.T) {
	lines := captureLog(t)
	r := newJobRig(t)
	breakTable(t, r.st, "audit_log")
	req := httptest.NewRequest("GET", "/", nil)
	r.b.audit(req, 2, auditSettingChanged, "x")
	lines.waitFor(t, "audit log write failed")
	lines2 := captureLog(t)
	r.b.auditBackground(2, auditConnect, "x")
	lines2.waitFor(t, "audit log write failed")
}

func TestAnAuditEntryOfAnUnknownKindIsShownByItsName(t *testing.T) {
	got := auditLines([]models.AuditEvent{{Action: "something_new"}}, func(int64) string { return "x" })
	if len(got) != 1 || got[0].Action != "something_new" {
		t.Fatalf("lines %+v", got)
	}
}

func TestAFloodOfFailedSignInsIsNotRecordedEveryTime(t *testing.T) {
	t.Run("the events left out are counted in the next one", func(t *testing.T) {
		r := newJobRig(t)
		req := httptest.NewRequest("POST", "/login", nil)
		req.RemoteAddr = "192.0.2.9:4000"
		key := auditLoginFailed + "|" + clientIP(req)
		r.b.auditNoise.seen[key] = noiseEntry{last: time.Now().Add(-2 * time.Minute), skipped: 3}
		r.b.auditAs(req, 0, 0, auditLoginFailed, "")
		events, err := r.st.ListAuditEvents(context.Background(), store.AuditFilter{})
		if err != nil || len(events) != 1 || events[0].Detail != "plus 3 more in the last minutes" {
			t.Fatalf("events %+v err %v", events, err)
		}
	})
	t.Run("the memory of addresses is bounded", func(t *testing.T) {
		n := newAuditNoise()
		now := time.Now()
		for i := range auditNoiseMax {
			n.allow("k"+string(rune('a'+i%26))+time.Duration(i).String(), now)
		}
		if got := len(n.seen); got != auditNoiseMax {
			t.Fatalf("%d addresses remembered, want %d", got, auditNoiseMax)
		}
		n.allow("one more", now) // everything is recent: the memory is cleared to make room
		if got := len(n.seen); got != 1 {
			t.Fatalf("%d addresses remembered after the clear, want 1", got)
		}
		for i := range 100 {
			n.seen["old"+time.Duration(i).String()] = noiseEntry{last: now.Add(-time.Hour)}
		}
		for i := len(n.seen); i < auditNoiseMax; i++ {
			n.seen["fill"+time.Duration(i).String()] = noiseEntry{last: now}
		}
		n.allow("another", now) // the old ones are forgotten first
		if _, ok := n.seen["old"+time.Duration(0).String()]; ok {
			t.Fatal("an old address was kept while the memory was full")
		}
	})
}

func TestTheAuditLogOfThePublicAPI(t *testing.T) {
	r := newJobRig(t)
	r.b.audit(httptest.NewRequest("GET", "/", nil), 2, auditSettingChanged, "x")
	if s, b, _ := r.call("GET", "/audit", ""); s != 200 || !strings.Contains(b, "server_setting_changed") {
		t.Fatalf("own audit log: %d %.200q", s, b)
	}
	if s, b := r.callAs(r.rootToken(), "GET", "/audit?limit=5", ""); s != 200 {
		t.Fatalf("everybody's audit log: %d %.200q", s, b)
	}
	breakTable(t, r.st, "audit_log")
	if s, _, _ := r.call("GET", "/audit", ""); s != 500 {
		t.Fatalf("audit log without its table: %d, want 500", s)
	}
}
