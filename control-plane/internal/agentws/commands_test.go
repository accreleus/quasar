package agentws

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestStreamSpecMicOmitsWhenFalse — microphone capture amendment (2026-08-02,
// spec §3.5): Mic must carry the same omitempty discipline as Codec/
// AbrFloorKbps, so a non-mic session_assign is byte-identical to the
// pre-amendment wire shape (an older agent parsing it sees nothing new).
func TestStreamSpecMicOmitsWhenFalse(t *testing.T) {
	spec := StreamSpec{
		Width: 1920, Height: 1080, FPS: 60,
		BitrateKbps: 8000, H264Profile: "constrained-baseline",
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), `"mic"`) {
		t.Errorf("mic key present when false, want omitted entirely: %s", b)
	}
	if strings.Contains(string(b), `"codec"`) || strings.Contains(string(b), `"abr_floor_kbps"`) {
		t.Errorf("codec/abr_floor_kbps present at their zero values, want omitted: %s", b)
	}
}

// TestStreamSpecMicPresentWhenTrue — the granted state rides the wire when set.
func TestStreamSpecMicPresentWhenTrue(t *testing.T) {
	spec := StreamSpec{
		Width: 1920, Height: 1080, FPS: 60,
		BitrateKbps: 8000, H264Profile: "constrained-baseline",
		Mic: true,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	mic, ok := out["mic"].(bool)
	if !ok || !mic {
		t.Errorf("mic = %v, want true", out["mic"])
	}
}
