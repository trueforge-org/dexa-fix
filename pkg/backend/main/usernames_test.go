package mainbackend

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/trueforge-org/dexafix/pkg/models"
)

func TestFirstAndLastNameAreKeptShownAndExported(t *testing.T) {
	ts, c, st := newAuthServer(t)
	signup(t, ts, c, "alice")
	other := noRedirectClient()
	signup(t, ts, other, "bob")

	save := func(username, first, last string) (int, string) {
		r, err := c.PostForm(ts.URL+"/u/1/profile", url.Values{"username": {username}, "first_name": {first}, "last_name": {last}, "email": {"alice@example.com"}})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return r.StatusCode, bodyOf(t, r)
	}
	if code, b := save("alice", "Alicia", "Wonderland"); code != 303 {
		t.Fatalf("save: %d %s", code, b)
	}
	u, err := st.GetUser(context.Background(), 1)
	if err != nil || u.FirstName != "Alicia" || u.LastName != "Wonderland" || u.Username != "alice" {
		t.Fatalf("stored: %+v %v", u, err)
	}

	_, page := body(t, c, ts.URL+"/u/1/settings")
	for _, want := range []string{`name="username" value="alice"`, `name="first_name" value="Alicia"`, `name="last_name" value="Wonderland"`, `class="user-name">Alicia<`} {
		if !strings.Contains(page, want) {
			t.Errorf("settings page lacks %s", want)
		}
	}
	if _, admin := body(t, c, ts.URL+"/admin"); !strings.Contains(admin, "<td>Alicia Wonderland</td>") {
		t.Errorf("the admin table does not show the full name")
	}
	_, export := body(t, c, ts.URL+"/u/1/export/profile")
	var tagged struct {
		Account struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
			LastName  string `json:"last_name"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(export), &tagged); err != nil || tagged.Account.Username != "alice" || tagged.Account.FirstName != "Alicia" || tagged.Account.LastName != "Wonderland" {
		t.Errorf("export: %+v %v", tagged, err)
	}

	for name, c := range map[string]struct{ username, first, last, want string }{
		"a username that is taken, whatever the case": {"BOB", "", "", "That username is taken."},
		"a username with a space":                     {"two words", "", "", "2-32 letters"},
		"a first name that is too long":               {"alice", strings.Repeat("x", 65), "", "First and last name"},
	} {
		if code, b := save(c.username, c.first, c.last); code != 422 || !strings.Contains(b, c.want) {
			t.Errorf("%s: %d, want 422 with %q", name, code, c.want)
		}
	}
	if again, _ := st.GetUser(context.Background(), 1); again.Username != "alice" || again.FirstName != "Alicia" {
		t.Fatalf("a refused save changed the user: %+v", again)
	}
}

func TestAProviderAccountGetsAUsernameAndNamesFromTheClaims(t *testing.T) {
	g := newRig(t)
	idp := newOAuthFake(t)
	p := idp.provider()
	p.SubjectClaim = "id"
	p.NameClaim = "login"
	g.addProvider(p)

	signIn := func(userinfo string) {
		t.Helper()
		idp.set(func(f *oauthFake) { f.userinfoBody = userinfo })
		state, cookie, _ := g.beginProvider("idp", map[string]any{})
		if out := g.callback("idp", state, cookie, nil); out.Redirect == "" || out.Error != "" {
			t.Fatalf("sign-in with %s: %+v", userinfo, out)
		}
	}
	signIn(`{"id":1,"login":"Pat Jones!","given_name":"Pat","family_name":"Jones"}`)
	signIn(`{"id":2,"login":"Pat Jones!","given_name":"` + strings.Repeat("x", 65) + `"}`)
	signIn(`{"id":3,"login":"pat-jones-","family_name":" "}`)

	byID := map[string]models.User{}
	users, _ := g.st.ListUsers(context.Background())
	for _, u := range users {
		byID[u.Username] = u
	}
	if u := byID["Pat-Jones-"]; u.FirstName != "Pat" || u.LastName != "Jones" {
		t.Errorf("the first account: %+v", u)
	}
	// The same username again: a number is added. A name the store refuses is only logged.
	if u, ok := byID["Pat-Jones--2"]; !ok || u.FirstName != "" {
		t.Errorf("the second account: %+v (all: %v)", u, users)
	}
	if _, ok := byID["pat-jones-"]; ok {
		t.Errorf("a case-only difference is not a different username: %v", users)
	}
	if _, ok := byID["pat-jones--3"]; !ok {
		t.Errorf("the third account: %v", users)
	}
}
