package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"

	"github.com/accreleus/quasar/control-plane/internal/telemetry"
)

// P4-05 integration tests — telemetry pipe + storage. Like the rest of the
// package's DB tests they skip without TEST_DATABASE_URL (provided by go-test-db).

// --- helpers -----------------------------------------------------------------

func countMetrics(t *testing.T, pool *pgxpool.Pool, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_metrics WHERE session_id = $1::uuid`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count metrics: %v", err)
	}
	return n
}

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// --- agent ingestion ---------------------------------------------------------

// TestAgentMetricsInsertHappyPath: an agent's session_metrics for a session on
// THIS host is stored as a source='agent' row with the dictionary keys.
func TestAgentMetricsInsertHappyPath(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sid := runningSession(t, store, s).ID

	coord.AgentMetrics(ctx, s.hostID, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: 1735689600000,
		FPS: f64(59.8), BitrateKbps: f64(14820), EncodeMs: f64(4.6),
		FramesEncoded: i64(299), FramesDropped: i64(1),
	})

	if n := countMetrics(t, pool, sid); n != 1 {
		t.Fatalf("agent metric rows: got %d want 1", n)
	}
	samples, _, err := store.Telemetry().Recent(ctx, sid, 10, nil, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(samples) != 1 || samples[0].Source != telemetry.SourceAgent {
		t.Fatalf("expected one agent sample, got %+v", samples)
	}
	var m map[string]any
	if err := json.Unmarshal(samples[0].Metrics, &m); err != nil {
		t.Fatalf("metrics json: %v", err)
	}
	if m["fps"] == nil || m["encode_ms"] == nil || m["frames_dropped"] == nil {
		t.Fatalf("agent metrics missing dictionary keys: %v", m)
	}
}

// TestAgentMetricsCrossHostRejected: an agent for host B must NOT write host A's
// session metrics (the trust boundary). No row is stored.
func TestAgentMetricsCrossHostRejected(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sess := runningSession(t, store, s)
	sid := sess.ID

	// The session is placed on some host; report from a DIFFERENT host id (any
	// non-owning host triggers the cross-host trust boundary). A random UUID that
	// is not the session's host stands in for "another host's agent".
	otherHost := "00000000-0000-0000-0000-0000000000aa"
	if sess.HostID != nil && *sess.HostID == otherHost {
		otherHost = "00000000-0000-0000-0000-0000000000bb"
	}

	coord.AgentMetrics(ctx, otherHost, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sid, TsUnixMs: 1735689600000, FPS: f64(60),
	})

	if n := countMetrics(t, pool, sid); n != 0 {
		t.Fatalf("cross-host write was stored: got %d rows want 0", n)
	}
}

// TestAgentMetricsNonRunningDropped: a sample for a session that is not running on
// this host is dropped, not stored (agent-api.md).
func TestAgentMetricsNonRunningDropped(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()
	sess, _ := store.ScheduleAndCreate(ctx, launchParams(s)) // assigned, not running

	coord.AgentMetrics(ctx, s.hostID, agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: sess.ID, TsUnixMs: 1, FPS: f64(60),
	})
	if n := countMetrics(t, pool, sess.ID); n != 0 {
		t.Fatalf("non-running sample stored: got %d want 0", n)
	}
}

// --- buildAgentMetrics (pure, no DB) -----------------------------------------

// TestBuildAgentMetricsRenderResolutionUIScale: render_width/render_height/
// ui_scale are copied into the metrics JSONB only when present (session-
// display-update amendment, agent-api.md § session_metrics).
func TestBuildAgentMetricsRenderResolutionUIScale(t *testing.T) {
	// nil fields => keys absent.
	out := buildAgentMetrics(agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: "s", TsUnixMs: 1, FPS: f64(60),
	})
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("metrics json: %v", err)
	}
	if _, ok := m["render_width"]; ok {
		t.Fatalf("render_width present when nil: %v", m)
	}
	if _, ok := m["render_height"]; ok {
		t.Fatalf("render_height present when nil: %v", m)
	}
	if _, ok := m["ui_scale"]; ok {
		t.Fatalf("ui_scale present when nil: %v", m)
	}

	// set fields => keys present with the values.
	out = buildAgentMetrics(agentws.SessionMetricsMsg{
		Type: "session_metrics", SessionID: "s", TsUnixMs: 1,
		RenderWidth: i32(1280), RenderHeight: i32(720), UIScale: f64(1.5),
	})
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("metrics json: %v", err)
	}
	if got, ok := m["render_width"].(float64); !ok || got != 1280 {
		t.Fatalf("render_width: got %v want 1280", m["render_width"])
	}
	if got, ok := m["render_height"].(float64); !ok || got != 720 {
		t.Fatalf("render_height: got %v want 720", m["render_height"])
	}
	if got, ok := m["ui_scale"].(float64); !ok || got != 1.5 {
		t.Fatalf("ui_scale: got %v want 1.5", m["ui_scale"])
	}
}

// --- store: browser insert + filtering + read ordering -----------------------

// --- retention ---------------------------------------------------------------
