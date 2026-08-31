package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// ST-03 integration tests — agent trace event ingestion. Skip without TEST_DATABASE_URL.

// TestAgentTraceEventHappyPath: a trace event from the owning host is persisted
// as a source='agent' row in session_trace_events.
func TestAgentTraceEventHappyPath(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sid := runningSession(t, store, s).ID

	coord.AgentTraceEvent(ctx, s.hostID, agentws.SessionTraceEventMsg{
		Type:      "session_trace_event",
		SessionID: sid,
		TsUnixMs:  1735689600000,
		Event:     "abr.retarget",
		Payload:   json.RawMessage(`{"from_kbps":14000,"to_kbps":11000,"reason":"gcc_downshift"}`),
	})

	if n := countTraceEvents(t, pool, sid); n != 1 {
		t.Fatalf("event rows: got %d want 1", n)
	}
	events, err := store.Telemetry().Events(ctx, sid, telemetry.Range{FromMs: 0, ToMs: 0}, telemetry.Filter{Types: nil, Limit: 10})
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("query: got %d want 1", len(events))
	}
	if events[0].Source != telemetry.SourceAgent {
		t.Errorf("source: got %q want %q", events[0].Source, telemetry.SourceAgent)
	}
	if events[0].Type != "abr.retarget" {
		t.Errorf("type: got %q want %q", events[0].Type, "abr.retarget")
	}
	var p map[string]any
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if p["reason"] != "gcc_downshift" {
		t.Errorf("payload reason: got %v", p["reason"])
	}
}

// TestAgentTraceEventCrossHostRejected: an agent event whose host does not own
// the session is silently dropped — no row is stored.
func TestAgentTraceEventCrossHostRejected(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sess := runningSession(t, store, s)
	sid := sess.ID

	// A UUID that does not match the session's owning host.
	otherHost := "00000000-0000-0000-0000-0000000000aa"
	if sess.HostID != nil && *sess.HostID == otherHost {
		otherHost = "00000000-0000-0000-0000-0000000000bb"
	}

	coord.AgentTraceEvent(ctx, otherHost, agentws.SessionTraceEventMsg{
		Type:      "session_trace_event",
		SessionID: sid,
		TsUnixMs:  1735689600000,
		Event:     "abr.retarget",
		Payload:   json.RawMessage(`{}`),
	})

	if n := countTraceEvents(t, pool, sid); n != 0 {
		t.Fatalf("cross-host trace event was stored: got %d rows want 0", n)
	}
}

// TestAgentTraceEventEmptyFieldsDropped: a trace event with an empty session_id
// or empty event type is discarded before any DB call (defensive guard in
// AgentTraceEvent).
func TestAgentTraceEventEmptyFieldsDropped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sid := runningSession(t, store, s).ID

	// Empty session_id: should be dropped without a DB round-trip.
	coord.AgentTraceEvent(ctx, s.hostID, agentws.SessionTraceEventMsg{
		Type:      "session_trace_event",
		SessionID: "",
		TsUnixMs:  1,
		Event:     "abr.retarget",
		Payload:   json.RawMessage(`{}`),
	})

	// Empty event type: also dropped.
	coord.AgentTraceEvent(ctx, s.hostID, agentws.SessionTraceEventMsg{
		Type:      "session_trace_event",
		SessionID: sid,
		TsUnixMs:  1,
		Event:     "",
		Payload:   json.RawMessage(`{}`),
	})

	if n := countTraceEvents(t, pool, sid); n != 0 {
		t.Fatalf("empty-field events were stored: got %d rows want 0", n)
	}
}
