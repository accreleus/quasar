package session

import (
	"log/slog"
	"sync"
	"time"
)

// ingestCounters records what client ingest dropped, per session, in memory.
// Not persisted: a table would cost a write on the ingest path, which must
// never slow down, to buy durability nobody would read. A control-plane restart
// resets it; the bundle field is optional for that reason.
type ingestCounters struct {
	mu sync.Mutex
	by map[string]*ingestStats
}

type ingestStats struct {
	RejectedTs   int64
	LastTsUnixMs int64
	LastReason   string

	lastSeen  time.Time
	lastLogAt time.Time
}

// ingestReport is the bundle's `ingest` object; absent entirely when nothing
// was rejected.
type ingestReport struct {
	RejectedTs           int64  `json:"rejected_ts"`
	LastRejectedTsUnixMs int64  `json:"last_rejected_ts_unix_ms,omitempty"`
	LastRejectedReason   string `json:"last_rejected_reason,omitempty"`
}

// ingestCounterIdle: how long a session's counters survive without a rejection
// before the sweep drops them.
const ingestCounterIdle = 2 * time.Hour

// ingestLogEvery bounds the WARN to one per session per minute, so a client
// stuck posting at 1 Hz forever doesn't itself become the problem.
const ingestLogEvery = time.Minute

func newIngestCounters() *ingestCounters {
	return &ingestCounters{by: map[string]*ingestStats{}}
}

// reject records one dropped point and returns true when the caller should log.
func (c *ingestCounters) reject(sessionID string, ts int64, reason string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.by[sessionID]
	if !ok {
		c.sweepLocked(now)
		st = &ingestStats{}
		c.by[sessionID] = st
	}
	st.RejectedTs++
	st.LastTsUnixMs = ts
	st.LastReason = reason
	st.lastSeen = now

	if now.Sub(st.lastLogAt) < ingestLogEvery {
		return false
	}
	st.lastLogAt = now
	return true
}

func (c *ingestCounters) sweepLocked(now time.Time) {
	for id, st := range c.by {
		if now.Sub(st.lastSeen) >= ingestCounterIdle {
			delete(c.by, id)
		}
	}
}

func (c *ingestCounters) report(sessionID string) *ingestReport {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.by[sessionID]
	if !ok || st.RejectedTs == 0 {
		return nil
	}
	return &ingestReport{
		RejectedTs:           st.RejectedTs,
		LastRejectedTsUnixMs: st.LastTsUnixMs,
		LastRejectedReason:   st.LastReason,
	}
}

// acceptTs is the ingest gate: returns whether the point may be stored, having
// already counted and (at most once a minute) logged a rejection. Nil-safe.
func (h *Handler) acceptTs(sessionID, kind string, ts int64) bool {
	ok, reason := plausibleTs(ts)
	if ok {
		return true
	}
	if h.ingest == nil {
		return false
	}
	if h.ingest.reject(sessionID, ts, reason, time.Now()) {
		slog.Warn("dropped client telemetry with an implausible ts_unix_ms",
			"session_id", sessionID, "kind", kind, "ts_unix_ms", ts, "reason", reason,
			"detail", "the point would have been stored where every read window excludes it; see the bundle's ingest counters")
	}
	return false
}
