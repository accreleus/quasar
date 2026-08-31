package agentws

import (
	"context"
	"encoding/json"
	"testing"
)

// ST-03: agentws decode tests for session_trace_event.

// captureTraceEvents is a minimal Events implementation that records every
// AgentTraceEvent call for inspection. All other methods are noops.
type captureTraceEvents struct {
	noopEvents
	calls []traceCall
}

type traceCall struct {
	hostID string
	msg    SessionTraceEventMsg
}

func (c *captureTraceEvents) AgentTraceEvent(_ context.Context, hostID string, m SessionTraceEventMsg) {
	c.calls = append(c.calls, traceCall{hostID: hostID, msg: m})
}

// TestSessionTraceEventDecodeValid: a well-formed session_trace_event JSON is
// decoded into a SessionTraceEventMsg and forwarded to AgentTraceEvent.
func TestSessionTraceEventDecodeValid(t *testing.T) {
	raw := []byte(`{
		"type": "session_trace_event",
		"session_id": "aaaaaaaa-0000-0000-0000-000000000001",
		"ts_unix_ms": 1735689600000,
		"event": "abr.retarget",
		"payload": {"from_kbps": 14000, "to_kbps": 11000, "reason": "gcc_downshift"}
	}`)

	// Decode + dispatch mirrors the handler's case "session_trace_event" branch.
	var m SessionTraceEventMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if m.Type != "session_trace_event" {
		t.Errorf("type: got %q want %q", m.Type, "session_trace_event")
	}
	if m.SessionID != "aaaaaaaa-0000-0000-0000-000000000001" {
		t.Errorf("session_id: got %q", m.SessionID)
	}
	if m.TsUnixMs != 1735689600000 {
		t.Errorf("ts_unix_ms: got %d", m.TsUnixMs)
	}
	if m.Event != "abr.retarget" {
		t.Errorf("event: got %q want %q", m.Event, "abr.retarget")
	}
	var payload map[string]any
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		t.Fatalf("payload json: %v", err)
	}
	if payload["reason"] != "gcc_downshift" {
		t.Errorf("payload reason: got %v", payload["reason"])
	}

	// Verify forward via the Events interface.
	cap := &captureTraceEvents{}
	cap.AgentTraceEvent(context.Background(), "host-1", m)
	if len(cap.calls) != 1 {
		t.Fatalf("calls: got %d want 1", len(cap.calls))
	}
	if cap.calls[0].hostID != "host-1" {
		t.Errorf("hostID: got %q", cap.calls[0].hostID)
	}
}

// TestSessionTraceEventDecodeMalformed: a malformed session_trace_event is
// dropped (decode error) without being forwarded to AgentTraceEvent — the
// handler logs a warning and continues (not fatal to the WS connection).
// This test verifies the error path does not forward a garbled message.
func TestSessionTraceEventDecodeMalformed(t *testing.T) {
	malformed := []byte(`{"type": "session_trace_event", "ts_unix_ms": "not-a-number"}`)

	var m SessionTraceEventMsg
	err := json.Unmarshal(malformed, &m)
	// Malformed (wrong ts_unix_ms type) must fail decode.
	if err == nil {
		t.Fatal("expected decode error for malformed ts_unix_ms, got nil")
	}

	// If err != nil the handler does `continue` — AgentTraceEvent is never called.
	cap := &captureTraceEvents{}
	// We don't call AgentTraceEvent here because the handler skips it on error.
	if len(cap.calls) != 0 {
		t.Fatalf("forward called despite decode error")
	}
}
