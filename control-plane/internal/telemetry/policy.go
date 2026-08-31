package telemetry

import (
	"fmt"
	"time"
)

// Telemetry retention: two rules and one exemption, applied by one periodic
// janitor (Store.Retain).
//
//  1. Rolling window — while a session is non-terminal, samples and events
//     older than Rolling are deleted; this bounds a long-lived session.
//  2. Post-mortem retention — at a terminal state the last Rolling of
//     telemetry is frozen and kept for PostMortem, then swept (samples,
//     non-capture events, the clock row): the verdict and bundle reads must
//     still answer on a session that stopped hours ago.
//  3. Captures (diag.*) are exempt from both — a human asked for them, they
//     are sparse and byte-capped — and leave only via the session row's ON
//     DELETE CASCADE.
//
// Both durations measure against the server-side created_at, never the
// reporter's ts_unix_ms — a skewed or hostile reporter clock must not be able
// to evade the cap.

// Retention defaults. Operators override them with
// QUASAR_TELEMETRY_ROLLING_WINDOW / QUASAR_TELEMETRY_POSTMORTEM_RETENTION.
const (
	DefaultRolling    = time.Hour
	DefaultPostMortem = 24 * time.Hour
	// DefaultBatch is how many rows one DELETE statement may take. The sweep
	// loops until a statement deletes nothing, so a backlog is drained in bounded
	// chunks and never holds a long lock on a table the ingest path is writing.
	DefaultBatch = 5000
	// maxBatchIterations caps one Retain pass so a pathological backlog (or a
	// bug) cannot make the janitor run unboundedly. What it does not finish this
	// pass, it finishes on the next.
	maxBatchIterations = 200
)

// Policy is the retention configuration in force for one Retain pass.
type Policy struct {
	// Rolling is the live per-session window: while a session is non-terminal,
	// telemetry older than this is swept.
	Rolling time.Duration
	// PostMortem is how long a TERMINAL session's frozen telemetry is kept for
	// diagnosis before it is swept. Must be >= Rolling — a post-mortem shorter
	// than the window it preserves would delete the evidence it exists to keep.
	PostMortem time.Duration
	// Batch is the per-statement row cap (DefaultBatch when <= 0).
	Batch int
}

// DefaultPolicy is retention as it ships.
func DefaultPolicy() Policy {
	return Policy{Rolling: DefaultRolling, PostMortem: DefaultPostMortem, Batch: DefaultBatch}
}

// Validate rejects a policy that cannot mean what it says. It is called at
// config load so a typo is a loud boot failure, not silent data loss.
func (p Policy) Validate() error {
	if p.Rolling <= 0 {
		return fmt.Errorf("telemetry rolling window must be positive, got %s", p.Rolling)
	}
	if p.PostMortem <= 0 {
		return fmt.Errorf("telemetry post-mortem retention must be positive, got %s", p.PostMortem)
	}
	if p.PostMortem < p.Rolling {
		return fmt.Errorf("telemetry post-mortem retention (%s) must be >= the rolling window (%s): "+
			"a post-mortem shorter than the window it preserves deletes the evidence it exists to keep",
			p.PostMortem, p.Rolling)
	}
	return nil
}

func (p Policy) normalized() Policy {
	if p.Rolling <= 0 {
		p.Rolling = DefaultRolling
	}
	if p.PostMortem <= 0 {
		p.PostMortem = DefaultPostMortem
	}
	if p.PostMortem < p.Rolling {
		p.PostMortem = p.Rolling
	}
	if p.Batch <= 0 {
		p.Batch = DefaultBatch
	}
	return p
}

// Disposition is what the policy decides about one stored row.
type Disposition int

const (
	// Keep: the row stays.
	Keep Disposition = iota
	// SweepRolling: the row aged out of a live session's rolling window.
	SweepRolling
	// SweepPostMortem: the row belongs to a terminal session whose post-mortem
	// retention has expired.
	SweepPostMortem
)

func (d Disposition) String() string {
	switch d {
	case SweepRolling:
		return "sweep_rolling"
	case SweepPostMortem:
		return "sweep_post_mortem"
	default:
		return "keep"
	}
}

// Row is the minimum a retention decision needs about one stored telemetry row.
// It is deliberately not a table row type: the SQL in the Postgres adapter and
// the maps in the Fake adapter both have to agree with THIS function, and the
// only way to state that agreement once is to keep the decision pure.
type Row struct {
	// CreatedAt is the server-side ingestion clock (never the reporter's).
	CreatedAt time.Time
	// SessionTerminal is true when the owning session has reached stopped/failed.
	SessionTerminal bool
	// SessionEndedAt is when the session reached its terminal state (the sessions
	// row's ended_at, falling back to updated_at). Meaningless unless
	// SessionTerminal.
	SessionEndedAt time.Time
	// Type is the event type for an event row, and "" for a sample row. Only
	// events can be captures.
	Type string
}

// Decide is the retention policy. Every deletion this package performs must
// agree with it, and the adapters' tests prove they do.
func (p Policy) Decide(r Row, now time.Time) Disposition {
	p = p.normalized()
	// Rule 3 first: a capture is exempt from every rule below it.
	if IsCapture(r.Type) {
		return Keep
	}
	if r.SessionTerminal {
		// Rule 2. The frozen window is kept whole until the post-mortem expires —
		// note the rolling window is NOT re-applied to a terminal session, which
		// is exactly what makes "the last hour of a failed session" survivable.
		if now.Sub(r.SessionEndedAt) >= p.PostMortem {
			return SweepPostMortem
		}
		return Keep
	}
	// Rule 1.
	if now.Sub(r.CreatedAt) >= p.Rolling {
		return SweepRolling
	}
	return Keep
}

// Report is what one Retain pass did. Counts are rows, not statements.
type Report struct {
	RollingSamples    int64
	RollingEvents     int64
	PostMortemSamples int64
	PostMortemEvents  int64
	PostMortemClocks  int64
	// Truncated is true when the pass hit maxBatchIterations with rows still to
	// delete — the next pass continues. It is the signal that a backlog exists.
	Truncated bool
	Duration  time.Duration
}

// Total is every row the pass deleted.
func (r Report) Total() int64 {
	return r.RollingSamples + r.RollingEvents +
		r.PostMortemSamples + r.PostMortemEvents + r.PostMortemClocks
}

// LogArgs renders the report as slog key/value pairs, so the janitor's one line
// per run has the same shape wherever it is emitted.
func (r Report) LogArgs() []any {
	return []any{
		"rolling_samples", r.RollingSamples,
		"rolling_events", r.RollingEvents,
		"postmortem_samples", r.PostMortemSamples,
		"postmortem_events", r.PostMortemEvents,
		"postmortem_clocks", r.PostMortemClocks,
		"total", r.Total(),
		"truncated", r.Truncated,
		"took_ms", r.Duration.Milliseconds(),
	}
}
