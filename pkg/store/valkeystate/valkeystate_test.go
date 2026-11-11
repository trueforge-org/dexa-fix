package valkeystate

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func open(t *testing.T) (*Client, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	v, err := Open(context.Background(), "redis://"+mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(v.Close)
	return v, mr
}

func TestFailuresCountInASlidingWindow(t *testing.T) {
	v, _ := open(t)
	ctx := context.Background()
	now := time.Now()
	for i, age := range []time.Duration{20 * time.Minute, 10 * time.Minute, 5 * time.Minute, time.Minute} {
		if err := v.RecordFailure(ctx, "login:alice", now.Add(-age)); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	n, err := v.CountFailures(ctx, "login:alice", now.Add(-15*time.Minute))
	if err != nil || n != 3 {
		t.Fatalf("failures in the last 15 minutes = %d, %v; want 3", n, err)
	}
	if n, _ := v.CountFailures(ctx, "login:alice", now.Add(-2*time.Minute)); n != 1 {
		t.Errorf("failures in the last 2 minutes = %d; want 1", n)
	}
	// An attempt exactly at the cutoff is outside the window, like failed_at > since in SQL.
	edge := time.Now().Add(-30 * time.Second)
	if err := v.RecordFailure(ctx, "edge", edge); err != nil {
		t.Fatal(err)
	}
	if n, _ := v.CountFailures(ctx, "edge", edge); n != 0 {
		t.Errorf("a failure at the cutoff counted: %d", n)
	}
	// Keys do not mix.
	if n, _ := v.CountFailures(ctx, "login:bob", now.Add(-time.Hour)); n != 0 {
		t.Errorf("another key's failures counted: %d", n)
	}
	if err := v.ClearFailures(ctx, "login:alice"); err != nil {
		t.Fatal(err)
	}
	if n, _ := v.CountFailures(ctx, "login:alice", now.Add(-time.Hour)); n != 0 {
		t.Errorf("failures after a clear: %d", n)
	}
}

// Failures that arrive in the same millisecond, from many goroutines, are all counted.
func TestEveryConcurrentFailureCounts(t *testing.T) {
	v, _ := open(t)
	ctx := context.Background()
	at := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := v.RecordFailure(ctx, "ip:203.0.113.0", at); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, err := v.CountFailures(ctx, "ip:203.0.113.0", at.Add(-time.Minute)); err != nil || n != 40 {
		t.Fatalf("counted %d of 40: %v", n, err)
	}
}

func TestACounterLeavesNothingBehindWhenItsFailuresAreOld(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	if err := v.RecordFailure(ctx, "k", time.Now()); err != nil {
		t.Fatal(err)
	}
	if mr.TTL(failPrefix+"k") <= 0 {
		t.Error("a counter without a life would stay forever")
	}
	mr.FastForward(failTTL + time.Second)
	if mr.Exists(failPrefix + "k") {
		t.Error("the counter outlived its life")
	}
}

func TestNonceIsUsedOnce(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	exp := time.Now().Add(10 * time.Minute)
	if seen, err := v.NonceSeen(ctx, "n1"); err != nil || seen {
		t.Fatalf("a new nonce was seen: %v %v", seen, err)
	}
	if first, err := v.UseNonce(ctx, "n1", exp); err != nil || !first {
		t.Fatalf("first use: %v %v", first, err)
	}
	if first, err := v.UseNonce(ctx, "n1", exp); err != nil || first {
		t.Fatalf("second use must lose: %v %v", first, err)
	}
	if seen, _ := v.NonceSeen(ctx, "n1"); !seen {
		t.Error("a used nonce is not seen")
	}
	if seen, _ := v.NonceSeen(ctx, "n2"); seen {
		t.Error("another nonce is seen")
	}
	// It is held until the value would have expired anyway, and a little longer for clock skew.
	if ttl := mr.TTL(noncePrefix + "n1"); ttl < 10*time.Minute || ttl > 12*time.Minute {
		t.Errorf("nonce life %v, want about 11 minutes", ttl)
	}
	mr.FastForward(12 * time.Minute)
	if seen, _ := v.NonceSeen(ctx, "n1"); seen {
		t.Error("an expired nonce is kept")
	}
	// An already expired value still holds its claim for the slack.
	if first, _ := v.UseNonce(ctx, "old", time.Now().Add(-time.Hour)); !first {
		t.Error("first use of an expired value")
	}
	if first, _ := v.UseNonce(ctx, "old", time.Now().Add(-time.Hour)); first {
		t.Error("an expired value was used twice")
	}
}

// Many processes racing for one value: exactly one wins.
func TestNonceRaceHasOneWinner(t *testing.T) {
	v, _ := open(t)
	ctx := context.Background()
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			first, err := v.UseNonce(ctx, "race", time.Now().Add(time.Minute))
			if err != nil {
				t.Error(err)
			}
			if first {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners, want exactly one", wins.Load())
	}
}

// When the server cannot be reached every operation returns an error: the backend turns that into
// "locked out" and "already spent" (it fails closed), never into "fine".
func TestErrorsAreReturned(t *testing.T) {
	v, mr := open(t)
	ctx := context.Background()
	mr.Close()
	if err := v.Ping(ctx); err == nil {
		t.Error("ping")
	}
	if err := v.RecordFailure(ctx, "k", time.Now()); err == nil {
		t.Error("record")
	}
	if _, err := v.CountFailures(ctx, "k", time.Now().Add(-time.Minute)); err == nil {
		t.Error("count")
	}
	if err := v.ClearFailures(ctx, "k"); err == nil {
		t.Error("clear")
	}
	if first, err := v.UseNonce(ctx, "n", time.Now().Add(time.Minute)); err == nil || first {
		t.Errorf("use: %v %v", first, err)
	}
	if seen, err := v.NonceSeen(ctx, "n"); err == nil || seen {
		t.Errorf("seen: %v %v", seen, err)
	}
}

func TestOpenChecksThePasswordAndNeverEchoesIt(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.RequireAuth("s3cret-pass")
	ctx := context.Background()
	v, err := Open(ctx, "redis://:s3cret-pass@"+mr.Addr()+"/0")
	if err != nil {
		t.Fatalf("right password: %v", err)
	}
	v.Close()

	for name, url := range map[string]string{
		"wrong password": "redis://:wrong-pass-123@" + mr.Addr(),
		"no password":    "redis://" + mr.Addr(),
		"nobody listens": "redis://:hunter2hunter2@127.0.0.1:1",
		"not a URL":      "redis://:hunter2hunter2@%zz",
		"another scheme": "http://:hunter2hunter2@" + mr.Addr(),
		"nothing at all": "",
	} {
		_, err := Open(ctx, url)
		if err == nil {
			t.Errorf("%s: Open succeeded", name)
			continue
		}
		for _, secret := range []string{"hunter2hunter2", "wrong-pass-123", "s3cret-pass"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error quotes a password: %v", name, err)
			}
		}
	}
}

func TestRedactRemovesThePasswordAsWrittenAndEscaped(t *testing.T) {
	pw := "p@ss w/rd"
	err := errors.New("dial " + pw + " and " + url.QueryEscape(pw) + " and " + url.PathEscape(pw))
	got := redact(err, pw).Error()
	if strings.Contains(got, pw) || strings.Contains(got, url.QueryEscape(pw)) || strings.Contains(got, url.PathEscape(pw)) {
		t.Fatalf("the password is still in %q", got)
	}
	if got := redact(errors.New("plain"), "").Error(); got != "plain" {
		t.Fatalf("without a password the text changed: %q", got)
	}
}
