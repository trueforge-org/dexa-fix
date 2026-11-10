package sqlstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/trueforge-org/dexafix/pkg/models"
	"github.com/trueforge-org/dexafix/pkg/store"
)

func TestGetSession(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	alice, err := s.CreateUser(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.CreateSession(ctx, "hash-live", alice.ID, models.ProviderLocal, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(ctx, "hash-live")
	if err != nil {
		t.Fatalf("live session: %v", err)
	}
	if sess.UserID != alice.ID || sess.Method != models.ProviderLocal {
		t.Fatalf("session: %+v", sess)
	}

	if _, err := s.GetSession(ctx, "hash-unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown hash: %v", err)
	}

	if err := s.CreateSession(ctx, "hash-expired", alice.ID, models.ProviderLocal, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(ctx, "hash-expired"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expired session: %v", err)
	}
}
