package agentws

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// #422: an auto-started console launches at the physical display's mode, not
// the app's 1920x1080@60.
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
		t.Fatalf("launch mode = %v, want %v (Automatic follows the preferred mode)", got, want)
	}
}

// Amendment 19: a default app that does not declare direct_display fails the
// console_default_app readiness check and is never launched, and that is not a
// launch failure either: nothing is tracked and no backoff is armed.
func TestConsoleAutoStartSkipsAppThatCannotRunDirect(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedHost(t, pool)
	cfg := map[string]any{
		"enabled":               true,
		"auto_start_on_display": true,
		"default_app":           seedConsoleApp(t, pool, false),
		"default_user":          "00000000-0000-0000-0000-0000000000bb",
	}
	if err := h.consoleStore.Upsert(context.Background(), hostID, cfg, nil); err != nil {
		t.Fatalf("seed console config: %v", err)
	}
	seedConsoleCapsFourK(t, h, hostID)

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})

	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 (the default app cannot run direct)", got)
	}
	h.consoleAuto.mu.Lock()
	_, tracked := h.consoleAuto.sessions[hostID]
	bo, backedOff := h.consoleAuto.backoff[hostID]
	h.consoleAuto.mu.Unlock()
	if tracked {
		t.Fatal("no session may be tracked for an app that cannot run direct")
	}
	if backedOff && bo.consecutiveFailures != 0 {
		t.Fatalf("a readiness refusal counted as %d launch failure(s)", bo.consecutiveFailures)
	}
}
