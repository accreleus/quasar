package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// The two properties this package is responsible for now that retention moved
// into internal/telemetry: the ingest paths must not delete anything, and a
// terminal transition must FREEZE a session's telemetry rather than wipe it.

// TestTerminalTransitionKeepsTelemetryForThePostMortem is the inversion of the
// old TestTerminalPruneRemovesMetrics. A session reaching a terminal state used
// to have its telemetry deleted on the spot, which meant `make session-verdict`
// on a session that had just failed returned nothing. It must now survive.
func TestTerminalTransitionKeepsTelemetryForThePostMortem(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sid := runningSession(t, store, s).ID

	tel := store.Telemetry()
	_ = tel.Append(ctx, sid, telemetry.SourceAgent, telemetry.SampleInput{TsUnixMs: 1, Metrics: json.RawMessage(`{"fps":60}`)})
	_ = tel.Append(ctx, sid, telemetry.SourceBrowser, telemetry.SampleInput{TsUnixMs: 2, Metrics: json.RawMessage(`{"fps":60}`)})
	_ = tel.AppendEvent(ctx, sid, telemetry.SourceAgent, telemetry.EventInput{TsUnixMs: 3, Type: "abr.retarget"})
	_ = tel.UpsertClock(ctx, sid, 5, 1)

	// Agent reports the session stopped.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sid, State: "stopped"})

	// Nothing in the terminal path deletes telemetry, so give the old async prune
	// every chance to have fired before asserting it did not.
	time.Sleep(200 * time.Millisecond)

	if n := countMetrics(t, pool, sid); n != 2 {
		t.Fatalf("terminal transition deleted samples (%d left, want 2) — post-mortem retention is broken", n)
	}
	if n := countTraceEvents(t, pool, sid); n != 1 {
		t.Fatalf("terminal transition deleted trace events (%d left, want 1)", n)
	}
	c, err := tel.Clock(ctx, sid)
	if err != nil || c == nil {
		t.Fatalf("terminal transition deleted the clock row: (%+v, %v)", c, err)
	}

	// And the verdict read — the thing an operator actually runs the next morning
	// — still has data to answer from.
	sl, err := tel.Window(ctx, sid, telemetry.Range{}, telemetry.Filter{Limit: 1000})
	if err != nil {
		t.Fatalf("window read on a stopped session: %v", err)
	}
	if len(sl.Samples) != 2 || len(sl.Events) != 1 {
		t.Fatalf("stopped session is not diagnosable: %d samples, %d events", len(sl.Samples), len(sl.Events))
	}
}

// TestIngestPathsDoNotPrune keeps the four inline DELETEs from growing back.
//
// A behavioural test cannot state this cleanly (the absence of a delete looks
// like a delete that found nothing), so this reads the package's own source. The
// rule it enforces is narrow and durable: NOTHING in internal/session deletes
// telemetry. The janitor in internal/telemetry is the only deleter.
func TestIngestPathsDoNotPrune(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// Any DELETE against one of the three telemetry tables, and any leftover
	// prune-shaped helper name.
	deleteRe := regexp.MustCompile(`(?is)DELETE\s+FROM\s+(session_metrics|session_trace_events|session_trace_clock)`)
	pruneRe := regexp.MustCompile(`Prune(SessionMetrics|SessionTraceEvents|TerminalSession|TerminalSessionTrace)\b`)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if m := deleteRe.FindString(string(src)); m != "" {
			t.Errorf("%s: %q — telemetry retention belongs to internal/telemetry's janitor, "+
				"not to an ingest or lifecycle path in this package", f, m)
		}
		if m := pruneRe.FindString(string(src)); m != "" {
			t.Errorf("%s: %q — the per-session prune helpers were replaced by telemetry.Store.Retain", f, m)
		}
	}
}

// TestTerminalStatesMatchTelemetryPolicy pins the two places that have to agree
// on what "terminal" means: State.IsTerminal here, and the SQL predicate the
// telemetry sweep uses. They are in different packages by design, so this is the
// only thing holding them in step.
func TestTerminalStatesMatchSessionPackage(t *testing.T) {
	all := []State{StatePending, StateAssigned, StateStarting, StateRunning, StateStopping, StateStopped, StateFailed}
	var terminal []string
	for _, s := range all {
		if s.IsTerminal() {
			terminal = append(terminal, string(s))
		}
	}
	got := strings.Join(terminal, ",")
	if got != "stopped,failed" {
		t.Fatalf("terminal states are now %q; internal/telemetry's sweep predicate "+
			"(terminalStates in postgres.go) says ('stopped','failed') and must be updated with it", got)
	}
}
