package agentws

import (
	"encoding/json"
	"testing"
)

// session-display-update amendment: agentws decode test for the optional
// render_width/render_height/ui_scale keys on session_metrics
// (agent-api.md § session_metrics).

// TestSessionMetricsDecodeRenderResolutionUIScale: a session_metrics JSON
// message carrying render_width/render_height/ui_scale unmarshals into the
// corresponding SessionMetricsMsg pointer fields.
func TestSessionMetricsDecodeRenderResolutionUIScale(t *testing.T) {
	raw := []byte(`{
		"type": "session_metrics",
		"session_id": "aaaaaaaa-0000-0000-0000-000000000001",
		"ts_unix_ms": 1735689600000,
		"render_width": 1280,
		"render_height": 720,
		"ui_scale": 1.5
	}`)

	var m SessionMetricsMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if m.Type != "session_metrics" {
		t.Errorf("type: got %q want %q", m.Type, "session_metrics")
	}
	if m.RenderWidth == nil || *m.RenderWidth != 1280 {
		t.Errorf("render_width: got %v want 1280", m.RenderWidth)
	}
	if m.RenderHeight == nil || *m.RenderHeight != 720 {
		t.Errorf("render_height: got %v want 720", m.RenderHeight)
	}
	if m.UIScale == nil || *m.UIScale != 1.5 {
		t.Errorf("ui_scale: got %v want 1.5", m.UIScale)
	}
}

// TestSessionMetricsDecodeRenderResolutionUIScaleAbsent: an old-agent message
// with no render/scale keys leaves the pointer fields nil (omit-when-default
// convention, not a decode error).
func TestSessionMetricsDecodeRenderResolutionUIScaleAbsent(t *testing.T) {
	raw := []byte(`{
		"type": "session_metrics",
		"session_id": "aaaaaaaa-0000-0000-0000-000000000001",
		"ts_unix_ms": 1735689600000,
		"fps": 60
	}`)

	var m SessionMetricsMsg
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}
	if m.RenderWidth != nil {
		t.Errorf("render_width: got %v want nil", *m.RenderWidth)
	}
	if m.RenderHeight != nil {
		t.Errorf("render_height: got %v want nil", *m.RenderHeight)
	}
	if m.UIScale != nil {
		t.Errorf("ui_scale: got %v want nil", *m.UIScale)
	}
}
