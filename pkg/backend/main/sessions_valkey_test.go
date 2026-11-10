package mainbackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

// valkeyKeys lists the keys in the fake Valkey server that start with prefix.
func valkeyKeys(mr *miniredis.Miniredis, prefix string) []string {
	var out []string
	for _, k := range mr.Keys() {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

// signedIn reports whether the client reaches its own settings page (a signed-out one is redirected).
func signedIn(t *testing.T, ts *httptest.Server, c *http.Client, id string) bool {
	t.Helper()
	code, _ := body(t, c, ts.URL+"/u/"+id+"/settings")
	return code == http.StatusOK
}

// secondSession signs the same person in again, as another browser would.
func secondSession(t *testing.T, ts *httptest.Server, name, password string) *http.Client {
	t.Helper()
	c := noRedirectClient()
	resp := post(t, c, ts.URL+"/login", url.Values{"email": {name + "@example.com"}, "password": {password}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second sign-in of %s: %d", name, resp.StatusCode)
	}
	useSession(t, c, resp)
	return c
}

// With Valkey configured the sessions are there and the sessions table stays empty: a signed-in
// visitor costs the database no session row and no session lookup. Signing out ends the session.
func TestValkeyHoldsTheSessions(t *testing.T) {
	ts, st, mr := newValkeyServer(t)
	ctx := context.Background()
	root := noRedirectClient()
	signup(t, ts, root, "root") // user 1, administrator
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2

	if !signedIn(t, ts, root, "1") || !signedIn(t, ts, bob, "2") {
		t.Fatal("a session in Valkey must sign the person in")
	}
	if got := valkeyKeys(mr, "dexafix:sess:"); len(got) != 2 {
		t.Errorf("sessions in Valkey: %v, want 2", got)
	}
	if stats, err := st.Stats(ctx); err != nil || stats.Sessions != 0 {
		t.Errorf("the database holds %d sessions (%v): they belong in Valkey", stats.Sessions, err)
	}
	// The cookie holds the token; Valkey holds only its hash.
	if ck := root.Transport.(cookieTransport).cookie.Value; len(valkeyKeys(mr, "dexafix:sess:"+ck)) != 0 {
		t.Error("the session token itself is a key in Valkey: only its hash may be")
	}

	if r := post(t, root, ts.URL+"/logout", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if signedIn(t, ts, root, "1") {
		t.Error("the session survived the sign-out")
	}
	if got := valkeyKeys(mr, "dexafix:sess:"); len(got) != 1 {
		t.Errorf("sessions in Valkey after one sign-out: %v, want 1", got)
	}
	if !signedIn(t, ts, bob, "2") {
		t.Error("signing out one person ended another's session")
	}
}

// What the person is comes from the database on every request, not from Valkey: an account that was
// deleted loses its session even when nothing told Valkey, and a role change shows at once.
func TestValkeySessionsReadTheUserFromTheDatabase(t *testing.T) {
	ts, st, _ := newValkeyServer(t)
	ctx := context.Background()
	root := noRedirectClient()
	signup(t, ts, root, "root")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")

	if code, _ := body(t, bob, ts.URL+"/admin"); code == http.StatusOK {
		t.Fatal("bob is not an administrator yet")
	}
	post(t, root, ts.URL+"/admin/users/2/role", url.Values{"role": {"admin"}})
	if code, _ := body(t, bob, ts.URL+"/admin"); code != http.StatusOK {
		t.Errorf("a role change must show at once: %d", code)
	}

	if err := st.DeleteUser(ctx, 2); err != nil { // behind the backend's back: Valkey is not told
		t.Fatal(err)
	}
	if signedIn(t, ts, bob, "2") {
		t.Error("the session of a deleted account still works")
	}
}

// Every way of ending sessions on purpose reaches Valkey: a changed password, a password set by an
// administrator, a reset link, and deleting the account. The other sessions of that person end, the
// sessions of other people do not.
func TestValkeySessionsEndOnPurpose(t *testing.T) {
	ts, _, mr := newValkeyServer(t)
	root := noRedirectClient()
	signup(t, ts, root, "root") // user 1
	bob := noRedirectClient()
	signup(t, ts, bob, "bob") // user 2
	carol := noRedirectClient()
	signup(t, ts, carol, "carol") // user 3

	t.Run("password change", func(t *testing.T) {
		other := secondSession(t, ts, "carol", testPassword)
		const next = "a brand new password"
		if r := post(t, carol, ts.URL+"/u/3/password", url.Values{"current": {testPassword}, "password": {next}, "confirm": {next}}); r.StatusCode != http.StatusOK {
			t.Fatalf("change: %d", r.StatusCode)
		}
		if signedIn(t, ts, other, "3") {
			t.Error("another session of the person survived the password change")
		}
		if !signedIn(t, ts, root, "1") || !signedIn(t, ts, bob, "2") {
			t.Error("a password change ended somebody else's session")
		}
		if n := mr.Exists("dexafix:usess:3"); !n {
			t.Error("the session started by the change is not indexed")
		}
	})

	t.Run("password set by an administrator", func(t *testing.T) {
		other := secondSession(t, ts, "bob", testPassword)
		post(t, root, ts.URL+"/admin/users/2/password", url.Values{"password": {"set by the admin"}})
		if signedIn(t, ts, bob, "2") || signedIn(t, ts, other, "2") {
			t.Error("a session survived the password an administrator set")
		}
		if mr.Exists("dexafix:usess:2") {
			t.Error("the index of the person's sessions was left behind")
		}
		if !signedIn(t, ts, root, "1") {
			t.Error("the administrator was signed out")
		}
	})

	t.Run("reset link", func(t *testing.T) {
		dave := noRedirectClient()
		signup(t, ts, dave, "dave") // user 4
		other := secondSession(t, ts, "dave", testPassword)
		token, _ := makeResetLink(t, ts, root, 4)
		if r, page := resetPOST(t, noRedirectClient(), ts.URL, resetForm(token, "reset by the link")); r.StatusCode >= 400 {
			t.Fatalf("reset: %d\n%s", r.StatusCode, page)
		}
		if signedIn(t, ts, dave, "4") || signedIn(t, ts, other, "4") {
			t.Error("a session survived a password reset")
		}
	})

	t.Run("deleting the account", func(t *testing.T) {
		erin := noRedirectClient()
		signup(t, ts, erin, "erin") // user 5
		secondSession(t, ts, "erin", testPassword)
		if len(valkeyKeys(mr, "dexafix:usess:5")) != 1 {
			t.Fatal("erin's sessions are not indexed")
		}
		post(t, root, ts.URL+"/admin/users/5/delete", nil)
		if signedIn(t, ts, erin, "5") {
			t.Error("a deleted account still has a session")
		}
		if mr.Exists("dexafix:usess:5") {
			t.Error("the sessions of a deleted account were left in Valkey")
		}
	})
}

// With Valkey down the sessions cannot be read, so nobody is signed in (the same fail-closed answer
// as for the counters), and they are there again when it is back: nothing was lost.
func TestValkeyDownSignsEverybodyOut(t *testing.T) {
	ts, _, mr := newValkeyServer(t)
	root := noRedirectClient()
	signup(t, ts, root, "root")
	if !signedIn(t, ts, root, "1") {
		t.Fatal("not signed in")
	}
	mr.Close()
	if signedIn(t, ts, root, "1") {
		t.Error("a session was believed while Valkey does not answer")
	}
	if err := mr.Restart(); err != nil {
		t.Fatal(err)
	}
	if !signedIn(t, ts, root, "1") {
		t.Error("the session did not come back with Valkey")
	}
}

// The database's own sessions are what they were: without Valkey nothing in this file applies.
func TestSessionsStayInTheDatabaseWithoutValkey(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "root")
	stats, err := st.Stats(context.Background())
	if err != nil || stats.Sessions != 1 {
		t.Fatalf("sessions in the database: %d (%v), want 1", stats.Sessions, err)
	}
}
