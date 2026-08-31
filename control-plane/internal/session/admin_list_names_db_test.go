package session

// #385 item 7 — Store.ListAll resolves the display names of the user / app / host
// a session references, so the admin oversight table can stop rendering truncated
// uuids. DB integration tests: skipped without TEST_DATABASE_URL (go-test-db).
//
// What is load-bearing here, and why each case exists:
//   - the names come back at all, on the right rows (the join is a LEFT JOIN
//     bolted onto a subquery, so a wrong ON clause would silently smear names
//     across rows rather than fail);
//   - a MISSING referent yields a nil name instead of dropping the row — that is
//     the entire reason the join is LEFT and not inner;
//   - newest-first ordering survives the join (a join does not preserve a
//     subquery's ORDER BY, so the outer ORDER BY is load-bearing, not decorative);
//   - the session columns themselves still scan correctly now that
//     scanSessionRow takes trailing variadic destinations.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertSessionRow inserts a session directly (bypassing the scheduler) so a test
// can pin exactly which references it has.
func insertSessionRow(t *testing.T, pool *pgxpool.Pool, userID, appID string, hostID *string, state string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO sessions
			(user_id, app_id, host_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 1280, 720, 60, 6000)
		RETURNING id::text
	`, userID, appID, hostID, state).Scan(&id))
	return id
}

func mustName(t *testing.T, p *string, what string) string {
	t.Helper()
	if p == nil {
		t.Fatalf("%s: got nil, want a name", what)
	}
	return *p
}

// TestAdminListResolvesDisplayNames is the happy path: every referent exists, so
// every name resolves, and it resolves to the right row.
func TestAdminListResolvesDisplayNames(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4) // user 'u', app 'app', host 'host-1'
	ctx := context.Background()

	insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "running")

	rows, _, err := store.ListAll(ctx, "", 50, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAll returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	if got := mustName(t, r.Username, "username"); got != "u" {
		t.Errorf("username = %q, want %q", got, "u")
	}
	if got := mustName(t, r.AppName, "app_name"); got != "app" {
		t.Errorf("app_name = %q, want %q", got, "app")
	}
	if got := mustName(t, r.HostName, "host_name"); got != "host-1" {
		t.Errorf("host_name = %q, want %q", got, "host-1")
	}
	// The embedded Session must still scan correctly — scanSessionRow now appends
	// variadic destinations after the shared positional column list, and a
	// one-column slip there would corrupt these rather than error.
	if r.UserID != s.userID || r.AppID != s.appID {
		t.Errorf("session ids mis-scanned: user=%s app=%s want user=%s app=%s",
			r.UserID, r.AppID, s.userID, s.appID)
	}
	if r.State != StateRunning {
		t.Errorf("state = %q, want running", r.State)
	}
	if r.Width != 1280 || r.Height != 720 || r.FPS != 60 {
		t.Errorf("stream mis-scanned: %dx%d@%d", r.Width, r.Height, r.FPS)
	}
}

// TestAdminListUnassignedSessionHasNoHostName covers the routinely-absent name:
// a session that has not been placed yet has host_id NULL, so host_name must be
// nil while the other two still resolve. This is the common case, not an edge.
func TestAdminListUnassignedSessionHasNoHostName(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	insertSessionRow(t, pool, s.userID, s.appID, nil, "pending")

	rows, _, err := store.ListAll(ctx, "", 50, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAll returned %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.HostID != nil {
		t.Fatalf("fixture is wrong: host_id = %v, want nil", *r.HostID)
	}
	if r.HostName != nil {
		t.Errorf("host_name = %q, want nil for an unassigned session", *r.HostName)
	}
	if got := mustName(t, r.Username, "username"); got != "u" {
		t.Errorf("username = %q, want %q", got, "u")
	}
	if got := mustName(t, r.AppName, "app_name"); got != "app" {
		t.Errorf("app_name = %q, want %q", got, "app")
	}
}

// TestAdminListSessionWithDeletedAppStillLists is the reason the join is LEFT.
//
// Constructing the state takes a deliberate step, and that is itself worth
// recording: `sessions.app_id` is ON DELETE CASCADE (migration 0014), so deleting
// an app through the normal path takes its sessions with it — a dangling app_id
// is NOT reachable today. The LEFT JOIN is what keeps that a property of the
// current schema rather than a load-bearing assumption of the admin read path: if
// 0014's cascade is ever softened to SET NULL, or a row is orphaned by a restore
// or a manual repair, the operator's oversight view must degrade to "unknown app",
// never to "session silently absent". So the test drops the constraint for its
// duration to build exactly the row an inner join would have swallowed.
func TestAdminListSessionWithDeletedAppStillLists(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sid := insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "stopped")

	if _, err := pool.Exec(ctx, `ALTER TABLE sessions DROP CONSTRAINT sessions_app_id_fkey`); err != nil {
		t.Fatalf("drop app fk: %v", err)
	}
	// Restore the schema no matter how this test exits. The orphan row has to go
	// first or re-adding the constraint would fail validation, and the shared
	// -p 1 database would be left without the FK for every later test.
	t.Cleanup(func() {
		bg := context.Background()
		if _, err := pool.Exec(bg, `DELETE FROM sessions WHERE id::text = $1`, sid); err != nil {
			t.Errorf("cleanup: delete orphan session: %v", err)
		}
		if _, err := pool.Exec(bg, `ALTER TABLE sessions
			ADD CONSTRAINT sessions_app_id_fkey
			FOREIGN KEY (app_id) REFERENCES apps(id) ON DELETE CASCADE`); err != nil {
			t.Errorf("cleanup: restore app fk: %v", err)
		}
	})

	if _, err := pool.Exec(ctx, `DELETE FROM apps WHERE id::text = $1`, s.appID); err != nil {
		t.Fatalf("delete app: %v", err)
	}

	rows, _, err := store.ListAll(ctx, "", 50, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListAll returned %d rows, want 1 — the session must survive its app", len(rows))
	}
	r := rows[0]
	if r.ID != sid {
		t.Fatalf("wrong row: %s want %s", r.ID, sid)
	}
	if r.AppName != nil {
		t.Errorf("app_name = %q, want nil for a deleted app", *r.AppName)
	}
	// app_id is still on the row, so the client has something to fall back to.
	if r.AppID != s.appID {
		t.Errorf("app_id = %q, want the dangling id %q", r.AppID, s.appID)
	}
	// The other two names are unaffected by the missing app.
	if got := mustName(t, r.Username, "username"); got != "u" {
		t.Errorf("username = %q, want %q", got, "u")
	}
	if got := mustName(t, r.HostName, "host_name"); got != "host-1" {
		t.Errorf("host_name = %q, want %q", got, "host-1")
	}
}

// TestAdminListOrderAndPaginationSurviveTheJoin guards the two things a join can
// quietly break: a subquery's ORDER BY is not preserved through a join, and the
// limit+1 lookahead that produces next_cursor must still count session rows (the
// joins are all many-to-one, so they must not multiply rows).
func TestAdminListOrderAndPaginationSurviveTheJoin(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Three sessions with explicitly increasing created_at, inserted oldest-first.
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = insertSessionRow(t, pool, s.userID, s.appID, &s.hostID, "stopped")
		if _, err := pool.Exec(ctx,
			`UPDATE sessions SET created_at = now() + make_interval(secs => $2) WHERE id::text = $1`,
			ids[i], i); err != nil {
			t.Fatalf("stamp created_at: %v", err)
		}
	}

	rows, next, err := store.ListAll(ctx, "", 50, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ListAll returned %d rows, want 3 (the joins must not multiply rows)", len(rows))
	}
	if next != "" {
		t.Errorf("next_cursor = %q, want empty (one page)", next)
	}
	want := []string{ids[2], ids[1], ids[0]} // newest first
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("order[%d] = %s, want %s (newest first must survive the join)", i, rows[i].ID, id)
		}
	}

	// Page size 2 → one page of 2 plus a cursor, then the remainder.
	page1, next1, err := store.ListAll(ctx, "", 2, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll page 1: %v", err)
	}
	if len(page1) != 2 || next1 != "2" {
		t.Fatalf("page 1: %d rows, next=%q; want 2 rows, next=%q", len(page1), next1, "2")
	}
	page2, next2, err := store.ListAll(ctx, next1, 2, AdminStateAll)
	if err != nil {
		t.Fatalf("ListAll page 2: %v", err)
	}
	if len(page2) != 1 || next2 != "" {
		t.Fatalf("page 2: %d rows, next=%q; want 1 row, no next", len(page2), next2)
	}
	if page2[0].ID != ids[0] {
		t.Errorf("page 2 row = %s, want the oldest %s", page2[0].ID, ids[0])
	}
}
