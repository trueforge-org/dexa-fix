package sqlstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/store"
)

func TestCreateUserRefusesBadAndTakenUsernames(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.CreateUser(ctx, "Alice"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a", "has space", ".dot", "-dash", "umläut", strings.Repeat("x", 33)} {
		if _, err := s.CreateUser(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("CreateUser(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	for _, taken := range []string{"Alice", "alice", "ALICE"} {
		_, err := s.CreateUser(ctx, taken)
		if !errors.Is(err, store.ErrUsernameTaken) || !errors.Is(err, store.ErrExists) {
			t.Errorf("CreateUser(%q) = %v, want ErrUsernameTaken (an ErrExists)", taken, err)
		}
	}
}

func TestProfileKeepsNamesAndChecksTheUsername(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateUserProfile(ctx, alice.ID, " Alice.W ", " Alice ", "Wonderä", "alice@example.com", nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetUser(ctx, alice.ID)
	if err != nil || got.Username != "Alice.W" || got.FirstName != "Alice" || got.LastName != "Wonderä" {
		t.Fatalf("after save: %+v %v", got, err)
	}
	if got.DisplayName() != "Alice" || got.FullName() != "Alice Wonderä" {
		t.Fatalf("display %q, full %q", got.DisplayName(), got.FullName())
	}

	cases := []struct {
		name                string
		username, first, ln string
		want                error
	}{
		{"a username that is taken, whatever the case", "BOB", "", "", store.ErrUsernameTaken},
		{"a username with a space", "two words", "", "", store.ErrInvalid},
		{"a first name that is too long", "alice", strings.Repeat("x", 65), "", store.ErrInvalid},
		{"a last name with a control character", "alice", "", "a\x00b", store.ErrInvalid},
	}
	for _, c := range cases {
		if err := s.UpdateUserProfile(ctx, alice.ID, c.username, c.first, c.ln, "alice@example.com", nil); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Failed saves change nothing.
	if again, _ := s.GetUser(ctx, alice.ID); again.Username != "Alice.W" || again.FirstName != "Alice" {
		t.Fatalf("a refused save changed the user: %+v", again)
	}
}

func TestAnOldUsernameThatBreaksTheRulesStillSaves(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u, err := s.CreateUser(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	// What a user named before the rules (spaces and the like) looks like.
	if _, err := s.db.ExecContext(ctx, "UPDATE users SET username = 'Old Name' WHERE id = ?", u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateUserProfile(ctx, u.ID, "Old Name", "Olly", "", "old@example.com", nil); err != nil {
		t.Fatalf("saving the profile with the same username: %v", err)
	}
	if err := s.UpdateUserProfile(ctx, u.ID, "Older Name", "", "", "old@example.com", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a changed username must follow the rules: %v", err)
	}
}

// A database made by v0.0.1 has a "name" column and may hold names that differ only in case. The
// upgrade keeps every account, makes the later ones unique and enforces uniqueness from then on.
func TestMigrationToUsernamesKeepsOldAccounts(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.MigrateTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	older, newer := time.Now().Add(-time.Hour).UTC(), time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO users (id, name, created_at) VALUES (?, ?, ?), (?, ?, ?), (?, ?, ?)",
		11, "Bob", older, 12, "bob", newer, 13, "Carol Smith", newer); err != nil {
		t.Fatalf("insert at version 1: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 3 {
		t.Fatalf("users after upgrade: %d %v", len(users), err)
	}
	byID := map[int64]string{}
	for _, u := range users {
		byID[u.ID] = u.Username
		if u.FirstName != "" || u.LastName != "" {
			t.Errorf("user %d has names after the upgrade: %q %q", u.ID, u.FirstName, u.LastName)
		}
	}
	if byID[11] != "Bob" || byID[12] != "bob-12" || byID[13] != "Carol Smith" {
		t.Fatalf("usernames after upgrade: %v", byID)
	}
	if _, err := s.CreateUser(ctx, "BOB"); !errors.Is(err, store.ErrUsernameTaken) {
		t.Fatalf("a third Bob: %v", err)
	}
	// And back down: the column is called name again and the accounts are still there.
	if _, err := s.MigrateTo(ctx, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var n int
	if err := s.db.NewRaw("SELECT count(*) FROM users WHERE name <> ''").Scan(ctx, &n); err != nil || n != 3 {
		t.Fatalf("after down: %d %v", n, err)
	}
}
