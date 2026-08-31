package agentws

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestValidDiagEventAcceptsAWellFormedCapture: the ordinary result — a bounded
// payload carrying the capture_id that GET .../captures/{id} will look it up by.
func TestValidDiagEventAcceptsAWellFormedCapture(t *testing.T) {
	m := SessionTraceEventMsg{
		Type: "session_trace_event", SessionID: "s-1", Event: "diag.pipeline_dot",
		Payload: json.RawMessage(`{"capture_id":"c-1","kind":"pipeline_dot","encoding":"gzip+base64","data":"H4sI"}`),
	}
	if !validDiagEvent(m, quietLog(), "host-1") {
		t.Fatal("a well-formed capture result was dropped")
	}
}

// TestValidDiagEventRejectsMissingCaptureID: without a capture_id the row is
// unaddressable — and because diag.* is prune-exempt, an unaddressable row is one
// nothing ever reads and nothing ever reaps.
func TestValidDiagEventRejectsMissingCaptureID(t *testing.T) {
	for name, payload := range map[string]string{
		"absent":     `{"kind":"pipeline_dot"}`,
		"empty":      `{"capture_id":"","kind":"pipeline_dot"}`,
		"not-object": `"just a string"`,
		"malformed":  `{`,
	} {
		m := SessionTraceEventMsg{SessionID: "s-1", Event: "diag.pipeline_dot",
			Payload: json.RawMessage(payload)}
		if validDiagEvent(m, quietLog(), "host-1") {
			t.Fatalf("%s payload was accepted: %s", name, payload)
		}
	}
}

// TestValidDiagEventRejectsOverTheWireCap: the belt-and-braces bound beside the
// agent's own byte budget. The cap is on the JSON payload, well above the 256 KiB
// compressed budget, because a maximum capture travels base64-encoded.
func TestValidDiagEventRejectsOverTheWireCap(t *testing.T) {
	big := `{"capture_id":"c-1","data":"` + strings.Repeat("A", maxDiagPayloadBytes) + `"}`
	m := SessionTraceEventMsg{SessionID: "s-1", Event: "diag.pipeline_dot",
		Payload: json.RawMessage(big)}
	if validDiagEvent(m, quietLog(), "host-1") {
		t.Fatalf("a %d-byte payload was accepted over a %d cap", len(big), maxDiagPayloadBytes)
	}

	// A capture at the full 256 KiB byte budget, base64-inflated, must still fit:
	// the cap exists to stop abuse, not to reject a legal maximum capture.
	atBudget := `{"capture_id":"c-1","data":"` + strings.Repeat("A", 262144*4/3) + `"}`
	m.Payload = json.RawMessage(atBudget)
	if !validDiagEvent(m, quietLog(), "host-1") {
		t.Fatalf("a legal maximum-budget capture (%d bytes on the wire) was rejected", len(atBudget))
	}
}
