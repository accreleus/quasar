package console

import (
	"encoding/json"
	"testing"
)

func TestCapabilitiesPreserveActiveDRMMode(t *testing.T) {
	var caps Capabilities
	if err := json.Unmarshal([]byte(`{
		"connectors":["DP-4"],"audio_sinks":[],"input_devices":[],
		"outputs":[{"id":"card0:DP-4","card":"card0","render_node":"/dev/dri/renderD128",
		"connector":"DP-4","connected":true,
		"active_mode":{"name":"2560x1440","width":2560,"height":1440,"refresh_millihz":119997,
		"preferred":false,"interlaced":false,"clock_khz":497750,"htotal":2720,"vtotal":1525},
		"modes":[]}]
	}`), &caps); err != nil {
		t.Fatal(err)
	}
	if got := caps.Outputs[0].ActiveMode; got == nil || got.Width != 2560 || got.Height != 1440 || got.RefreshMillihz != 119997 {
		t.Fatalf("active mode lost during capability decode: %#v", got)
	}

	raw, err := json.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip map[string]any
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	outputs := roundTrip["outputs"].([]any)
	if outputs[0].(map[string]any)["active_mode"] == nil {
		t.Fatal("active_mode missing from capability response")
	}
}

// CM-09 item 3: PinnedConnector derives the connector to key the level-trigger
// presence check on from output_id — connector itself stays locked to "auto".
func TestConsoleConfigPinnedConnector(t *testing.T) {
	auto := ConsoleConfig{Connector: "auto"}
	if got := auto.PinnedConnector(); got != "auto" {
		t.Fatalf("unset output_id: PinnedConnector() = %q, want auto", got)
	}

	id := "card0:DP-4"
	pinned := ConsoleConfig{Connector: "auto", OutputID: &id}
	if got := pinned.PinnedConnector(); got != "DP-4" {
		t.Fatalf("output_id=%q: PinnedConnector() = %q, want DP-4", id, got)
	}

	multiCard := "card1:HDMI-A-1"
	pinnedMulti := ConsoleConfig{Connector: "auto", OutputID: &multiCard}
	if got := pinnedMulti.PinnedConnector(); got != "HDMI-A-1" {
		t.Fatalf("output_id=%q: PinnedConnector() = %q, want HDMI-A-1", multiCard, got)
	}

	// Review round (LOW fix): a malformed output_id (no ":") falls back to
	// "auto" defensively, but is not silent about it — PinnedConnector warns
	// via slog rather than disabling the pin with no signal. Not asserting on
	// the log line itself here (no logger injection point on this value
	// type); the fallback behavior is what's under test.
	malformed := "not-card-scoped"
	pinnedMalformed := ConsoleConfig{Connector: "auto", OutputID: &malformed}
	if got := pinnedMalformed.PinnedConnector(); got != "auto" {
		t.Fatalf("output_id=%q (malformed): PinnedConnector() = %q, want auto", malformed, got)
	}
}
