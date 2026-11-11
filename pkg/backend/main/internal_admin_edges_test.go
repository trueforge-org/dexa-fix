package mainbackend

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/shared/apiclient"
	"github.com/trueforge-org/dexafix/pkg/shared/bodyapi"
	"github.com/trueforge-org/dexafix/pkg/store"
)

// internalRaw sends a request straight to the internal API as the signed-in administrator, with a
// body the frontend would never produce.
func internalRaw(t *testing.T, srvURL, session, method, path, body string) int {
	t.Helper()
	req, err := http.NewRequest(method, srvURL+apiclient.Prefix+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(apiclient.HeaderService, "current-secret-0123456789abcdef")
	req.Header.Set(apiclient.HeaderCookie, sessionCookie+"="+session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAdminEndpointsRefuseWhatTheFrontendWouldNeverSend(t *testing.T) {
	bs, _, root := newInternalServer(t)
	withBody := []string{
		"POST /admin/users", "POST /admin/signup", "POST /admin/analytics", "POST /admin/password-login", "POST /admin/two-factor",
		"POST /admin/providers", "POST /admin/withings", "POST /admin/server", "POST /admin/smtp", "POST /admin/tls",
		"POST /admin/users/1/role", "POST /admin/users/1/strengthlevel", "POST /admin/users/1/strengthlevel-bulk",
		"POST /admin/users/1/password", "POST /admin/users/1/reset-link",
	}
	for _, route := range withBody {
		method, path, _ := strings.Cut(route, " ")
		t.Run("garbage for "+route, func(t *testing.T) {
			if got := internalRaw(t, bs.URL, root.Value, method, path, "{not json"); got != http.StatusBadRequest {
				t.Fatalf("status %d, want 400", got)
			}
		})
	}
	withID := []string{
		"POST /admin/users/abc/role", "POST /admin/users/abc/strengthlevel", "POST /admin/users/abc/strengthlevel-bulk",
		"POST /admin/users/abc/password", "POST /admin/users/abc/reset-link", "DELETE /admin/users/abc", "DELETE /admin/users/abc/2fa",
	}
	for _, route := range withID {
		method, path, _ := strings.Cut(route, " ")
		t.Run("a user id that is not a number for "+route, func(t *testing.T) {
			if got := internalRaw(t, bs.URL, root.Value, method, path, "{}"); got != http.StatusNotFound {
				t.Fatalf("status %d, want 404", got)
			}
		})
	}
}

func TestAdminPageShowsWhatIsOddAboutTheData(t *testing.T) {
	e := newAdminEnv(t)
	ctx := context.Background()
	// A user who can sign in by no means, and audit events by a user who was deleted while the page was made.
	if _, err := e.st.CreateUser(ctx, "ghost"); err != nil {
		t.Fatal(err)
	}
	e.flaky.failOn("ListUsers:without-bob")
	p := models.AuthProvider{Slug: "gh", Label: "GitHub", Kind: models.ProviderKindOAuth2, ClientID: "c", ClientSecret: "s",
		AuthURL: "https://idp.example/auth", TokenURL: "https://idp.example/token", UserinfoURL: "https://idp.example/me", SubjectClaim: "sub"}
	if err := e.st.PutAuthProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	code, page := e.do(t, "GET", "/admin?edit=gh", nil)
	if code != 200 {
		t.Fatalf("page: %d", code)
	}
	for _, want := range []string{"ghost", ">None<", "deleted user", `value="gh"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the Admin page lacks %q", want)
		}
	}
}

func TestAdminPageTellsAFailingBodyServiceFromADownOne(t *testing.T) {
	e := newAdminEnv(t)
	e.useBody(bodyAnswering(t, http.StatusForbidden, bodyapi.CodeForbidden))
	if code, _ := e.do(t, "GET", "/admin", nil); code != http.StatusServiceUnavailable {
		t.Fatalf("page with a body service that refuses the backend: %d, want 503", code)
	}
	e.useBody(bodyDown(t))
	code, page := e.do(t, "GET", "/admin", nil)
	if code != 200 || !strings.Contains(page, "Unavailable") {
		t.Fatalf("page with the body service down: %d", code)
	}
}

func (f *flakyStore) DeleteUser(ctx context.Context, id int64) error {
	if f.fails("DeleteUser:last-admin") {
		return store.ErrLastAdmin
	}
	return f.Store.DeleteUser(ctx, id)
}

func TestAdminCannotDeleteTheLastAdministrator(t *testing.T) {
	runAdminChanges(t, []adminChange{
		{name: "deleting the only administrator", setup: failing("DeleteUser:last-admin"), path: "/admin/users/2/delete", form: url.Values{}, wantFlash: "at least one administrator"},
	})
}
