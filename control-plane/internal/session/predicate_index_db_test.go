package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// #414 — a `col::text = $1` predicate defeats the index on `col` (the
// implicit cast is applied per-row, so Postgres cannot use a btree on the
// bare column and falls back to a sequential scan of the whole table). The
// fix is a parameter cast (`col = $1::uuid`) instead, which lets the planner
// use the column's native index.
//
// This test seeds 5000 session rows, ANALYZEs the table so the planner has
// real statistics, then EXPLAINs the exact predicates Get / Transition /
// GetSessionHostState now issue and asserts an Index Scan (or Index Only
// Scan) is chosen — with NO Seq Scan anywhere in the plan. Before the #414
// fix this failed: every one of these predicates planned as a Seq Scan.
func TestSessionPredicatesUseIndexScan(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	ctx := context.Background()

	store := NewStore(pool)
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	seedBulkSessions(t, pool, s, 5000)

	if _, err := pool.Exec(ctx, `ANALYZE sessions`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "Get (id predicate)",
			sql:  selectSessionSQL + ` WHERE id = $1::uuid`,
			args: []any{sess.ID},
		},
		{
			name: "GetSessionHostState (id predicate)",
			sql:  `SELECT host_id::text, state FROM sessions WHERE id = $1::uuid`,
			args: []any{sess.ID},
		},
		{
			name: "Transition lock read (id predicate, FOR UPDATE)",
			sql:  `SELECT state FROM sessions WHERE id = $1::uuid FOR UPDATE`,
			args: []any{sess.ID},
		},
		{
			name: "Transition update (id predicate)",
			sql:  `UPDATE sessions SET state_detail = $2 WHERE id = $1::uuid`,
			args: []any{sess.ID, "probe"},
		},
		{
			name: "ListByUser (user_id predicate)",
			sql:  selectSessionSQL + ` WHERE user_id = $1::uuid ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
			args: []any{s.userID, int32(50), int64(0)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertIndexScan(t, pool, tc.sql, tc.args...)
		})
	}
}

// seedBulkSessions inserts n filler session rows (state 'stopped', spread
// across (user, app, host, gpu) fixtures) so the planner sees a table large
// enough that a Seq Scan is the WRONG choice — on a handful of rows Postgres
// correctly picks a Seq Scan regardless of the predicate shape, which would
// make this regression test pass even with the ::text bug present.
func seedBulkSessions(t *testing.T, pool *pgxpool.Pool, s seedIDs, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps, ended_at)
		SELECT $1::uuid, $2::uuid, $3::uuid, $4::uuid, 'stopped', 1280, 720, 60, 6000, now()
		FROM generate_series(1, $5)
	`, s.userID, s.appID, s.hostID, s.gpuID, n); err != nil {
		t.Fatalf("seed bulk sessions: %v", err)
	}
}

// explainNode is the subset of Postgres's EXPLAIN (FORMAT JSON) plan-node
// shape this test cares about.
type explainNode struct {
	NodeType  string        `json:"Node Type"`
	RelName   string        `json:"Relation Name"`
	IndexName string        `json:"Index Name"`
	Plans     []explainNode `json:"Plans"`
}

type explainRow struct {
	Plan explainNode `json:"Plan"`
}

// assertIndexScan EXPLAINs sql and fails the test unless the plan contains an
// Index Scan / Index Only Scan / Bitmap Index Scan on `sessions` and contains
// NO Seq Scan on `sessions` anywhere in the tree (a Seq Scan on an unrelated,
// tiny fixture table is fine and unavoidable — e.g. none here, but this stays
// relation-scoped rather than blanket-banning "Seq Scan" for future-proofing).
func assertIndexScan(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	var raw []byte
	if err := pool.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var rows []explainRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse explain json: %v (raw=%s)", err, raw)
	}
	if len(rows) == 0 {
		t.Fatalf("explain returned no plan")
	}

	var sawIndexScan, sawSeqScanOnSessions bool
	var walk func(n explainNode)
	walk = func(n explainNode) {
		switch n.NodeType {
		case "Index Scan", "Index Only Scan", "Bitmap Index Scan":
			if n.RelName == "sessions" || n.IndexName != "" {
				sawIndexScan = true
			}
		case "Seq Scan":
			if n.RelName == "sessions" {
				sawSeqScanOnSessions = true
			}
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(rows[0].Plan)

	if sawSeqScanOnSessions {
		t.Fatalf("expected an index scan on sessions, got a Seq Scan (plan=%s)", raw)
	}
	if !sawIndexScan {
		t.Fatalf("expected an Index Scan/Index Only Scan/Bitmap Index Scan on sessions, found none (plan=%s)", raw)
	}
}
