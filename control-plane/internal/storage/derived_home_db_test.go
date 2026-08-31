package storage

// steam-library-discovery Phase 3 — the STORAGE-LAYER half of spec §2's
// homeAppID rule, plus RequireHome.
//
// §2 lists seven sites keyed on app_id that are actually asking a storage
// question. Three of them are SQL in this package and are covered here, one test
// each, named so a reviewer can match them to the table:
//
//	site 3  hasLiveSessionForHome  → TestHomeRuleSite3TombstoneGuardSeesDerivedTileSession
//	site 6  TouchUsed              → TestHomeRuleSite6TouchUsedStampsTheParentHome
//	site 7  ReportBytesUsed        → TestHomeRuleSite7ReportBytesUsedWritesTheParentHome
//
// Sites 1, 2, 4 and 5 are in internal/session; see derived_tiles_db_test.go
// there for the other four.
//
// EVERY ONE OF THESE THREE WOULD PASS AGAINST THE OLD CODE IF IT WERE WRITTEN
// ONLY WITH AN ORDINARY APP. Each therefore asserts the DERIVED case, and site 3
// additionally asserts that the ordinary case is unchanged — the old join
// (`uh.app_id = s.app_id`) is right for an ordinary app and simply never matches
// a tile, which is why the bug was invisible.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedProviderApp inserts a managed-home "Steam" provider app — the parent a
// derived tile borrows everything from.
func seedProviderApp(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path,
		 kind, library_provider)
		VALUES ($1, 512, 1, 1280, 720, 30, 2000, '{"image":"steam:1"}', true, '/home/quasar',
		        'launcher', 'steam')
		RETURNING id::text`, name).Scan(&id))
	return id
}

// seedDerivedTile inserts a tile of parentID for the given Steam appid. Every
// column apps_derived_shape_ck constrains is left at the tile-shaped value, so
// this fixture doubles as a demonstration of the legal shape.
func seedDerivedTile(t *testing.T, pool *pgxpool.Pool, parentID, name, appID string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, parent_app_id, external_source, external_id, origin, kind)
		VALUES ($1, $2::uuid, 'steam', $3, 'manual', 'game')
		RETURNING id::text`, name, parentID, appID).Scan(&id))
	return id
}

// --- §2 site 3: hasLiveSessionForHome ---------------------------------------

// TestHomeRuleSite3TombstoneGuardSeesDerivedTileSession is the dangerous one.
//
// TombstoneHome stands in front of a DATA-DESTRUCTION path: it sets gc_after and
// the agent-pull reaper then deletes the backing store. Its guard used to join
// `uh.app_id = s.app_id`, which NEVER matches a derived-tile session — a tile has
// no user_homes row of its own — so an admin could tombstone a Steam library that
// a live session was mid-write into, and the guard would cheerfully report "not
// in use".
//
// The test asserts BOTH directions, because a fix that simply made the guard
// always fire would also "pass" a one-sided assertion:
//   - a live session on the TILE blocks tombstoning the PARENT's home;
//   - with that session terminal, the tombstone succeeds.
func TestHomeRuleSite3TombstoneGuardSeesDerivedTileSession(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "site3@test")
	parent := seedProviderApp(t, pool, "Steam")
	tile := seedDerivedTile(t, pool, parent, "Hades", "1145360")
	host := seedHost(t, pool)

	// The home belongs to the PARENT. The tile owns none, and never will.
	homeID := insertHome(t, pool, user, parent, host)

	// A live session on the TILE. This is the session writing to the parent's
	// steamapps tree.
	sessID := insertRunningSession(t, pool, user, tile, host)

	if _, err := mgr.TombstoneHome(ctx, homeID); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("TombstoneHome with a live DERIVED-TILE session: err = %v, want ErrHomeInUse.\n"+
			"This is §2 site 3: the guard joined uh.app_id = s.app_id, which never matches a tile, "+
			"so an admin could tombstone (and the reaper then delete) a home being actively written to.", err)
	}

	// Guard the guard: the row must be untouched, not merely the call refused.
	var gcAfterSet bool
	must(t, pool.QueryRow(ctx,
		`SELECT gc_after IS NOT NULL FROM user_homes WHERE id::text = $1`, homeID).Scan(&gcAfterSet))
	if gcAfterSet {
		t.Error("gc_after was set despite ErrHomeInUse — the refusal must leave no trace")
	}

	// Once the session is terminal the home is tombstonable, so the fix is not
	// simply "always refuse".
	must(t, exec(ctx, pool, `UPDATE sessions SET state = 'stopped' WHERE id::text = $1`, sessID))
	if _, err := mgr.TombstoneHome(ctx, homeID); err != nil {
		t.Fatalf("TombstoneHome after the tile's session ended: %v, want success", err)
	}
}

// TestHomeRuleSite3OrdinaryAppUnchanged pins the pre-existing behaviour the site-3
// rewrite must not disturb: a live session on an ORDINARY app still blocks
// tombstoning that app's own home.
func TestHomeRuleSite3OrdinaryAppUnchanged(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "site3b@test")
	app := seedApp(t, pool, "ordinary")
	host := seedHost(t, pool)
	homeID := insertHome(t, pool, user, app, host)
	insertRunningSession(t, pool, user, app, host)

	if _, err := mgr.TombstoneHome(ctx, homeID); !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("TombstoneHome with a live ordinary-app session: err = %v, want ErrHomeInUse", err)
	}
}

// --- §2 site 6: TouchUsed ----------------------------------------------------

// TestHomeRuleSite6TouchUsedStampsTheParentHome covers the quiet one.
//
// TouchUsed is called with the SESSION's app id, which for a derived tile is the
// tile. Keyed on that, the UPDATE matches nothing: no error, no log, no row. The
// consequence is not a broken launch but two things that degrade silently — the
// §5 placement pin and policyOrderSQL both order by last_used_at, and so does GC
// candidate selection. A Steam family whose launches all go through tiles would
// have a home that never appears to have been used.
func TestHomeRuleSite6TouchUsedStampsTheParentHome(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "site6@test")
	parent := seedProviderApp(t, pool, "Steam")
	tile := seedDerivedTile(t, pool, parent, "Hades", "1145360")
	host := seedHost(t, pool)
	homeID := insertHome(t, pool, user, parent, host)

	// Age the stamp so "did it move" is unambiguous.
	must(t, exec(ctx, pool,
		`UPDATE user_homes SET last_used_at = now() - interval '10 days' WHERE id::text = $1`, homeID))
	var before string
	must(t, pool.QueryRow(ctx, `SELECT last_used_at::text FROM user_homes WHERE id::text = $1`, homeID).Scan(&before))

	// Called exactly as Coordinator.AgentState calls it: with the TILE's id.
	must(t, mgr.TouchUsed(ctx, user, tile, host))

	var after string
	must(t, pool.QueryRow(ctx, `SELECT last_used_at::text FROM user_homes WHERE id::text = $1`, homeID).Scan(&after))
	if after == before {
		t.Fatalf("last_used_at did not advance on the PARENT's home after a derived-tile session ended "+
			"(still %s).\nThis is §2 site 6: TouchUsed keyed on the tile matches no row, silently.", before)
	}
}

// --- §2 site 7: ReportBytesUsed ---------------------------------------------

// TestHomeRuleSite7ReportBytesUsedWritesTheParentHome is site 6's sibling and
// fails the same way: the UPDATE ... FROM sessions joined uh.app_id = s.app_id,
// so a derived-tile session's storage report was silently discarded and the
// admin storage view simply stopped moving for those users.
func TestHomeRuleSite7ReportBytesUsedWritesTheParentHome(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "site7@test")
	parent := seedProviderApp(t, pool, "Steam")
	tile := seedDerivedTile(t, pool, parent, "Hades", "1145360")
	host := seedHost(t, pool)
	homeID := insertHome(t, pool, user, parent, host)
	sessID := insertRunningSession(t, pool, user, tile, host)

	const want = int64(4096)
	must(t, mgr.ReportBytesUsed(ctx, sessID, want))

	var got int64
	must(t, pool.QueryRow(ctx, `SELECT bytes_used FROM user_homes WHERE id::text = $1`, homeID).Scan(&got))
	if got != want {
		t.Fatalf("user_homes.bytes_used = %d, want %d.\n"+
			"This is §2 site 7: the join keyed on the tile matches no row and the write vanishes with no error.", got, want)
	}
}

// TestReportBytesUsedSkipsTombstonedHome pins the guard the site-7 rewrite kept:
// a home already marked for reaping is not written to.
func TestReportBytesUsedSkipsTombstonedHome(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "site7b@test")
	parent := seedProviderApp(t, pool, "Steam")
	tile := seedDerivedTile(t, pool, parent, "Hades", "1145360")
	host := seedHost(t, pool)
	homeID := insertHome(t, pool, user, parent, host)
	sessID := insertRunningSession(t, pool, user, tile, host)
	must(t, exec(ctx, pool, `UPDATE user_homes SET gc_after = now() WHERE id::text = $1`, homeID))

	must(t, mgr.ReportBytesUsed(ctx, sessID, 4096)) // a miss is a no-op, not an error

	var got int64
	must(t, pool.QueryRow(ctx, `SELECT bytes_used FROM user_homes WHERE id::text = $1`, homeID).Scan(&got))
	if got != 0 {
		t.Errorf("bytes_used = %d on a tombstoned home, want 0 (the write must be skipped)", got)
	}
}

// --- RequireHome (spec §3) ---------------------------------------------------

// TestRequireHomeReturnsTheParentMountAndTouches is the happy path, and it
// asserts the property that matters most: the mount string RequireHome produces
// is BYTE-IDENTICAL to the one EnsureHome would have produced for the parent.
// The two apps mount the same directory, and a divergence would be invisible
// until a container saw a different path.
func TestRequireHomeReturnsTheParentMountAndTouches(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "req@test")
	parent := seedProviderApp(t, pool, "Steam")
	host := seedHost(t, pool)

	// EnsureHome is how the home came to exist: the user launched Steam once.
	ensured, err := mgr.EnsureHome(ctx, user, parent, host, "/home/quasar")
	must(t, err)

	required, err := mgr.RequireHome(ctx, user, parent, host, "/home/quasar")
	must(t, err)
	if required != ensured {
		t.Errorf("RequireHome mount %q != EnsureHome mount %q — a derived tile must mount the exact "+
			"same string its parent would have", required, ensured)
	}

	// It advances last_used_at, exactly as EnsureHome's upsert does.
	must(t, exec(ctx, pool,
		`UPDATE user_homes SET last_used_at = now() - interval '10 days'
		 WHERE user_id::text=$1 AND app_id::text=$2`, user, parent))
	must(t, exec(ctx, pool, `SELECT 1`))
	if _, err := mgr.RequireHome(ctx, user, parent, host, "/home/quasar"); err != nil {
		t.Fatalf("RequireHome (touch): %v", err)
	}
	var stale bool
	must(t, pool.QueryRow(ctx, `SELECT last_used_at < now() - interval '1 day'
		FROM user_homes WHERE user_id::text=$1 AND app_id::text=$2`, user, parent).Scan(&stale))
	if stale {
		t.Error("RequireHome did not advance last_used_at")
	}
}

// TestRequireHomeNeverCreatesAndNeverResurrects is the whole reason RequireHome
// exists rather than a call to EnsureHome, and it asserts both halves of §3:
//
//  1. NO ROW: it must not provision. EnsureHome would have created one, mounted
//     an empty directory, and let the session reach `running` looking healthy
//     with the game absent — a failure nothing in session_state, session_metrics
//     or the trace events can distinguish from success.
//  2. NO RESURRECTION: EnsureHome's upsert sets gc_after = NULL, so it would
//     un-tombstone a home an admin has just marked for reaping.
func TestRequireHomeNeverCreatesAndNeverResurrects(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	user := seedUser(t, pool, "req2@test")
	parent := seedProviderApp(t, pool, "Steam")
	host := seedHost(t, pool)

	// (1) No row at all.
	if _, err := mgr.RequireHome(ctx, user, parent, host, "/home/quasar"); !errors.Is(err, ErrHomeNotProvisioned) {
		t.Fatalf("RequireHome with no home: err = %v, want ErrHomeNotProvisioned", err)
	}
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes`).Scan(&n))
	if n != 0 {
		t.Fatalf("RequireHome created %d user_homes row(s); it must never provision one", n)
	}

	// (2) A tombstoned row is treated as absent AND left tombstoned.
	homeID := insertHome(t, pool, user, parent, host)
	must(t, exec(ctx, pool, `UPDATE user_homes SET gc_after = now() WHERE id::text = $1`, homeID))

	if _, err := mgr.RequireHome(ctx, user, parent, host, "/home/quasar"); !errors.Is(err, ErrHomeNotProvisioned) {
		t.Fatalf("RequireHome on a TOMBSTONED home: err = %v, want ErrHomeNotProvisioned", err)
	}
	var stillTombstoned bool
	must(t, pool.QueryRow(ctx,
		`SELECT gc_after IS NOT NULL FROM user_homes WHERE id::text = $1`, homeID).Scan(&stillTombstoned))
	if !stillTombstoned {
		t.Error("RequireHome cleared gc_after — that is EnsureHome's upsert behaviour and it must not be reachable here")
	}
}

func TestRequireHomeRejectsRelativeContainerPath(t *testing.T) {
	m := NewLocal(nil, t.TempDir()) // pool untouched on this error path
	if _, err := m.RequireHome(context.Background(), u, a, u, "relative/home"); err == nil ||
		!strings.Contains(err.Error(), "not absolute") {
		t.Errorf("RequireHome with a relative path: err = %v, want 'not absolute'", err)
	}
}

// exec is a terse pool.Exec that discards the command tag.
func exec(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(ctx, sql, args...)
	return err
}
