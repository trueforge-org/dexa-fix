package valkeystate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestOpenGivesUpWhenTheServerDoesNotAnswerThePing(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Open(ctx, "redis://:hunter2hunter2@"+mr.Addr())
	if err == nil || !strings.HasPrefix(err.Error(), "connect to Valkey") || strings.Contains(err.Error(), "hunter2hunter2") {
		t.Fatalf("got %v", err)
	}
}

func TestASessionThatCannotBeWrittenAsJSONIsRefused(t *testing.T) {
	v, mr := open(t)
	// A year beyond 9999 has no JSON form.
	if err := v.CreateSession(context.Background(), "far", 1, "local", time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("the session was stored")
	}
	if keys := mr.Keys(); len(keys) != 0 {
		t.Fatalf("the failed session left %v behind", keys)
	}
}

func TestEndingASessionFailsWhenTheServerIsDown(t *testing.T) {
	v, mr := open(t)
	mr.Close()
	if err := v.DeleteSession(context.Background(), "x"); err == nil {
		t.Fatal("a session was reported as ended while the server was down")
	}
}

// The server drops a key when its time is up, but a clock that is a little behind must not let the
// session live on: the expiry in the value is checked as well.
func TestASessionWhoseKeyOutlivesItsExpiryIsNotValid(t *testing.T) {
	v, mr := open(t)
	mr.Set(sessPrefix+"late", `{"u":1,"m":"local","c":"2000-01-01T00:00:00Z","e":"2000-01-02T00:00:00Z"}`)
	if _, err := v.GetSession(context.Background(), "late"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}
