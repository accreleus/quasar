package agentws

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// CM-09 item 3: the level-trigger presence check must key on the connector
// pinned via output_id (not the validation-locked `connector` field, which
// stays "auto"). seedEligibleConsolePinnedHost mirrors
// seedEligibleConsoleHost (console_selfheal_test.go) with an output_id/mode
// pin added.
func seedEligibleConsolePinnedHost(t *testing.T, h *Handler, pool *pgxpool.Pool, outputID string) string {
	t.Helper()
	hostID := seedHost(t, pool)
	cfg := map[string]any{
		"enabled":               true,
		"auto_start_on_display": true,
		"default_app":           "00000000-0000-0000-0000-0000000000aa",
		"default_user":          "00000000-0000-0000-0000-0000000000bb",
		"output_id":             outputID,
		"mode":                  map[string]any{"width": 1920, "height": 1080, "refresh_millihz": 60000},
	}
	if err := h.consoleStore.Upsert(context.Background(), hostID, cfg, nil); err != nil {
		t.Fatalf("seed pinned console config: %v", err)
	}
	return hostID
}

// A pinned connector present in the reported list launches, exactly like the
// "auto" behavior when any connector is present.
func TestConsoleAutoStartLaunchesWhenPinnedConnectorPresent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3", "DP-4"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1 (pinned connector DP-4 present)", got)
	}
}

// A pinned connector NOT in the reported list must not launch — no silent
// fallback to a different connected monitor (fail-loud-by-omission, spec
// item 3 design point 2), even though other connectors are present.
func TestConsoleAutoStartSkipsWhenPinnedConnectorAbsent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3", "HDMI-A-1"})

	if got := ev.count(); got != 0 {
		t.Fatalf("launch count = %d, want 0 (pinned connector DP-4 absent)", got)
	}
	if _, tracked := h.consoleAuto.sessions[hostID]; tracked {
		t.Fatal("no session should be tracked when the pinned connector never launched")
	}
}

// A running pinned-connector session must auto-stop the instant its specific
// connector drops out of the reported list, even while other connectors
// remain present — the auto-stop keys off the same pin as auto-start.
func TestConsoleAutoStopWhenPinnedConnectorGoesAbsent(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsolePinnedHost(t, h, pool, "card0:DP-4")

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-4"})
	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1", got)
	}

	stopEv := &teardownEvents{active: true}
	h.events = stopEv
	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3"})

	if len(stopEv.stopped) != 1 {
		t.Fatalf("stopped sessions = %v, want exactly one stop", stopEv.stopped)
	}
	if stopEv.stopReasons[0] != "console_display_disconnected" {
		t.Fatalf("stop reason = %q, want console_display_disconnected", stopEv.stopReasons[0])
	}
}

// Unset output_id keeps today's "auto" behavior: any reported connector is
// enough to launch.
func TestConsoleAutoStartUnpinnedKeepsAutoBehavior(t *testing.T) {
	h, pool, ev := selfHealHandler(t)
	hostID := seedEligibleConsoleHost(t, h, pool)

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"HDMI-A-1"})

	if got := ev.count(); got != 1 {
		t.Fatalf("launch count = %d, want 1 (unpinned auto behavior)", got)
	}
}

// connectorPresent unit coverage (pure function, no DB): pinned match/no-match
// and the auto passthrough.
func TestConnectorPresentPinned(t *testing.T) {
	cases := []struct {
		name       string
		connectors []string
		configured string
		want       bool
	}{
		{"auto with connectors", []string{"DP-4"}, "auto", true},
		{"auto with none", nil, "auto", false},
		{"pinned present", []string{"DP-3", "DP-4"}, "DP-4", true},
		{"pinned absent", []string{"DP-3", "HDMI-A-1"}, "DP-4", false},
		{"pinned empty list", nil, "DP-4", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := connectorPresent(tc.connectors, tc.configured); got != tc.want {
				t.Fatalf("connectorPresent(%v, %q) = %v, want %v", tc.connectors, tc.configured, got, tc.want)
			}
		})
	}
}

// console.ConsoleConfig.PinnedConnector plumbing sanity: Resolve() locks
// `connector` to "auto" but preserves output_id, and PinnedConnector derives
// the connector from it.
func TestResolvedConfigPinnedConnectorFromOutputID(t *testing.T) {
	cfg, err := console.Resolve(map[string]any{"output_id": "card0:DP-4"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Connector != "auto" {
		t.Fatalf("Connector = %q, want auto (validation-locked)", cfg.Connector)
	}
	if got := cfg.PinnedConnector(); got != "DP-4" {
		t.Fatalf("PinnedConnector() = %q, want DP-4", got)
	}

	unpinned, err := console.Resolve(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if got := unpinned.PinnedConnector(); got != "auto" {
		t.Fatalf("PinnedConnector() with no output_id = %q, want auto", got)
	}
}
