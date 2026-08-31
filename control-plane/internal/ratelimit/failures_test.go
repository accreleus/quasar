package ratelimit

import (
	"testing"
	"time"
)

func TestFailureLimiterBlocksExpiresAndForgets(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewFailureLimiter(2, time.Minute, 8)
	l.now = func() time.Time { return now }

	if !l.Allow("a") {
		t.Fatal("new key blocked")
	}
	l.Failure("a")
	if !l.Allow("a") {
		t.Fatal("blocked before failure limit")
	}
	l.Failure("a")
	if l.Allow("a") {
		t.Fatal("key allowed at failure limit")
	}
	l.Forget("a")
	if !l.Allow("a") {
		t.Fatal("successful authentication did not clear failures")
	}
	l.Failure("a")
	l.Failure("a")
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("expired failures still block")
	}
}

func TestFailureLimiterStateIsBounded(t *testing.T) {
	now := time.Unix(100, 0)
	l := NewFailureLimiter(2, time.Hour, 2)
	l.now = func() time.Time { return now }
	l.Failure("oldest")
	now = now.Add(time.Second)
	l.Failure("newer")
	now = now.Add(time.Second)
	l.Failure("third")
	if len(l.entries) != 2 {
		t.Fatalf("entries=%d, want 2", len(l.entries))
	}
	if _, ok := l.entries["oldest"]; ok {
		t.Fatal("oldest entry was not evicted")
	}
}

func TestFailureLimiterBoundsInFlightHandshakes(t *testing.T) {
	l := NewFailureLimiter(10, time.Minute, 8)
	if !l.Reserve("a", 2, 3) || !l.Reserve("a", 2, 3) {
		t.Fatal("initial per-key reservations rejected")
	}
	if l.Reserve("a", 2, 3) {
		t.Fatal("per-key in-flight limit bypassed")
	}
	if !l.Reserve("b", 2, 3) {
		t.Fatal("global third reservation rejected early")
	}
	if l.Reserve("c", 2, 3) {
		t.Fatal("global in-flight limit bypassed")
	}
	l.Release("a")
	if !l.Reserve("c", 2, 3) {
		t.Fatal("released capacity was not reusable")
	}
	l.Forget("a")
	l.Release("a")
	l.Release("b")
	l.Release("c")
	if l.inFlight != 0 {
		t.Fatalf("inFlight=%d want 0", l.inFlight)
	}
}

func TestForgetPreservesConcurrentReservations(t *testing.T) {
	l := NewFailureLimiter(10, time.Minute, 8)
	if !l.Reserve("shared", 2, 4) || !l.Reserve("shared", 2, 4) {
		t.Fatal("reserve")
	}
	l.Failure("shared")
	l.Forget("shared")
	if l.Reserve("shared", 2, 4) {
		t.Fatal("successful sibling erased in-flight accounting")
	}
	l.Release("shared")
	if !l.Reserve("shared", 2, 4) {
		t.Fatal("released sibling slot not reusable")
	}
	l.Release("shared")
	l.Release("shared")
	if l.inFlight != 0 {
		t.Fatalf("inFlight=%d want 0", l.inFlight)
	}
}
