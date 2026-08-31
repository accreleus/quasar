package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// Integration tests for the Postgres adapter. Like the rest of the repo's DB
// tests they skip without TEST_DATABASE_URL (provided by `make test-db` /
// `scripts/dev/dev.sh go-test-db`).

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		DELETE FROM session_trace_clock;
		DELETE FROM session_trace_events;
		DELETE FROM session_metrics;
		DELETE FROM sessions;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedSession inserts a session in the given state and returns its id. endedAt
// may be zero for a live session.
func seedSession(t *testing.T, pool *pgxpool.Pool, state string, endedAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	var userID, appID string
	// Reuse a user/app if the shared test DB already has one — this package
	// never truncates users/apps, because other packages' fixtures live there.
	err := pool.QueryRow(ctx, `SELECT id::text FROM users LIMIT 1`).Scan(&userID)
	if err != nil {
		if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
			VALUES ($1, $2, 'x') RETURNING id::text`,
			fmt.Sprintf("telemetry-%d@test.local", time.Now().UnixNano()),
			fmt.Sprintf("telemetry-%d", time.Now().UnixNano())).Scan(&userID); err != nil {
			t.Fatalf("user: %v", err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM apps LIMIT 1`).Scan(&appID); err != nil {
		if err := pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('telemetry-test') RETURNING id::text`).
			Scan(&appID); err != nil {
			t.Fatalf("app: %v", err)
		}
	}
	var sid string
	var ended any
	if !endedAt.IsZero() {
		ended = endedAt
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps, ended_at, updated_at)
		VALUES ($1, $2, $3, 1280, 720, 60, 6000, $4, COALESCE($4, now()))
		RETURNING id::text`, userID, appID, state, ended).Scan(&sid); err != nil {
		t.Fatalf("session: %v", err)
	}
	return sid
}

// backdate rewrites the server-side ingestion clock for a session's telemetry,
// so a retention test does not have to sleep for an hour.
func backdate(t *testing.T, pool *pgxpool.Pool, sessionID string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		`UPDATE session_metrics SET created_at = $2 WHERE session_id = $1::uuid`,
		`UPDATE session_trace_events SET created_at = $2 WHERE session_id = $1::uuid`,
	} {
		if _, err := pool.Exec(ctx, q, sessionID, at); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM `+table+` WHERE session_id = $1::uuid`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// --- round-trips ---------------------------------------------------------------

func TestPostgresAppendAndWindow(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})

	if err := s.AppendBatch(ctx, sid, SourceAgent, []SampleInput{
		{TsUnixMs: 1000, Metrics: json.RawMessage(`{"fps":60}`)},
		{TsUnixMs: 2000},
	}); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	if err := s.AppendEvents(ctx, sid, SourceAgent, []EventInput{
		{TsUnixMs: 1500, Type: "abr.retarget", Payload: json.RawMessage(`{"to_kbps":11000}`)},
		{TsUnixMs: 2500, Type: "encoder.drop_detected"},
	}); err != nil {
		t.Fatalf("append events: %v", err)
	}

	sl, err := s.Window(ctx, sid, Range{}, Filter{})
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if len(sl.Samples) != 2 || sl.Samples[0].TsUnixMs != 2000 {
		t.Fatalf("samples not newest-first: %+v", sl.Samples)
	}
	if string(sl.Samples[0].Metrics) != "{}" {
		t.Fatalf("a nil metrics must default to {}, got %s", sl.Samples[0].Metrics)
	}
	if len(sl.Events) != 2 || sl.Events[0].Type != "encoder.drop_detected" {
		t.Fatalf("events not newest-first: %+v", sl.Events)
	}
	if sl.Clock != nil {
		t.Fatal("no clock row was written; the slice must report unmeasured")
	}

	// Bounded window + type filter.
	sl, err = s.Window(ctx, sid, Range{FromMs: 1200, ToMs: 2400}, Filter{Types: []string{"abr.retarget"}})
	if err != nil {
		t.Fatalf("bounded window: %v", err)
	}
	if len(sl.Samples) != 1 || sl.Samples[0].TsUnixMs != 2000 {
		t.Fatalf("bounded samples: %+v", sl.Samples)
	}
	if len(sl.Events) != 1 || sl.Events[0].Type != "abr.retarget" {
		t.Fatalf("typed events: %+v", sl.Events)
	}
}

func TestPostgresRecentPaginatesAndFiltersSource(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})

	for i := 0; i < 5; i++ {
		if err := s.Append(ctx, sid, SourceAgent, SampleInput{TsUnixMs: int64(1000 + i)}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := s.Append(ctx, sid, SourceBrowser, SampleInput{TsUnixMs: 9999}); err != nil {
		t.Fatalf("append browser: %v", err)
	}

	got, next, err := s.Recent(ctx, sid, 3, nil, "")
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 || got[0].TsUnixMs != 9999 || next != "3" {
		t.Fatalf("page 1: %+v next=%q", got, next)
	}
	agent := SourceAgent
	got, _, err = s.Recent(ctx, sid, 100, &agent, "")
	if err != nil {
		t.Fatalf("recent agent: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("source filter: got %d agent samples, want 5", len(got))
	}
}

func TestPostgresClockUpsertAndUnmeasured(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})

	c, err := s.Clock(ctx, sid)
	if err != nil || c != nil {
		t.Fatalf("unmeasured must be (nil, nil): (%+v, %v)", c, err)
	}
	if err := s.UpsertClock(ctx, sid, -12.5, 3); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertClock(ctx, sid, -11.0, 2); err != nil {
		t.Fatalf("refine: %v", err)
	}
	c, err = s.Clock(ctx, sid)
	if err != nil || c == nil || c.ClientOffsetMs != -11.0 || c.UncertaintyMs != 2 {
		t.Fatalf("refined clock: (%+v, %v)", c, err)
	}
}

func TestPostgresCaptureReads(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})
	other := seedSession(t, pool, "running", time.Time{})

	if err := s.AppendEvent(ctx, sid, SourceAgent, EventInput{
		TsUnixMs: 1, Type: "diag.pipeline_graph",
		Payload: json.RawMessage(`{"capture_id":"cap-1","graph":"..."}`),
	}); err != nil {
		t.Fatalf("append capture: %v", err)
	}

	if _, err := s.Capture(ctx, sid, "missing"); err != ErrNotFound {
		t.Fatalf("an unlanded capture must be ErrNotFound, got %v", err)
	}
	// A capture id from another session is a 404, not a cross-session read.
	if _, err := s.Capture(ctx, other, "cap-1"); err != ErrNotFound {
		t.Fatalf("cross-session capture read must be ErrNotFound, got %v", err)
	}
	e, err := s.Capture(ctx, sid, "cap-1")
	if err != nil || e.Type != "diag.pipeline_graph" {
		t.Fatalf("capture: (%+v, %v)", e, err)
	}
	all, err := s.Captures(ctx, sid)
	if err != nil || len(all) != 1 {
		t.Fatalf("captures: (%d, %v)", len(all), err)
	}
}

func TestPostgresLatestPerSessionIsSourceScoped(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	a := seedSession(t, pool, "running", time.Time{})
	b := seedSession(t, pool, "running", time.Time{})

	_ = s.Append(ctx, a, SourceAgent, SampleInput{TsUnixMs: 10, Metrics: json.RawMessage(`{"frames_dropped":1}`)})
	_ = s.Append(ctx, a, SourceBrowser, SampleInput{TsUnixMs: 20, Metrics: json.RawMessage(`{"frames_dropped":9}`)})

	got, err := s.LatestPerSession(ctx, []string{a, b})
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, ok := got[b]; ok {
		t.Fatal("a session with no telemetry must be absent from the map")
	}
	l := got[a]
	if l.Agent == nil || l.Browser == nil {
		t.Fatalf("both sources must survive: %+v", l)
	}
	// JSONB round-trips with its own whitespace, so compare the decoded value.
	var agentMetrics struct {
		FramesDropped int `json:"frames_dropped"`
	}
	if err := json.Unmarshal(l.Agent.Metrics, &agentMetrics); err != nil {
		t.Fatalf("decode agent metrics: %v", err)
	}
	if agentMetrics.FramesDropped != 1 {
		t.Fatalf("the newer browser sample overlaid the agent's: %s", l.Agent.Metrics)
	}
}

// --- retention -------------------------------------------------------------------

// The rolling trim keeps a running session inside its window and leaves
// everything fresher alone.
func TestRetainRollingTrimBoundsALiveSession(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})

	_ = s.Append(ctx, sid, SourceAgent, SampleInput{TsUnixMs: 1})
	_ = s.AppendEvent(ctx, sid, SourceAgent, EventInput{TsUnixMs: 1, Type: "abr.retarget"})
	_ = s.AppendEvent(ctx, sid, SourceAgent, EventInput{TsUnixMs: 2, Type: "diag.pipeline_graph"})
	backdate(t, pool, sid, time.Now().Add(-3*time.Hour))
	// A fresh sample, written after the backdate, must survive.
	_ = s.Append(ctx, sid, SourceAgent, SampleInput{TsUnixMs: 3})

	rep, err := s.Retain(ctx, DefaultPolicy())
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if got := countRows(t, pool, "session_metrics", sid); got != 1 {
		t.Fatalf("live session metrics after trim: got %d, want 1 (the fresh one)", got)
	}
	if got := countRows(t, pool, "session_trace_events", sid); got != 1 {
		t.Fatalf("live session events after trim: got %d, want 1 (the capture)", got)
	}
	if rep.RollingSamples != 1 || rep.RollingEvents != 1 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.PostMortemSamples != 0 || rep.PostMortemEvents != 0 {
		t.Fatalf("a live session must never be post-mortem swept: %+v", rep)
	}
}

// The change this whole module exists for: a session that stopped hours ago
// still has its last hour, and it is swept only once the post-mortem expires.
func TestRetainKeepsATerminalSessionForThePostMortemThenSweepsIt(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()

	recent := seedSession(t, pool, "failed", time.Now().Add(-8*time.Hour))
	_ = s.Append(ctx, recent, SourceAgent, SampleInput{TsUnixMs: 1})
	_ = s.AppendEvent(ctx, recent, SourceAgent, EventInput{TsUnixMs: 1, Type: "abr.retarget"})
	_ = s.UpsertClock(ctx, recent, 5, 1)
	backdate(t, pool, recent, time.Now().Add(-9*time.Hour))

	old := seedSession(t, pool, "stopped", time.Now().Add(-30*time.Hour))
	_ = s.Append(ctx, old, SourceAgent, SampleInput{TsUnixMs: 1})
	_ = s.AppendEvent(ctx, old, SourceAgent, EventInput{TsUnixMs: 1, Type: "abr.retarget"})
	_ = s.AppendEvent(ctx, old, SourceAgent, EventInput{TsUnixMs: 2, Type: "diag.encoder_state"})
	_ = s.UpsertClock(ctx, old, 5, 1)
	backdate(t, pool, old, time.Now().Add(-31*time.Hour))

	rep, err := s.Retain(ctx, DefaultPolicy())
	if err != nil {
		t.Fatalf("retain: %v", err)
	}

	// Diagnosable the next morning.
	if got := countRows(t, pool, "session_metrics", recent); got != 1 {
		t.Fatalf("a session that stopped 8 h ago lost its samples (%d) — post-mortem retention is broken", got)
	}
	if got := countRows(t, pool, "session_trace_events", recent); got != 1 {
		t.Fatalf("a session that stopped 8 h ago lost its events (%d)", got)
	}
	if got := countRows(t, pool, "session_trace_clock", recent); got != 1 {
		t.Fatalf("a session that stopped 8 h ago lost its clock row (%d)", got)
	}

	// Past the post-mortem: swept, captures excepted.
	if got := countRows(t, pool, "session_metrics", old); got != 0 {
		t.Fatalf("expired session still has %d samples", got)
	}
	if got := countRows(t, pool, "session_trace_clock", old); got != 0 {
		t.Fatalf("expired session still has its clock row — that leak is the bug this fixes")
	}
	if got := countRows(t, pool, "session_trace_events", old); got != 1 {
		t.Fatalf("expired session events: got %d, want 1 (the capture is exempt)", got)
	}
	if rep.PostMortemSamples != 1 || rep.PostMortemEvents != 1 || rep.PostMortemClocks != 1 {
		t.Fatalf("post-mortem report: %+v", rep)
	}
}

// The janitor deletes in bounded chunks and loops, so a backlog larger than one
// batch is still fully drained in one pass.
func TestRetainDrainsABacklogInBatches(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	sid := seedSession(t, pool, "running", time.Time{})

	const n = 25
	batch := make([]SampleInput, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, SampleInput{TsUnixMs: int64(i)})
	}
	if err := s.AppendBatch(ctx, sid, SourceAgent, batch); err != nil {
		t.Fatalf("append batch: %v", err)
	}
	backdate(t, pool, sid, time.Now().Add(-2*time.Hour))

	p := DefaultPolicy()
	p.Batch = 4 // five-plus statements to clear 25 rows
	rep, err := s.Retain(ctx, p)
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if rep.RollingSamples != n {
		t.Fatalf("batched drain deleted %d of %d", rep.RollingSamples, n)
	}
	if got := countRows(t, pool, "session_metrics", sid); got != 0 {
		t.Fatalf("%d rows survived a batched drain", got)
	}
	if rep.Truncated {
		t.Fatal("a 25-row backlog must not report truncated")
	}
}

// A pending/starting/stopping session is not terminal, so it is trimmed on the
// rolling rule — not frozen.
func TestRetainTreatsNonTerminalStatesAsLive(t *testing.T) {
	pool := testDB(t)
	s := Postgres(pool)
	ctx := context.Background()
	for _, state := range []string{"pending", "assigned", "starting", "running", "stopping"} {
		sid := seedSession(t, pool, state, time.Time{})
		_ = s.Append(ctx, sid, SourceAgent, SampleInput{TsUnixMs: 1})
		backdate(t, pool, sid, time.Now().Add(-2*time.Hour))
		if _, err := s.Retain(ctx, DefaultPolicy()); err != nil {
			t.Fatalf("retain %s: %v", state, err)
		}
		if got := countRows(t, pool, "session_metrics", sid); got != 0 {
			t.Fatalf("state %q: aged sample survived the rolling trim", state)
		}
	}
}
