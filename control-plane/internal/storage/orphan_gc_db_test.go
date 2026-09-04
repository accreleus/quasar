package storage

// #92: an ORPHANED tombstone — one whose owning users row is gone — skips the
// 24h GC grace. TEST_DATABASE_URL-gated; reuses the gc_test.go / agent_gc_test.go
// helpers.

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// The grace window exists so a launch can revive a tombstoned home. Nothing can
// revive a home whose owner no longer exists, so the window protects nothing and
// only costs disk.
func TestGCPending_OrphanSkipsTheGracePeriod(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "orph@test")
	app1 := seedApp(t, pool, "o1")
	app2 := seedApp(t, pool, "o2")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")

	// Owner still exists, tombstoned a minute ago: protected by the grace.
	owned := insertHome(t, pool, u, app1, hostA)
	setGCAfter(t, pool, owned, "interval '1 minute'")
	// Same age, but the owner row went with a user delete: reapable now.
	orphan := insertHome(t, pool, u, app2, hostA)
	setGCAfter(t, pool, orphan, "interval '1 minute'")
	_, err := pool.Exec(ctx, `UPDATE user_homes SET user_id = NULL WHERE id::text = $1`, orphan)
	must(t, err)

	pending, err := mgr.GCPending(ctx, hostA)
	must(t, err)
	if len(pending) != 1 || pending[0].ID != orphan {
		t.Fatalf("GCPending: want only the orphan %s, got %v", orphan, pending)
	}

	// The confirm must honour the same predicate, or the agent reaps a store
	// whose row it can never delete.
	deleted, err := mgr.GCConfirm(ctx, hostA, []string{orphan, owned})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("GCConfirm deleted = %d, want 1 (the orphan only)", deleted)
	}
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, owned).Scan(&n))
	if n != 1 {
		t.Errorf("owned in-grace home was wrongly deleted")
	}
}

// The janitor's row-only sweep uses the same predicate: an orphan with no host
// has no agent to reap it and no reason to wait.
func TestJanitor_ReapsOrphanNullHostImmediately(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	u := seedUser(t, pool, "jo@test")
	app1 := seedApp(t, pool, "jo1")
	app2 := seedApp(t, pool, "jo2")
	hostA := seedHostWithSecret(t, pool, "node-a", "s")

	owned := insertHome(t, pool, u, app1, hostA)
	setGCAfter(t, pool, owned, "interval '1 minute'")
	orphan := insertHome(t, pool, u, app2, hostA)
	setGCAfter(t, pool, orphan, "interval '1 minute'")
	_, err := pool.Exec(ctx,
		`UPDATE user_homes SET host_id = NULL WHERE id::text IN ($1, $2)`, owned, orphan)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE user_homes SET user_id = NULL WHERE id::text = $1`, orphan)
	must(t, err)

	mgr := NewLocal(pool, t.TempDir())
	n, err := mgr.SweepHomes(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	must(t, err)
	if n != 1 {
		t.Fatalf("SweepHomes deleted %d rows, want 1 (the orphan only)", n)
	}
	var left int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, owned).Scan(&left))
	if left != 1 {
		t.Errorf("owned in-grace home was wrongly swept")
	}
}
