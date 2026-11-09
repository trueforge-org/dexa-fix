package sqlstore

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trueforge-org/dexa-fix/pkg/models"
	"github.com/trueforge-org/dexa-fix/pkg/store"
)

func TestPasskeyHandle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}

	h1, err := s.EnsurePasskeyHandle(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	h1Again, err := s.EnsurePasskeyHandle(ctx, alice.ID)
	if err != nil || h1Again != h1 {
		t.Fatalf("second call returned %q (%v), want the same handle %q", h1Again, err, h1)
	}
	if len(h1) != 86 { // 64 random bytes, unpadded base64url
		t.Fatalf("handle is %d characters, want 86", len(h1))
	}
	raw, err := base64.RawURLEncoding.DecodeString(h1)
	if err != nil || len(raw) != 64 {
		t.Fatalf("handle decodes to %d bytes (%v), want 64", len(raw), err)
	}

	h2, err := s.EnsurePasskeyHandle(ctx, bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if h2 == h1 {
		t.Fatal("two users got the same handle")
	}

	u, err := s.UserByPasskeyHandle(ctx, h1)
	if err != nil || u.ID != alice.ID {
		t.Fatalf("UserByPasskeyHandle = %+v, %v; want alice", u, err)
	}
	if _, err := s.UserByPasskeyHandle(ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("empty handle: %v, want ErrNotFound", err)
	}
	if _, err := s.UserByPasskeyHandle(ctx, "not-a-handle"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown handle: %v, want ErrNotFound", err)
	}
	if _, err := s.EnsurePasskeyHandle(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("handle for unknown user: %v, want ErrNotFound", err)
	}
}

func TestPasskeyCRUD(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, _ := s.CreateUser(ctx, "alice")
	bob, _ := s.CreateUser(ctx, "bob")

	first := models.Passkey{CredentialID: "cred-1", UserID: alice.ID, Name: "Laptop", Credential: `{"id":"cred-1"}`}
	if err := s.AddPasskey(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Listed in the order they were added (created_at), not by name or id.
	time.Sleep(2 * time.Millisecond)
	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "aaa-2", UserID: alice.ID, Name: "Phone", Credential: `{"id":"aaa-2"}`, SignCount: 3}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListPasskeys(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].CredentialID != "cred-1" || list[1].CredentialID != "aaa-2" {
		t.Fatalf("list order: %+v", list)
	}
	if list[0].Name != "Laptop" || list[0].Credential != `{"id":"cred-1"}` || list[0].SignCount != 0 || list[0].LastUsedAt != nil {
		t.Fatalf("first passkey: %+v", list[0])
	}
	if list[1].SignCount != 3 || list[1].Name != "Phone" {
		t.Fatalf("second passkey: %+v", list[1])
	}
	if other, err := s.ListPasskeys(ctx, bob.ID); err != nil || len(other) != 0 {
		t.Fatalf("bob's passkeys: %+v, %v", other, err)
	}

	// The same authenticator credential cannot be registered twice, not even by another user.
	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-1", UserID: bob.ID, Name: "Copy", Credential: "{}"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate credential for another user: %v, want ErrExists", err)
	}
	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-new", UserID: 999, Name: "Ghost", Credential: "{}"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: %v, want ErrNotFound", err)
	}

	// A sign-in records the counter, the refreshed credential and the time.
	if err := s.UpdatePasskeyUse(ctx, "cred-1", `{"id":"cred-1","n":7}`, 7); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListPasskeys(ctx, alice.ID)
	if list[0].SignCount != 7 || list[0].Credential != `{"id":"cred-1","n":7}` || list[0].LastUsedAt == nil {
		t.Fatalf("after sign-in: %+v", list[0])
	}
	if time.Since(*list[0].LastUsedAt) > time.Minute {
		t.Fatalf("last used %v is not recent", *list[0].LastUsedAt)
	}
	if err := s.UpdatePasskeyUse(ctx, "missing", "{}", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("use of unknown passkey: %v, want ErrNotFound", err)
	}

	// Rename and delete only work for the owner.
	if err := s.RenamePasskey(ctx, bob.ID, "cred-1", "Hacked"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rename by another user: %v, want ErrNotFound", err)
	}
	if err := s.DeletePasskey(ctx, bob.ID, "cred-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete by another user: %v, want ErrNotFound", err)
	}
	if list, _ := s.ListPasskeys(ctx, alice.ID); list[0].Name != "Laptop" {
		t.Fatalf("name changed by another user: %+v", list[0])
	}
	if err := s.RenamePasskey(ctx, alice.ID, "cred-1", "Work"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenamePasskey(ctx, alice.ID, "missing", "x"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rename unknown: %v, want ErrNotFound", err)
	}
	if list, _ := s.ListPasskeys(ctx, alice.ID); list[0].Name != "Work" {
		t.Fatalf("rename did not stick: %+v", list[0])
	}

	if err := s.DeletePasskey(ctx, alice.ID, "cred-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePasskey(ctx, alice.ID, "cred-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete twice: %v, want ErrNotFound", err)
	}
	if list, _ := s.ListPasskeys(ctx, alice.ID); len(list) != 1 || list[0].CredentialID != "aaa-2" {
		t.Fatalf("after delete: %+v", list)
	}
}

func TestPasskeyShowsAsProvider(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, _ := s.CreateUser(ctx, "alice")
	bob, _ := s.CreateUser(ctx, "bob")
	// Real providers, added out of order: they come back sorted, with the passkey after them.
	for _, p := range []string{"zeta", "alpha"} {
		if err := s.PutIdentity(ctx, models.Identity{UserID: alice.ID, Provider: p, Subject: p + "-sub"}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.UserProviders(ctx, alice.ID)
	if err != nil || !equalStrings(got, []string{"alpha", "zeta"}) {
		t.Fatalf("before a passkey: %v, %v", got, err)
	}

	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-p", UserID: alice.ID, Name: "Laptop", Credential: "{}"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.UserProviders(ctx, alice.ID)
	if err != nil || !equalStrings(got, []string{"alpha", "zeta", models.ProviderPasskey}) {
		t.Fatalf("with a passkey: %v, %v", got, err)
	}
	ids, err := s.IdentityProviders(ctx)
	if err != nil || !equalStrings(ids[alice.ID], []string{"alpha", "zeta", models.ProviderPasskey}) {
		t.Fatalf("identity map for alice: %v, %v", ids[alice.ID], err)
	}
	if _, ok := ids[bob.ID]; ok {
		t.Fatalf("bob has no sign-in methods, map has %v", ids[bob.ID])
	}

	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-b", UserID: bob.ID, Name: "Phone", Credential: "{}"}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.IdentityProviders(ctx); !equalStrings(ids[bob.ID], []string{models.ProviderPasskey}) {
		t.Fatalf("passkey-only user: %v", ids[bob.ID])
	}

	if err := s.DeletePasskey(ctx, alice.ID, "cred-p"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.UserProviders(ctx, alice.ID)
	if !equalStrings(got, []string{"alpha", "zeta"}) {
		t.Fatalf("after the last passkey is removed: %v", got)
	}
	ids, _ = s.IdentityProviders(ctx)
	if !equalStrings(ids[alice.ID], []string{"alpha", "zeta"}) {
		t.Fatalf("identity map after removal: %v", ids[alice.ID])
	}
}

func TestPasskeysCascadeWithUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, _ := s.CreateUser(ctx, "alice")
	bob, _ := s.CreateUser(ctx, "bob")
	// The first account is the administrator and cannot be removed while it is the last one, so the
	// account that goes is bob.
	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-a", UserID: alice.ID, Name: "A", Credential: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPasskey(ctx, models.Passkey{CredentialID: "cred-b", UserID: bob.ID, Name: "B", Credential: "{}"}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatal(err)
	}
	n, err := s.db.NewSelect().Model((*passkeyRow)(nil)).Where("credential_id = ?", "cred-b").Count(ctx)
	if err != nil || n != 0 {
		t.Fatalf("bob's passkey after his removal: %d rows, %v", n, err)
	}
	if list, err := s.ListPasskeys(ctx, alice.ID); err != nil || len(list) != 1 {
		t.Fatalf("alice's passkey should stay: %+v, %v", list, err)
	}
}

func TestCopyKeepsPasskeys(t *testing.T) {
	ctx := context.Background()
	src, err := Open(ctx, DriverSQLite, "file:"+filepath.Join(t.TempDir(), "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := newStore(t)

	alice, err := src.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := src.EnsurePasskeyHandle(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.AddPasskey(ctx, models.Passkey{CredentialID: "cred-copy", UserID: alice.ID, Name: "Laptop", Credential: `{"id":"cred-copy"}`}); err != nil {
		t.Fatal(err)
	}
	if err := src.UpdatePasskeyUse(ctx, "cred-copy", `{"id":"cred-copy","n":4}`, 4); err != nil {
		t.Fatal(err)
	}

	if _, err := Copy(ctx, src, dst); err != nil {
		t.Fatal(err)
	}

	want, err := src.ListPasskeys(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dst.ListPasskeys(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("passkeys: copied %d, source has %d", len(got), len(want))
	}
	if got[0].CredentialID != want[0].CredentialID || got[0].Name != want[0].Name || got[0].Credential != want[0].Credential ||
		got[0].SignCount != 4 || got[0].LastUsedAt == nil || !got[0].LastUsedAt.Equal(*want[0].LastUsedAt) {
		t.Fatalf("copied passkey differs:\n%+v\n%+v", got[0], want[0])
	}

	h, err := dst.EnsurePasskeyHandle(ctx, alice.ID)
	if err != nil || h != handle {
		t.Fatalf("handle on the copy = %q (%v), want %q", h, err, handle)
	}
	u, err := dst.UserByPasskeyHandle(ctx, handle)
	if err != nil || u.ID != alice.ID {
		t.Fatalf("copy cannot find the user by handle: %+v, %v", u, err)
	}
}

// equalStrings reports whether two lists hold the same strings in the same order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
