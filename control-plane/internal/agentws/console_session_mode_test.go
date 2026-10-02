package agentws

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// #422: an auto-started console launches at the physical display's mode, not
// the app's 1920x1080@60, unless it streams with no configured mode.
func seedConsoleCapsFourK(t *testing.T, h *Handler, hostID string) {
	t.Helper()
	caps := console.EmptyCapabilities()
	caps.Connectors = []string{"DP-4"}
	caps.Outputs = []console.DRMOutput{{
		ID: "card0:DP-4", Card: "card0", Connector: "DP-4", Connected: true,
		ActiveMode: &console.DRMMode{Width: 3840, Height: 2160, RefreshMillihz: 239990},
		Modes: []console.DRMMode{
			{Width: 3840, Height: 2160, RefreshMillihz: 60000, Preferred: true},
			{Width: 3840, Height: 2160, RefreshMillihz: 239990},
		},
	}}
	if err := h.consoleStore.UpsertCapabilities(context.Background(), hostID, caps); err != nil {
		t.Fatalf("seed console capabilities: %v", err)
	}
}

func TestConsoleAutoStartLocalOnlyFollowsPhysicalMode(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)
	seedConsoleCapsFourK(t, h, hostID)

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	if got, want := ev.lastMode, [3]int32{3840, 2160, 60}; got != want {
		t.Fatalf("launch mode = %v, want %v (Automatic follows weston's preferred mode)", got, want)
	}
}

func TestConsoleAutoStartStreamingKeepsAppDefaults(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedHost(t, pool)
	cfg := map[string]any{
		"enabled":               true,
		"auto_start_on_display": true,
		"stream":                true,
		"default_app":           "00000000-0000-0000-0000-0000000000aa",
		"default_user":          "00000000-0000-0000-0000-0000000000bb",
	}
	if err := h.consoleStore.Upsert(context.Background(), hostID, cfg, nil); err != nil {
		t.Fatalf("seed console config: %v", err)
	}
	seedConsoleCapsFourK(t, h, hostID)

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}
	if got, want := ev.lastMode, [3]int32{0, 0, 0}; got != want {
		t.Fatalf("launch mode = %v, want %v (streaming with no configured mode keeps the app defaults)", got, want)
	}
}
