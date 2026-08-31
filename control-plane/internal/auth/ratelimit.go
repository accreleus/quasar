package auth

import (
	"sync"
	"time"
)

// loginLimiter is a per-client-IP token bucket guarding the credential
// endpoints (login/register). argon2id makes each verify expensive
// server-side, but without a gate an attacker can still run unlimited
// parallel guesses. Same shape as the session/devices limiters.
//
// Burst tolerates a small flurry of legitimate retries (typo'd password,
// SPA retry); the refill keeps the steady ceiling far below brute-force
// rates while never locking out a real user for long.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	burst   int
	refill  time.Duration
}

// loginLimiterMaxEntries is the hard backstop on distinct keys (#404). The TTL
// prune alone is not one: it only reclaims buckets idle for burst x refill
// (50 s at the production settings), so within that window a single real host
// rotating addresses across an IPv6 /64 mints unbounded keys — and because the
// prune sweeps the whole map on every miss, the CPU cost of that rotation grows
// quadratically, which is the sharper risk of the two.
//
// 4096 matches the in-repo precedent (internal/ratelimit.FailureLimiter as
// constructed by agentws and signal, both at 4096): far above any plausible
// count of legitimate distinct client IPs authenticating inside a 50 s window,
// far below anything that matters for memory.
const loginLimiterMaxEntries = 4096

type ipBucket struct {
	tokens float64
	last   time.Time
}

func newLoginLimiter(burst int, refill time.Duration) *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*ipBucket), burst: burst, refill: refill}
}

// allow reports whether the key may proceed, consuming a token if so.
// Full idle buckets are pruned lazily to bound the map.
func (rl *loginLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[key]
	if !ok {
		// Prune full idle buckets opportunistically before inserting.
		for k, old := range rl.buckets {
			if now.Sub(old.last).Seconds()/rl.refill.Seconds() >= float64(rl.burst) {
				delete(rl.buckets, k)
			}
		}
		// Hard cap (#404): if the prune reclaimed nothing, evict the least
		// recently seen bucket. Oldest-first is load-bearing — evicting an
		// arbitrary (or the newest) entry would hand a live attacker a fresh
		// allowance every time the map filled, turning the backstop into a
		// bypass. Mirrors ratelimit.FailureLimiter.evictOldest.
		if len(rl.buckets) >= loginLimiterMaxEntries {
			rl.evictOldest()
		}
		b = &ipBucket{tokens: float64(rl.burst), last: now}
		rl.buckets[key] = b
	}
	if rl.refill > 0 {
		b.tokens += now.Sub(b.last).Seconds() / rl.refill.Seconds()
	}
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// evictOldest drops the least recently seen bucket. Callers must hold rl.mu.
// A no-op on an empty map.
func (rl *loginLimiter) evictOldest() {
	var oldestKey string
	var oldest time.Time
	for key, b := range rl.buckets {
		if oldestKey == "" || b.last.Before(oldest) {
			oldestKey, oldest = key, b.last
		}
	}
	if oldestKey != "" {
		delete(rl.buckets, oldestKey)
	}
}

// The limiter key comes from internal/ratelimit.ClientIP (#438) — this package
// no longer has its own RemoteAddr-only copy. One helper means one place where
// the trusted-proxy policy is decided, and no endpoint can quietly drift off it.
