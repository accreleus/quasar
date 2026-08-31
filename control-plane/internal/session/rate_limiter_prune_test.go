package session

import (
	"strconv"
	"testing"
	"time"
)

// TestRateLimiterPrunesRefilledBuckets: the limiter's doc comment claims idle
// buckets that have refilled to full are pruned lazily. Until #403 it did not
// prune at all — every key ever seen (a session id, plus its "/trace" and
// "/trace/clock" variants) was retained for the life of the process, up to three
// permanent map entries per session ever created.
func TestRateLimiterPrunesRefilledBuckets(t *testing.T) {
	// Short refill so "fully refilled" is reachable in a test without a fake
	// clock: a bucket is prunable once burst x refill has elapsed since it was
	// last touched. 100 ms (not 1 ms) so the fill loop below cannot itself
	// outlast the window — under -race 500 inserts take milliseconds, and a
	// 1 ms refill made the prune fire mid-setup.
	const burst = 2
	const refill = 100 * time.Millisecond
	rl := newRateLimiter(burst, refill)

	for i := 0; i < 500; i++ {
		rl.allow("session-" + strconv.Itoa(i))
	}
	rl.mu.Lock()
	before := len(rl.buckets)
	rl.mu.Unlock()
	if before != 500 {
		t.Fatalf("setup: %d buckets, want 500", before)
	}

	// Let every bucket refill to full, then touch one NEW key — the lazy prune
	// runs on insert.
	time.Sleep(burst*refill + 50*time.Millisecond)
	rl.allow("fresh")

	rl.mu.Lock()
	after := len(rl.buckets)
	rl.mu.Unlock()
	if after > 2 {
		t.Fatalf("idle full buckets were not pruned: %d remain (was %d)", after, before)
	}
}

// TestRateLimiterStillLimitsAfterPrune: pruning must not hand a hot key a fresh
// allowance. A bucket that is still being consumed is never idle-full, so it is
// never a prune candidate.
func TestRateLimiterStillLimitsAfterPrune(t *testing.T) {
	rl := newRateLimiter(3, time.Hour) // effectively no refill during the test

	for i := 0; i < 3; i++ {
		if !rl.allow("hot") {
			t.Fatalf("allow %d denied, want allowed within burst", i)
		}
	}
	// Insert other keys to drive the prune path repeatedly.
	for i := 0; i < 50; i++ {
		rl.allow("other-" + strconv.Itoa(i))
	}
	if rl.allow("hot") {
		t.Fatal("exhausted key was allowed again: the prune dropped a live bucket")
	}
}
