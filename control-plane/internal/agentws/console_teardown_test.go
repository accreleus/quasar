package agentws

import (
	"context"
	"log/slog"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// teardownEvents records StopConsoleSession calls and reports a configurable
// liveness for the tracked session.
type teardownEvents struct {
	noopEvents
	active      bool
	stopped     []string
	stopReasons []string
}

func (e *teardownEvents) StopConsoleSession(_ context.Context, sessionID, reason string) error {
	e.stopped = append(e.stopped, sessionID)
	e.stopReasons = append(e.stopReasons, reason)
	return nil
}

func (e *teardownEvents) ConsoleSessionActive(context.Context, string) bool { return e.active }

func teardownHandler(t *testing.T, ev Events) *Handler {
	t.Helper()
	pool := testPool(t)
	return &Handler{
		log:          slog.Default(),
		events:       ev,
		consoleStore: console.NewStore(pool),
		consoleAuto:  newConsoleAutoState(),
	}
}

// Disabling console mode while an auto-started console session is running must
// stop that session (Tower console gap, 2026-07-14). A host with no
// console_configs row resolves to the defaults (enabled=false), which is
// exactly the post-disable state handleConsoleAutoStart re-evaluates when the
// agent re-sends capacity after the config_update push.
func TestConsoleAutoStartStopsTrackedSessionWhenDisabled(t *testing.T) {
	ev := &teardownEvents{active: true}
	h := teardownHandler(t, ev)
	hostID := "00000000-0000-0000-0000-000000000001"
	h.consoleAuto.sessions[hostID] = "sess-console-1"

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3"})

	if len(ev.stopped) != 1 || ev.stopped[0] != "sess-console-1" {
		t.Fatalf("stopped sessions = %v, want [sess-console-1]", ev.stopped)
	}
	if ev.stopReasons[0] != "console_disabled" {
		t.Fatalf("stop reason = %q, want console_disabled", ev.stopReasons[0])
	}
	if _, tracked := h.consoleAuto.sessions[hostID]; tracked {
		t.Fatal("tracker entry not cleared after teardown-on-disable")
	}
}

// A tracked session that already terminated on its own is cleared as stale
// first — the disable path must not issue a spurious stop for it.
func TestConsoleAutoStartDisabledSkipsStopForDeadSession(t *testing.T) {
	ev := &teardownEvents{active: false}
	h := teardownHandler(t, ev)
	hostID := "00000000-0000-0000-0000-000000000002"
	h.consoleAuto.sessions[hostID] = "sess-console-2"

	h.handleConsoleAutoStart(context.Background(), hostID, []string{"DP-3"})

	if len(ev.stopped) != 0 {
		t.Fatalf("stopped sessions = %v, want none (session already terminal)", ev.stopped)
	}
	if _, tracked := h.consoleAuto.sessions[hostID]; tracked {
		t.Fatal("stale tracker entry not cleared")
	}
}
