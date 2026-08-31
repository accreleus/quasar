// Package telemetry owns per-session observability storage — session_metrics,
// session_trace_events, session_trace_clock — and retention as a policy rather
// than inline DELETEs. One store, one limit clamp, one retention policy is the
// point (it replaced two near-identical stores in internal/session).
//
// Everything here is observability: a telemetry write is never on the session
// lifecycle hot path, never alters a session row, and is never an
// access-control or session-state authority — trust boundaries stay in
// internal/session, which owns the sessions table.
package telemetry

import (
	"context"
	"encoding/json"
	"time"
)

// SourceAgent / SourceBrowser / SourceNative are the session_metrics.source
// values (schema.md CHECK (source IN ('agent','browser','native'))). SourceNative
// is the P9-07 native-client reporter, posted via the same client-submitted
// telemetry path as SourceBrowser (POST /v1/sessions/{id}/stats, "client":"native").
const (
	SourceAgent   = "agent"
	SourceBrowser = "browser"
	SourceNative  = "native"
)

// CaptureTypePrefix is the event-type prefix of an on-demand capture result
// (agent-api.md §session_trace_event, diag.*). Captures are EXEMPT from both
// retention rules — see Policy.
const CaptureTypePrefix = "diag."

// capturePattern is CaptureTypePrefix as a SQL LIKE pattern.
const capturePattern = CaptureTypePrefix + "%"

// Sample is one session_metrics row as a read returns it. The server-only
// created_at is intentionally not surfaced — it is the ingestion clock the
// retention policy measures against, not a reporter-visible field (schema.md).
type Sample struct {
	Source   string
	TsUnixMs int64
	Metrics  json.RawMessage
}

// SampleInput is one already-filtered sample for an append. Source is per-call
// (one reporter writes one batch), so it is not repeated per row.
type SampleInput struct {
	TsUnixMs int64
	Metrics  json.RawMessage
}

// Event is one session_trace_events row as a read returns it. Same posture as
// Sample: created_at is not surfaced.
type Event struct {
	Source   string
	TsUnixMs int64
	Type     string
	Payload  json.RawMessage
}

// EventInput is one event for an append.
type EventInput struct {
	TsUnixMs int64
	Type     string
	Payload  json.RawMessage
}

// Clock is the per-session client↔host clock-offset estimate
// (session_trace_clock). Its ABSENCE is "unmeasured" — there is no sentinel
// ClientOffsetMs=0 (trace-format.md §4, no false precision), so a read signals
// absence with a nil return, never a zero value.
type Clock struct {
	ClientOffsetMs float64
	UncertaintyMs  float64
	MeasuredAt     time.Time
	UpdatedAt      time.Time
}

// Latest is the most-recent sample per source for one session, kept
// SOURCE-SCOPED: frames_dropped means different things to the agent and to the
// browser, so a merged latest must never blind-overlay one source's keys over
// the other's (schema.md). Either field may be nil when that source has produced
// no telemetry.
type Latest struct {
	Agent   *Sample
	Browser *Sample
}

// Range is a bounded read window on the REPORTER clock (ts_unix_ms, inclusive).
// A zero bound means "no bound" on that side, so a caller may pass only a lower
// or only an upper bound.
type Range struct {
	FromMs int64
	ToMs   int64
}

// Filter narrows a read. Types, when non-empty, restricts events to those types
// (the typed read served by session_trace_events_session_type_ts_idx). Limit
// bounds each series independently and is clamped by clampLimit — the ONE clamp
// in this package, where three identical copies used to live.
type Filter struct {
	Types []string
	Limit int32
}

// Slice is the joined raw material one bounded window yields for a session: the
// samples in window (both sources, newest first), the events in window (newest
// first), and the clock row (nil ⇒ unmeasured). Assembly only — the taxonomy
// normalization, derived windows and classifier verdict are the session
// package's job, deliberately not built here.
type Slice struct {
	Samples []Sample
	Events  []Event
	Clock   *Clock // nil ⇒ unmeasured
}

// Store is the whole telemetry surface. Two adapters implement it: Postgres (the
// production one) and Fake (in-memory, for handler and policy tests that need no
// database).
//
// Read semantics that are load-bearing and must hold for every adapter:
//   - reads are newest-first (ts_unix_ms DESC),
//   - Limit is clamped identically everywhere (clampLimit),
//   - Latest is source-scoped and never merged,
//   - Clock absence is nil, never a zero value.
type Store interface {
	// Append writes one sample. A nil/empty Metrics defaults to "{}".
	Append(ctx context.Context, sessionID, source string, s SampleInput) error
	// AppendBatch writes many samples for one (session, source) in a single
	// round-trip. No-op for an empty slice.
	AppendBatch(ctx context.Context, sessionID, source string, samples []SampleInput) error
	// AppendEvent writes one trace event. A nil/empty Payload defaults to "{}".
	AppendEvent(ctx context.Context, sessionID, source string, e EventInput) error
	// AppendEventReturningID is AppendEvent plus the generated row id — the
	// operator-annotation POST returns it as 201 { "id": … }.
	AppendEventReturningID(ctx context.Context, sessionID, source string, e EventInput) (string, error)
	// AppendEvents writes many events for one (session, source) in a single
	// round-trip. No-op for an empty slice.
	AppendEvents(ctx context.Context, sessionID, source string, events []EventInput) error

	// UpsertClock writes (or refines) the per-session offset estimate. There is
	// no "unmeasured" write — unmeasured is the absence of the row, so a caller
	// with no measurement simply never calls this.
	UpsertClock(ctx context.Context, sessionID string, clientOffsetMs, uncertaintyMs float64) error
	// Clock reads the offset estimate; (nil, nil) means UNMEASURED.
	Clock(ctx context.Context, sessionID string) (*Clock, error)

	// Window returns samples + events + clock for one bounded window. Filter.Types
	// applies to the events only.
	Window(ctx context.Context, sessionID string, r Range, f Filter) (Slice, error)
	// Events returns just the events for one bounded window (the typed read).
	Events(ctx context.Context, sessionID string, r Range, f Filter) ([]Event, error)
	// Recent returns the bounded recent-N window of samples, newest first, both
	// sources unless source is non-nil. Pagination is offset-cursor, matching the
	// session list scheme; the returned cursor is empty at the end.
	Recent(ctx context.Context, sessionID string, limit int32, source *string, cursor string) ([]Sample, string, error)
	// LatestPerSession returns each requested session's most-recent sample per
	// source in ONE query (no N+1). Sessions with no telemetry are absent.
	LatestPerSession(ctx context.Context, sessionIDs []string) (map[string]Latest, error)

	// Captures returns a session's capture results (diag.*), newest first.
	Captures(ctx context.Context, sessionID string) ([]Event, error)
	// Capture returns ONE capture by its capture_id. ErrNotFound until it lands —
	// that 404 is the poll signal, not an error.
	Capture(ctx context.Context, sessionID, captureID string) (Event, error)

	// Retain applies the retention policy across every session in one pass and
	// reports what it deleted. It is the ONLY thing in the system that deletes
	// telemetry; no ingest path prunes.
	Retain(ctx context.Context, p Policy) (Report, error)
}

// clampLimit is the one read clamp: non-positive means 100, and no read may
// ask for more than 1000 rows.
func clampLimit(limit int32) int32 {
	if limit <= 0 {
		return 100
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

// defaultJSON normalizes a nil/empty JSON value to an empty object, so a
// malformed or absent payload is stored as {} rather than NULL or an error.
func defaultJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// IsCapture reports whether an event type is a capture result — the one event
// class exempt from both retention rules.
func IsCapture(eventType string) bool {
	return len(eventType) >= len(CaptureTypePrefix) && eventType[:len(CaptureTypePrefix)] == CaptureTypePrefix
}
