// home_janitor_db_test.go — SweepHomes is the pass body the storage.home_janitor
// job (internal/jobs, wired in cmd/quasar-control/app.go) now drives instead of
// the 6h-hardcoded StartHomeJanitor ticker (WP2 §8.2). Same SQL as before; this
// asserts the SQL itself (host_id IS NULL only — the host-pinned #175 pull path
// is untouched) and the returned count the job records as its run summary.
package storage

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertHomeNoHost inserts a user_homes row with host_id NULL — the "the host
// was removed" case SweepHomes targets, which insertHome (gc_test.go) has no
// way to express (it always casts its hostID argument to ::uuid).
func insertHomeNoHost(t *testing.T, pool *pgxpool.Pool, userID, appID string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, NULL, 'volume', 'vol-test')
		RETURNING id::text`, userID, appID).Scan(&id))
	return id
}

func TestSweepHomesDeletesOnlyUnreapableTombstones(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := &Manager{pool: pool}

	user := seedUser(t, pool, "sweephomes@t.local")
	app := seedApp(t, pool, "SweepHomes App")
	host := seedHost(t, pool)

	pastGraceNoHost := insertHomeNoHost(t, pool, user, app)
	_, err := pool.Exec(ctx, `UPDATE user_homes SET gc_after = now() - interval '25 hours' WHERE id=$1::uuid`, pastGraceNoHost)
	must(t, err)

	withinGraceNoHost := insertHomeNoHost(t, pool, user, app)
	_, err = pool.Exec(ctx, `UPDATE user_homes SET gc_after = now() - interval '1 hour' WHERE id=$1::uuid`, withinGraceNoHost)
	must(t, err)

	pastGraceHostPinned := insertHome(t, pool, user, app, host)
	_, err = pool.Exec(ctx, `UPDATE user_homes SET gc_after = now() - interval '25 hours' WHERE id=$1::uuid`, pastGraceHostPinned)
	must(t, err)

	notTombstoned := insertHomeNoHost(t, pool, user, app)

	n, err := m.SweepHomes(ctx, log)
	if err != nil {
		t.Fatalf("SweepHomes: %v", err)
	}
	if n != 1 {
		t.Fatalf("SweepHomes deleted = %d, want 1 (only the past-grace, host_id IS NULL row)", n)
	}

	for id, wantGone := range map[string]bool{
		pastGraceNoHost:     true,
		withinGraceNoHost:   false,
		pastGraceHostPinned: false, // host-pinned: left for the agent-pull reaper (#175)
		notTombstoned:       false,
	} {
		var exists bool
		must(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM user_homes WHERE id=$1::uuid)`, id).Scan(&exists))
		if exists == wantGone {
			t.Errorf("home %s: exists=%v, want exists=%v", id, exists, !wantGone)
		}
	}
}
