package mainbackend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func TestSignInsAreAudited(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"root@example.com"}, "password": {"wrong password"}})

	events, err := st.ListAuditEvents(t.Context(), store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]models.AuditEvent{}
	for _, e := range events {
		seen[e.Action] = e
	}
	for _, want := range []string{auditSignup, auditLogin, auditLoginFailed} {
		if _, ok := seen[want]; !ok {
			t.Errorf("no %q event in %v", want, events)
		}
	}
	if e := seen[auditLoginFailed]; strings.Contains(e.Detail, "root") || e.ActorID != nil {
		t.Errorf("failed login records who tried: %+v", e)
	}
	if seen[auditLogin].IP == "" {
		t.Error("login event has no IP address")
	}
}

func TestFailedSignInsDoNotFloodTheLog(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	for i := 0; i < 5; i++ {
		post(t, noRedirectClient(), ts.URL+"/login", url.Values{"email": {"root@example.com"}, "password": {"wrong password"}})
	}
	events, _ := st.ListAuditEvents(t.Context(), store.AuditFilter{})
	failed := 0
	for _, e := range events {
		if e.Action == auditLoginFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("%d failed-login events for 5 attempts from one address, want 1", failed)
	}
}

func TestAuditNoiseCountsWhatItSkips(t *testing.T) {
	n := newAuditNoise()
	t0 := time.Now()
	if ok, _ := n.allow("k", t0); !ok {
		t.Fatal("first event dropped")
	}
	for i := 0; i < 3; i++ {
		if ok, _ := n.allow("k", t0.Add(time.Second)); ok {
			t.Fatal("event inside the window recorded")
		}
	}
	if ok, skipped := n.allow("k", t0.Add(2*time.Minute)); !ok || skipped != 3 {
		t.Errorf("after the window: %v, skipped %d", ok, skipped)
	}
}

func TestExportsAreOwnDataOnlyAndHaveNoSecrets(t *testing.T) {
	ts, alice, st := newAuthServer(t)
	signup(t, ts, alice, "alice")
	day, _ := store.ParseDay("2026-01-02")
	if _, err := st.UpsertScaleReadings(t.Context(), 1, []models.ScaleReading{{MeasuredAt: day, Source: "manual", Method: "scale"}}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/u/1/export/profile", "/u/1/export/data"} {
		resp, err := alice.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, resp.Header)
		}
		text := b.String()
		if !json.Valid([]byte(text)) {
			t.Fatalf("%s is not JSON:\n%s", path, text)
		}
		for _, secret := range []string{"$2a$", "$2b$", "token_hash", "secret", "password"} {
			if strings.Contains(strings.ToLower(text), strings.ToLower(secret)) {
				t.Errorf("%s contains %q:\n%s", path, secret, text)
			}
		}
		if path == "/u/1/export/profile" && !strings.Contains(text, `"email": "alice@example.com"`) {
			t.Errorf("profile export lacks the e-mail address:\n%s", text)
		}
		if path == "/u/1/export/data" && !strings.Contains(text, "2026-01-02") {
			t.Errorf("data export lacks the reading:\n%s", text)
		}
	}
	// The export itself is audited, and shows up in the next profile export.
	profile := string(mustGet(t, alice, ts.URL+"/u/1/export/profile"))
	if !strings.Contains(profile, auditExportData) {
		t.Errorf("export not in the audit log:\n%s", profile)
	}

	anon := noRedirectClient()
	if code, _ := body(t, anon, ts.URL+"/u/1/export/data"); code == 200 {
		t.Error("anonymous export allowed")
	}
	signup(t, ts, anon, "bob") // a second account
	if code, _ := body(t, anon, ts.URL+"/u/1/export/profile"); code != http.StatusNotFound {
		t.Errorf("exporting someone else's data: %d", code)
	}
}

func TestAuditViewsRespectWho(t *testing.T) {
	ts, admin, _ := newAuthServer(t)
	signup(t, ts, admin, "root")
	if _, body := body(t, admin, ts.URL+"/admin"); !strings.Contains(body, "Audit log") || !strings.Contains(body, "Account created") {
		t.Errorf("admin page lacks the log:\n%s", body)
	}
	if code, body := body(t, admin, ts.URL+"/api/v1/audit"); code != 200 || !strings.Contains(body, `"action":"signup"`) {
		t.Errorf("admin audit API: %d %s", code, body)
	}
	if _, body := body(t, admin, ts.URL+"/u/1/settings"); !strings.Contains(body, "Recent account activity") {
		t.Errorf("settings page lacks the activity:\n%s", body)
	}

	other := noRedirectClient()
	signup(t, ts, other, "bob")
	if code, _ := body(t, other, ts.URL+"/api/v1/audit"); code != http.StatusForbidden {
		t.Errorf("non-admin read the whole log: %d", code)
	}
	if code, _ := body(t, other, ts.URL+"/api/v1/users/1/audit"); code != http.StatusNotFound {
		t.Errorf("read another user's log: %d", code)
	}
}

func TestSignupIsClosedByDefault(t *testing.T) {
	ts, _ := newProxyServer(t, "", "") // no sign-up setting stored
	signupVia(t, ts, nil)              // the first account can always be created
	resp := post(t, noRedirectClient(), ts.URL+"/signup", url.Values{"username": {"bob"}, "email": {"bob@example.com"}, "password": {testPassword}, "confirm": {testPassword}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second sign-up on a default server: %d", resp.StatusCode)
	}
}
