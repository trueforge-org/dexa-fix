package mainbackend

import (
	"fmt"
	"testing"

	"github.com/trueforge-org/dexa-fix/pkg/store/sqlstore"
)

// With random user ids nothing may assume that the first user is 1: every page and API route is
// reached with the id the user really has, and another user's id still answers 404.
func TestRandomUserIDsFlow(t *testing.T) {
	sqlstore.SetSequentialUserIDsForTests(false)
	t.Cleanup(func() { sqlstore.SetSequentialUserIDsForTests(true) })

	ts, alice, st := newAuthServer(t)
	signup(t, ts, alice, "alice")
	bob := noRedirectClient()
	signup(t, ts, bob, "bob")

	users, err := st.ListUsers(t.Context())
	if err != nil || len(users) != 2 {
		t.Fatalf("users: %v %v", users, err)
	}
	var aliceID, bobID int64
	for _, u := range users {
		if u.Name == "alice" {
			aliceID = u.ID
		} else {
			bobID = u.ID
		}
	}
	if aliceID == 1 || bobID == 2 || aliceID == bobID || aliceID >= 1<<53 || bobID >= 1<<53 {
		t.Fatalf("ids do not look random: %d, %d", aliceID, bobID)
	}
	if code, _ := body(t, alice, fmt.Sprintf("%s/u/%d/scale", ts.URL, aliceID)); code != 200 {
		t.Errorf("own page: %d", code)
	}
	if code, _ := body(t, alice, fmt.Sprintf("%s/u/%d/scale", ts.URL, bobID)); code != 404 {
		t.Errorf("other user's page: %d", code)
	}
	if code, _ := apiCall(t, bob, "GET", fmt.Sprintf("%s/api/v1/users/%d/estimates", ts.URL, aliceID), "", nil); code != 404 {
		t.Errorf("other user's API: %d", code)
	}
	if code, _ := body(t, alice, ts.URL+"/u/1/scale"); code != 404 {
		t.Errorf("a guessed id: %d", code)
	}
}
