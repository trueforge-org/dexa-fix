package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/apiclient"
	"github.com/trueforge-org/dexa-fix/pkg/importer"
	"github.com/trueforge-org/dexa-fix/pkg/importer/withings"
)

// integrationBackend starts a backend with a configured Withings provider on a real connection and
// two signed-in users, with the session cookies of both.
func integrationBackend(t *testing.T) (*apiclient.Client, string, string) {
	t.Helper()
	cfg := withings.Config{ClientID: "id", ClientSecret: "s", RedirectURL: "http://app/integrations/withings/callback"}
	ts, _, st := newServerAndStore(t, withings.New(cfg, nil))
	var sessions []string
	for _, n := range []string{"one", "two"} {
		form := url.Values{"name": {n}, "email": {n + "@example.com"}, "password": {testPassword}, "confirm": {testPassword}}
		req, _ := http.NewRequest("POST", ts.URL+"/signup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := noRedirectClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		sessions = append(sessions, sessionCookie+"="+sessionCookieOf(t, resp).Value)
	}
	srv, err := New(st, importer.NewRegistry(withings.New(cfg, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.SetServiceSecrets("current-secret-0123456789abcdef")
	bs := httptest.NewServer(srv.InternalHandler())
	t.Cleanup(bs.Close)
	return splitClient(t, bs), sessions[0], sessions[1]
}

func TestSplitIntegrationStateCookieReachesTheRelay(t *testing.T) {
	c, one, _ := integrationBackend(t)
	rec := httptest.NewRecorder()
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: one, Secure: true, Relay: rec})
	target, err := c.ConnectIntegration(ctx, 1, "withings")
	if err != nil || !strings.Contains(target, "state=") {
		t.Fatalf("target %q err %v", target, err)
	}
	ck := relayedCookie(rec, oauthCookiePrefix+"withings")
	if ck == nil || !ck.HttpOnly || !ck.Secure || !strings.HasPrefix(ck.Value, "1.") {
		t.Fatalf("state cookie %+v", ck)
	}
	state := strings.TrimPrefix(ck.Value, "1.")
	if !strings.Contains(target, state) {
		t.Errorf("the authorize URL does not carry the state")
	}

	// The callback with a wrong state is refused, and the state cookie is cleared either way.
	back := httptest.NewRecorder()
	bctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: one + "; " + ck.Name + "=" + ck.Value, Relay: back})
	_, err = c.IntegrationCallback(bctx, "withings", "wrong", "code", "")
	if apiclient.StatusOf(err) != http.StatusBadRequest {
		t.Fatalf("wrong state: %v", err)
	}
	if len(back.Header().Values("Set-Cookie")) == 0 {
		t.Error("the state cookie was not cleared")
	}
}

func TestSplitIntegrationCallbackRefusesAnotherUser(t *testing.T) {
	c, one, two := integrationBackend(t)
	rec := httptest.NewRecorder()
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: one, Relay: rec})
	if _, err := c.ConnectIntegration(ctx, 1, "withings"); err != nil {
		t.Fatal(err)
	}
	ck := relayedCookie(rec, oauthCookiePrefix+"withings")
	state := strings.TrimPrefix(ck.Value, "1.")
	other := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: two + "; " + ck.Name + "=" + ck.Value})
	if _, err := c.IntegrationCallback(other, "withings", state, "code", ""); apiclient.StatusOf(err) != http.StatusForbidden {
		t.Errorf("another user's callback: %v", err)
	}
	// The provider reporting an error ends in a message for the user who started the flow.
	own := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: one + "; " + ck.Name + "=" + ck.Value})
	f, err := c.IntegrationCallback(own, "withings", state, "", "access_denied")
	if err != nil || f.UserID != 1 || !strings.Contains(f.Message, "not authorised") {
		t.Errorf("flash %+v err %v", f, err)
	}
}

func TestSplitIntegrationRefusesAnotherUsersId(t *testing.T) {
	c, _, two := integrationBackend(t)
	ctx := apiclient.WithRequest(context.Background(), apiclient.Request{Cookie: two})
	if _, err := c.ConnectIntegration(ctx, 1, "withings"); apiclient.StatusOf(err) != http.StatusNotFound {
		t.Errorf("connect: %v", err)
	}
	if _, err := c.Import(ctx, 1, "withings", ""); apiclient.StatusOf(err) != http.StatusNotFound {
		t.Errorf("import: %v", err)
	}
	if _, err := c.DisconnectIntegration(ctx, 1, "withings"); apiclient.StatusOf(err) != http.StatusNotFound {
		t.Errorf("disconnect: %v", err)
	}
	if _, err := c.SaveStrengthLevelLogin(ctx, 1, "a@b.c", "x"); apiclient.StatusOf(err) != http.StatusNotFound {
		t.Errorf("strengthlevel login: %v", err)
	}
	if _, err := c.StrengthLevelStatus(ctx, 1); apiclient.StatusOf(err) != http.StatusNotFound {
		t.Errorf("strengthlevel status: %v", err)
	}
}
