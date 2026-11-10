package must

import (
	"errors"
	"testing"
)

// panicValue runs f and returns what it panicked with, or nil when it returned normally.
func panicValue(f func()) (got any) {
	defer func() { got = recover() }()
	f()
	return nil
}

func TestNothingHappensWhenThereIsNoError(t *testing.T) {
	if got := panicValue(func() { Do(nil) }); got != nil {
		t.Fatalf("Do(nil) panicked with %v", got)
	}
}

func TestAnErrorBecomesAPanicCarryingThatError(t *testing.T) {
	boom := errors.New("boom")
	if got := panicValue(func() { Do(boom) }); got != boom {
		t.Fatalf("Do(boom) panicked with %v, want the error itself", got)
	}
}

func TestValueIsReturnedWhenThereIsNoError(t *testing.T) {
	if got := Value("key", nil); got != "key" {
		t.Fatalf("Value = %q, want %q", got, "key")
	}
}

func TestValuePanicsWithTheErrorInsteadOfReturningAnything(t *testing.T) {
	boom := errors.New("boom")
	if got := panicValue(func() { _ = Value(42, boom) }); got != boom {
		t.Fatalf("Value(42, boom) panicked with %v, want the error itself", got)
	}
}

func TestTwoValuesAreReturnedWhenThereIsNoError(t *testing.T) {
	r, s := Value2("r", "s", nil)
	if r != "r" || s != "s" {
		t.Fatalf("Value2 = %q, %q, want r, s", r, s)
	}
}

func TestTwoValuesPanicWithTheErrorInsteadOfReturningAnything(t *testing.T) {
	boom := errors.New("boom")
	if got := panicValue(func() { _, _ = Value2(1, 2, boom) }); got != boom {
		t.Fatalf("Value2 panicked with %v, want the error itself", got)
	}
}
