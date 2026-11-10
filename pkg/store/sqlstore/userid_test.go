package sqlstore

import (
	"context"
	"testing"
)

// New users get random ids below 2^53: not counted, never 0, exact in JSON and JavaScript.
func TestUserIDsAreRandom(t *testing.T) {
	randomUserIDs(t)
	st := newStore(t)
	ctx := context.Background()
	seen := map[int64]bool{}
	var prev int64
	consecutive := 0
	for i := 0; i < 40; i++ {
		u, err := st.CreateUser(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		if u.ID <= 0 || u.ID >= 1<<53 {
			t.Fatalf("id %d out of range [1, 2^53)", u.ID)
		}
		if seen[u.ID] {
			t.Fatalf("id %d given twice", u.ID)
		}
		seen[u.ID] = true
		if prev != 0 && u.ID == prev+1 {
			consecutive++
		}
		prev = u.ID
		got, err := st.GetUser(ctx, u.ID)
		if err != nil || got.ID != u.ID {
			t.Fatalf("a user is found by the id it got: %v %v", got, err)
		}
	}
	if consecutive > 2 {
		t.Fatalf("%d consecutive ids in 40: they look counted", consecutive)
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	admins := 0
	for _, u := range users {
		if u.Role == "admin" {
			admins++
		}
	}
	if admins != 1 {
		t.Fatalf("exactly the first user is the admin whatever the id, got %d admins", admins)
	}
}

// A collision is retried with a new id, and gives up cleanly when the source keeps colliding.
func TestUserIDCollisionRetries(t *testing.T) {
	randomUserIDs(t)
	st := newStore(t)
	ctx := context.Background()
	first, err := st.CreateUser(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	defer func(old func() (int64, error)) { userIDSource = old }(userIDSource)

	calls := 0
	userIDSource = func() (int64, error) {
		calls++
		if calls < 3 {
			return first.ID, nil // taken
		}
		return first.ID + 7, nil
	}
	u, err := st.CreateUser(ctx, "second")
	if err != nil || u.ID != first.ID+7 || calls != 3 {
		t.Fatalf("collision not retried: id=%d calls=%d err=%v", u.ID, calls, err)
	}

	userIDSource = func() (int64, error) { return first.ID, nil }
	if _, err := st.CreateUser(ctx, "third"); err == nil {
		t.Fatal("an id source that always collides must fail, not loop or overwrite")
	}
}
