package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func (f *flakyStore) ListAccessTokens(ctx context.Context, userID int64) ([]models.AccessToken, error) {
	if f.fails("ListAccessTokens") {
		return nil, errFlaky
	}
	return f.Store.ListAccessTokens(ctx, userID)
}

func (f *flakyStore) GetTOTP(ctx context.Context, userID int64) (models.TOTP, error) {
	if f.fails("GetTOTP") {
		return models.TOTP{}, errFlaky
	}
	return f.Store.GetTOTP(ctx, userID)
}

func TestExportsAreDownloadsAndAreAudited(t *testing.T) {
	r := newJobRig(t)
	r.connectWithings()
	r.saveStrengthLevelLogin()
	for _, kind := range []string{"profile", "data"} {
		req, _ := http.NewRequest("GET", r.pub.URL+"/api/v1/users/2/export/"+kind, nil)
		req.Header.Set("Authorization", "Bearer "+r.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), `attachment; filename="dexafix-`+kind+`-user2-`) {
			t.Fatalf("%s export: %d %q", kind, resp.StatusCode, resp.Header.Get("Content-Disposition"))
		}
	}
	events, err := r.st.ListAuditEvents(context.Background(), store.AuditFilter{UserID: 2})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Action] = true
	}
	if !seen[auditExportProfile] || !seen[auditExportData] {
		t.Fatalf("both exports should be in the audit log: %v", seen)
	}
	// The profile says what is connected, and never the password of the Strength Level login.
	_, profile, _ := r.call("GET", "/export/profile", "")
	if !strings.Contains(profile, `"email": "a@b.c"`) || strings.Contains(profile, `"password"`) || strings.Contains(profile, `"pw"`) {
		t.Fatalf("profile export: %s", profile)
	}
}

func TestAnExportThatCannotBeBuiltIsAnErrorAndNotAHalfFile(t *testing.T) {
	scenarios := []struct {
		name    string
		path    string
		breakIt func(*testing.T, *jobRig)
		want    int
	}{
		{"the identities table is gone", "/export/profile", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "user_identities") }, 500},
		{"the second step cannot be read", "/export/profile", func(t *testing.T, r *jobRig) { r.flaky.failOn("GetTOTP") }, 500},
		{"the passkeys table is gone", "/export/profile", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "passkeys") }, 500},
		{"the access tokens cannot be listed", "/export/profile", func(t *testing.T, r *jobRig) { r.flaky.failOn("ListAccessTokens") }, 500},
		{"the audit log table is gone", "/export/profile", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "audit_log") }, 500},
		{"the Strength Level login cannot be read", "/export/profile", func(t *testing.T, r *jobRig) { breakTable(t, r.st, "user_integrations") }, 500},
		{"the body service is down", "/export/data", func(t *testing.T, r *jobRig) { r.b.body = bodyService{c: bodyDown(t)} }, 503},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			r := newJobRig(t)
			sc.breakIt(t, r)
			req, _ := http.NewRequest("GET", r.pub.URL+"/api/v1/users/2"+sc.path, nil)
			req.Header.Set("Authorization", "Bearer "+r.token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != sc.want || resp.Header.Get("Content-Disposition") != "" {
				t.Fatalf("status %d, disposition %q, want %d and no file", resp.StatusCode, resp.Header.Get("Content-Disposition"), sc.want)
			}
		})
	}
}

func TestAnExportThatCannotBeEncodedSendsNoFile(t *testing.T) {
	rec := httptest.NewRecorder()
	sendExport(rec, "data", 7, make(chan int)) // a value JSON cannot hold
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Disposition") != "" || !strings.Contains(rec.Body.String(), "internal error") {
		t.Fatalf("status %d, disposition %q, body %q", rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String())
	}
}
