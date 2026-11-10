package mainbackend

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/store/sqlstore"
)

func TestABackendThatCannotStartSaysWhy(t *testing.T) {
	open := func(t *testing.T) *flakyStore {
		t.Helper()
		st, err := sqlstore.Open(context.Background(), sqlstore.DriverSQLite, "file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return &flakyStore{Store: st}
	}
	t.Run("without a body service", func(t *testing.T) {
		if _, err := NewBackend(open(t)); err == nil {
			t.Fatal("a backend started without a body service")
		}
	})
	t.Run("when the cookie key cannot be read", func(t *testing.T) {
		st := open(t)
		st.failOn("GetSetting:" + settingCookieKey)
		if b, err := NewBackend(st, testBody(st)); err == nil {
			b.Close()
			t.Fatal("a backend started without its cookie key")
		}
	})
	t.Run("when the settings cannot be read", func(t *testing.T) {
		st := open(t)
		st.failOn("GetSetting:" + settingBaseURL)
		if b, err := NewBackend(st, testBody(st)); err == nil {
			b.Close()
			t.Fatal("a backend started without its settings")
		}
	})
}

func TestTheSettingsPollSaysWhatItCannotDo(t *testing.T) {
	lines := captureLog(t)
	old := settingsPollInterval
	settingsPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { settingsPollInterval = old })
	r := newJobRig(t)
	r.b.StartSettingsPoll()
	r.flaky.failOn("SettingsGeneration")
	lines.waitFor(t, "could not check for changed settings")
	// Another process changes a setting this backend then cannot read.
	r.flaky.heal()
	r.flaky.failOn("GetSetting:" + settingBaseURL)
	if err := r.st.PutSetting(context.Background(), settingAuditRetention, "7"); err != nil {
		t.Fatal(err)
	}
	lines.waitFor(t, "reload the changed settings")
}

func TestTheBackendListenerUnderstandsTheProxyProtocolWhenTheSettingsSayTo(t *testing.T) {
	r := newJobRig(t)
	trusted, err := ParseTrustedProxies([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	l := *r.b.cfg()
	l.proxyProtocol, l.trusted = true, trusted
	r.b.swapSettings(&l)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := r.b.WrapListener(ln)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		host, _, _ := net.SplitHostPort(req.RemoteAddr)
		w.Write([]byte(host))
	})}
	go srv.Serve(wrapped)
	t.Cleanup(func() { srv.Close() })
	if code, body := rawHTTP(t, wrapped.Addr().String(), v1Line, "GET", "/", ""); code != 200 || body != "203.0.113.7" {
		t.Fatalf("a connection with a PROXY header: %d %q, want the address in the header", code, body)
	}
}

func TestABackendWithoutASlotCountUsesTheDefault(t *testing.T) {
	var b Backend
	if got := b.publicInFlightOrDefault(); got != publicMaxInFlight {
		t.Fatalf("%d slots, want %d", got, publicMaxInFlight)
	}
}

func TestTheBackoffOfAFailedJobIsBoundedBothWays(t *testing.T) {
	short := kindSettings{RetryBase: 0}
	if got := short.backoff(1); got < 800*time.Millisecond || got > 1200*time.Millisecond {
		t.Errorf("backoff with no base: %v, want about a second", got)
	}
	long := kindSettings{RetryBase: time.Hour}
	if got := long.backoff(1); got > maxRetryDelay+maxRetryDelay/5 {
		t.Errorf("backoff above the cap: %v", got)
	}
	doubling := kindSettings{RetryBase: time.Minute}
	if got := doubling.backoff(30); got > maxRetryDelay+maxRetryDelay/5 {
		t.Errorf("backoff after many attempts: %v", got)
	}
}

func TestStoredWorkerSettingsThatAreNonsenseFallBackToTheDefaults(t *testing.T) {
	lines := captureLog(t)
	r := newJobRig(t)
	name := kindSettingName(kindWithingsImport, fieldMaxAttempts)
	if err := r.st.PutSetting(context.Background(), name, "lots"); err != nil {
		t.Fatal(err)
	}
	if err := r.b.reloadSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := r.b.kinds[kindWithingsImport].Defaults.MaxAttempts
	if got := r.b.cfg().workers.Kinds[kindWithingsImport].MaxAttempts; got != want {
		t.Fatalf("attempts %d, want the default %d", got, want)
	}
	if !lines.has("ignoring an invalid setting") {
		t.Fatal("the log never said the setting was ignored")
	}
}

func TestWorkerSettingsThatCannotBeReadAreNotPutInForce(t *testing.T) {
	fields := []string{fieldMaxRunning, fieldMaxAttempts, fieldRetryBase, fieldTimeout, fieldScheduleHours, fieldPauseAfter, fieldBreaker,
		fieldEnabled, fieldPausedReason, fieldBreakerSince}
	var names []string
	for _, f := range fields {
		names = append(names, kindSettingName(kindStrengthLevelSync, f))
	}
	names = append(names, optionSettingName(kindStrengthLevelSync, optionRequestDelay))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			r := newJobRig(t)
			r.flaky.failOn("GetSetting:" + name)
			if err := r.b.reloadSettings(context.Background()); err == nil {
				t.Fatal("the settings were loaded although one could not be read")
			}
		})
	}
}

func TestDeletingAUserWhoseDataCannotBeRemovedYetIsStillDone(t *testing.T) {
	lines := captureLog(t)
	runAdminChanges(t, []adminChange{
		{name: "the body service is down", setup: func(t *testing.T, e *adminEnv) { e.useBody(bodyDown(t)) }, path: "/admin/users/2/delete", form: url.Values{}, wantFlash: "bob and their data were deleted"},
	})
	if !lines.has("could not remove a deleted user's readings and scans yet") {
		t.Fatal("the log never said the data stays until the sweep")
	}
}
