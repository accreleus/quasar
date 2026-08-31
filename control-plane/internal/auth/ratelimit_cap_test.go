package auth

import (
	"strconv"
	"testing"
	"time"
)

// TestLoginLimiterIsCapped: the login limiter is keyed by client IP with no
// size backstop (#404). Within its 50s idle window a single real host rotating
// an IPv6 /64 mints unbounded distinct keys, and — worse than the memory — each
// NEW key walked the whole map under the lock, so the CPU cost grew
// quadratically with the rotation. internal/ratelimit/failures.go already had
// the answer (TTL prune + maxEntries + evictOldest); this ports it.
func TestLoginLimiterIsCapped(t *testing.T) {
	// Long refill so nothing is idle-prunable: the cap must hold on its own.
	rl := newLoginLimiter(10, time.Hour)

	for i := 0; i < loginLimiterMaxEntries*2; i++ {
		rl.allow("2001:db8::" + strconv.Itoa(i))
	}

	rl.mu.Lock()
	n := len(rl.buckets)
	rl.mu.Unlock()
	if n > loginLimiterMaxEntries {
		t.Fatalf("bucket map = %d entries, want <= cap %d", n, loginLimiterMaxEntries)
	}
}

// TestLoginLimiterEvictsOldestNotNewest: eviction must drop the least recently
// seen key. Dropping an active one would hand a live attacker a fresh
// allowance every time the map filled.
func TestLoginLimiterEvictsOldestNotNewest(t *testing.T) {
	rl := newLoginLimiter(2, time.Hour)

	// Exhaust one key's allowance, then keep it warm while the map fills.
	rl.allow("attacker")
	rl.allow("attacker")
	if rl.allow("attacker") {
		t.Fatal("setup: attacker still had tokens")
	}

	for i := 0; i < loginLimiterMaxEntries*2; i++ {
		rl.allow("filler-" + strconv.Itoa(i))
		if i%64 == 0 {
			// Touch the attacker so it is never the oldest entry.
			rl.allow("attacker")
		}
	}
	if rl.allow("attacker") {
		t.Fatal("a warm, exhausted key was evicted and reset by the cap")
	}
}

// TestLoginLimiterStillPrunesIdle: the TTL semantics must survive the cap —
// a fully refilled idle bucket is still dropped on the next insert.
func TestLoginLimiterStillPrunesIdle(t *testing.T) {
	// 100 ms (not 1 ms) so the fill loop cannot outlast the prune window under
	// -race and trigger the prune mid-setup.
	const burst = 2
	const refill = 100 * time.Millisecond
	rl := newLoginLimiter(burst, refill)

	for i := 0; i < 200; i++ {
		rl.allow("ip-" + strconv.Itoa(i))
	}
	rl.mu.Lock()
	filled := len(rl.buckets)
	rl.mu.Unlock()
	if filled != 200 {
		t.Fatalf("setup: %d buckets, want 200", filled)
	}

	time.Sleep(burst*refill + 50*time.Millisecond)
	rl.allow("fresh")

	rl.mu.Lock()
	n := len(rl.buckets)
	rl.mu.Unlock()
	if n > 2 {
		t.Fatalf("idle full buckets were not pruned: %d remain", n)
	}
}
