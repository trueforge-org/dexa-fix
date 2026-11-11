package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
)

func (f *flakyStore) HasUsers(ctx context.Context) (bool, error) {
	if f.fails("HasUsers") {
		return false, errFlaky
	}
	return f.Store.HasUsers(ctx)
}

func (f *flakyStore) ListPasskeys(ctx context.Context, userID int64) ([]models.Passkey, error) {
	if f.fails("ListPasskeys") {
		return nil, errFlaky
	}
	return f.Store.ListPasskeys(ctx, userID)
}

func (f *flakyStore) CountRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	if f.fails("CountRecoveryCodes") {
		return 0, errFlaky
	}
	return f.Store.CountRecoveryCodes(ctx, userID)
}

func (f *flakyStore) GetIdentity(ctx context.Context, provider, subject string) (models.Identity, error) {
	if f.fails("GetIdentity") {
		return models.Identity{}, errFlaky
	}
	return f.Store.GetIdentity(ctx, provider, subject)
}

func TestAProviderNameIsShownWithACapital(t *testing.T) {
	if got := label("withings"); got != "Withings" {
		t.Errorf("label(withings) = %q", got)
	}
	if got := label(""); got != "" {
		t.Errorf("label of nothing = %q", got)
	}
}

func TestAMessageThatIsTooLongForAFlashIsCut(t *testing.T) {
	long := strings.Repeat("x", maxFlashLen+50)
	if got := settingsPath(1, long); len(got) > len("/u/1/settings?msg=")+maxFlashLen {
		t.Errorf("settings path %d bytes", len(got))
	}
	r := httptest.NewRequest("GET", "/?msg="+long, nil)
	if got := flashParam(r); len(got) != maxFlashLen {
		t.Errorf("flash %d bytes, want %d", len(got), maxFlashLen)
	}
}

func TestTheStatusPageSaysWhatIsDown(t *testing.T) {
	old := statusKeep
	statusKeep = 0
	t.Cleanup(func() { statusKeep = old })
	state := func(d *apiclient.StatusDetail, name string) string {
		for _, c := range d.Components {
			if c.Name == name {
				return c.State
			}
		}
		return "missing"
	}
	t.Run("a store that cannot say it answers is down, and one that does is ready", func(t *testing.T) {
		r := newJobRig(t)
		if err := r.b.Ready(context.Background()); err != nil {
			t.Fatalf("ready: %v", err)
		}
		r.flaky.failOn("HasUsers")
		if err := r.b.Ready(context.Background()); err == nil {
			t.Fatal("ready although the database does not answer")
		}
		d, err := r.api.AdminStatus(r.root)
		if err != nil || state(d, "database") != apiclient.StateDown {
			t.Fatalf("status %+v err %v", d, err)
		}
	})
	t.Run("a body service that is down and providers without workers", func(t *testing.T) {
		r := newJobRig(t)
		r.b.body = bodyService{c: bodyDown(t)}
		d, err := r.api.AdminStatus(r.root)
		if err != nil {
			t.Fatal(err)
		}
		if state(d, "body") != apiclient.StateDown || state(d, "workers.withings") != apiclient.StateOff || d.Notes["body"] == "" {
			t.Fatalf("status %+v", d)
		}
	})
	t.Run("Valkey that stops answering is down", func(t *testing.T) {
		ts, _, mr := newValkeyServer(t)
		b := serverOf(ts).Backend
		d := b.checkStatus(context.Background())
		if d.components == nil {
			t.Fatal("no status")
		}
		up := false
		for _, c := range d.components {
			up = up || (c.Name == "valkey" && c.State == apiclient.StateUp)
		}
		if !up {
			t.Fatalf("Valkey not up: %+v", d.components)
		}
		mr.Close()
		for _, c := range b.checkStatus(context.Background()).components {
			if c.Name == "valkey" && c.State != apiclient.StateDown {
				t.Fatalf("Valkey state %q after it stopped, want down", c.State)
			}
		}
	})
}

func TestTheFrontendConfigIsNotServedWhenItCannotBeRead(t *testing.T) {
	cert, key := certAndKey(t)
	for _, name := range []string{"SettingsGeneration", "GetSetting:" + settingTLSCert, "GetSetting:" + settingTLSKey} {
		t.Run(name, func(t *testing.T) {
			r := newJobRig(t)
			ctx := context.Background()
			for n, v := range map[string]string{settingTLSMode: apiclient.TLSUpload, settingTLSCert: cert, settingTLSKey: key} {
				if err := r.st.PutSetting(ctx, n, v); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.b.reloadSettings(ctx); err != nil {
				t.Fatal(err)
			}
			r.flaky.failOn(name)
			req := httptest.NewRequest("GET", "/", nil)
			if _, err := r.b.frontendConfig(req); err == nil {
				t.Fatal("a configuration was served that could not be read")
			}
			rec := httptest.NewRecorder()
			r.b.internalFrontendConfig(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500", rec.Code)
			}
		})
	}
}

func TestTheAnalyticsSettingsTellTheAdministratorWhatHappenedOrFailCleanly(t *testing.T) {
	good := analyticsForm(testScarf, testGA, testAds)
	runAdminChanges(t, []adminChange{
		{name: "an analytics setting that cannot be stored", setup: failing("PutSetting:" + settingScarfPixel), path: "/admin/analytics", form: good},
		{name: "analytics settings that cannot be applied", setup: failing("SettingsGeneration"), path: "/admin/analytics", form: good},
	})
	t.Run("a stored value that is not valid switches the analytics off", func(t *testing.T) {
		e := newAdminEnv(t)
		if err := e.st.PutSetting(context.Background(), settingScarfPixel, "not an id"); err != nil {
			t.Fatal(err)
		}
		if err := e.backend.reloadSettings(context.Background()); err != nil {
			t.Fatal(err)
		}
		if e.backend.cfg().analytics.Any() {
			t.Fatal("analytics are on with a stored value that is not valid")
		}
	})
}

func TestTheSettingsPageFailsWhenAnyPartOfItCannotBeRead(t *testing.T) {
	cases := map[string]func(*testing.T, *jobRig){
		"the sign-in methods": func(t *testing.T, r *jobRig) { breakTable(t, r.st, "user_identities") },
		"the passkeys":        func(t *testing.T, r *jobRig) { r.flaky.failOn("ListPasskeys") },
		"the second step":     func(t *testing.T, r *jobRig) { r.flaky.failOn("GetTOTP") },
		"the recovery codes": func(t *testing.T, r *jobRig) {
			r.flaky.failOn("GetTOTP:confirmed", "CountRecoveryCodes") // a second step that is set up, whose codes cannot be counted
		},
		"the providers": func(t *testing.T, r *jobRig) { breakTable(t, r.st, "auth_providers") },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			r := newJobRig(t)
			breakIt(t, r)
			if got := r.raw("GET", "/users/2/pages/settings", ""); got != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500", got)
			}
		})
	}
}

func TestAReauthenticationThatCannotBeCheckedIsAServerFailure(t *testing.T) {
	r := newJobRig(t)
	r.flaky.failOn("GetIdentity")
	u, _ := r.st.GetUser(context.Background(), 2)
	req := httptest.NewRequest("POST", "/", nil)
	if ok, _, _, err := r.b.checkReauth(req, u, false); ok || err == nil {
		t.Fatalf("ok=%v err=%v, want a failure", ok, err)
	}
}

type brokenState struct{ StateStore }

func (brokenState) CountFailures(context.Context, string, time.Time) (int, error) {
	return 0, errFlaky
}
func (brokenState) RecordFailure(context.Context, string, time.Time) error { return errFlaky }
func (brokenState) ClearFailures(context.Context, string) error            { return errFlaky }

func TestTheFailureCounterFailsClosedAndSaysSo(t *testing.T) {
	lines := captureLog(t)
	l := newFailureLimiter(brokenState{})
	ctx := context.Background()
	if !l.Blocked(ctx, "k", 5) {
		t.Fatal("an address was let in although its failures cannot be counted")
	}
	l.Fail(ctx, "k")
	l.Reset(ctx, "k")
	for _, want := range []string{"count failed attempts", "record a failed attempt", "clear failed attempts"} {
		if !lines.has(want) {
			t.Errorf("the log never said %q", want)
		}
	}
}
