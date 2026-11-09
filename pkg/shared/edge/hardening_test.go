package edge

import (
	"fmt"
	"testing"
)

func TestLimiterEvictionKeepsNearlyBlockedKeys(t *testing.T) {
	l := NewFailLimiter()
	for range 10 - 1 {
		l.Fail("login:victim")
	}
	for i := range maxLimiterKeys + 100 {
		l.Fail(fmt.Sprintf("login:flood%d", i))
	}
	if len(l.fails) > maxLimiterKeys {
		t.Fatalf("%d keys, want at most %d", len(l.fails), maxLimiterKeys)
	}
	l.Fail("login:victim")
	if !l.Blocked("login:victim", 10) {
		t.Fatal("a flood of other keys reset the victim's counter")
	}
}
