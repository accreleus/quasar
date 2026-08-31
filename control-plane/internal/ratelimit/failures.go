// Package ratelimit contains small in-memory admission limiters shared by HTTP
// and WebSocket endpoints.
package ratelimit

import (
	"sync"
	"time"
)

// FailureLimiter blocks a key after limit failures within ttl. Successful
// authentication should call Forget so legitimate clients do not retain stale
// penalties. State is capped; when full, the oldest entry is evicted.
type FailureLimiter struct {
	mu         sync.Mutex
	entries    map[string]failureEntry
	limit      int
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	inFlight   int
}

type failureEntry struct {
	count    int
	inFlight int
	last     time.Time
}

// Reserve admits and counts one in-progress handshake. It bounds slow clients
// before authentication failures become visible. Call Release exactly once.
func (l *FailureLimiter) Reserve(key string, maxPerKey, maxTotal int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneExpired(now)
	e := l.entries[key]
	if e.count >= l.limit || e.inFlight >= maxPerKey || l.inFlight >= maxTotal {
		return false
	}
	if _, exists := l.entries[key]; !exists && len(l.entries) >= l.maxEntries {
		if !l.evictOldest() {
			return false
		}
	}
	e = l.entries[key]
	e.inFlight++
	e.last = now
	l.entries[key] = e
	l.inFlight++
	return true
}

func (l *FailureLimiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || e.inFlight == 0 {
		return
	}
	e.inFlight--
	l.inFlight--
	if e.count == 0 && e.inFlight == 0 {
		delete(l.entries, key)
	} else {
		l.entries[key] = e
	}
}

func NewFailureLimiter(limit int, ttl time.Duration, maxEntries int) *FailureLimiter {
	return &FailureLimiter{
		entries:    make(map[string]failureEntry),
		limit:      limit,
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// Allow reports whether key may attempt a handshake. It does not consume or
// mutate a successful client's allowance.
func (l *FailureLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneExpired(now)
	e, ok := l.entries[key]
	return !ok || e.count < l.limit
}

// Failure records one failed authentication/handshake for key.
func (l *FailureLimiter) Failure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneExpired(now)
	if _, exists := l.entries[key]; !exists && len(l.entries) >= l.maxEntries {
		if !l.evictOldest() {
			return
		}
	}
	e := l.entries[key]
	e.count++
	e.last = now
	l.entries[key] = e
}

// Forget clears prior failures after a valid authentication.
func (l *FailureLimiter) Forget(key string) {
	l.mu.Lock()
	if e, ok := l.entries[key]; ok && e.inFlight > 0 {
		e.count = 0
		l.entries[key] = e
	} else {
		delete(l.entries, key)
	}
	l.mu.Unlock()
}

func (l *FailureLimiter) pruneExpired(now time.Time) {
	for key, e := range l.entries {
		if e.inFlight == 0 && now.Sub(e.last) >= l.ttl {
			delete(l.entries, key)
		}
	}
}

func (l *FailureLimiter) evictOldest() bool {
	var oldestKey string
	var oldest time.Time
	for key, e := range l.entries {
		if e.inFlight > 0 {
			continue
		}
		if oldestKey == "" || e.last.Before(oldest) {
			oldestKey, oldest = key, e.last
		}
	}
	if oldestKey == "" {
		return false
	}
	delete(l.entries, oldestKey)
	return true
}
