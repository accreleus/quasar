package storage

// migration_0065_test.go — the pin predicate of
// migrations/0065_pin_volume_fallback_instances.up.sql, exercised BOTH WAYS.
//
// This migration is the only part of the "storage root is the control" change
// that can break a running deployment, and it can break it in two opposite
// directions: pin too eagerly and a healthy local instance is dragged back to
// the legacy volume driver for every home it creates from now on; pin too shyly
// and an instance that was living on the old silent fallback starts failing
// every launch. Neither failure announces itself — both look like a normal
// deploy — so the predicate is tested rather than reasoned about.
//
// IT RUNS THE REAL SQL. The statement is read out of the embedded migrations FS
// and executed verbatim (minus its transaction wrapper, since pgx's extended
// protocol takes one statement per Exec). A hand-copied predicate in a test
// would pass forever after the migration itself was edited, which for a
// data-migration test is worse than having no test at all.
//
// TEST_DATABASE_URL-gated like every other DB test here: `make test-db`.

import (
	"context"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pin0065 runs migration 0065's UPDATE against the current database state and
// returns the resulting storage_provider.
//
// The migration has ALREADY run once by the time testDB returns (migrate.Run
// applies everything), against an empty database where its predicate matched
// nothing. Re-running the same statement over seeded state is exactly what the
// migration would have done had that state existed at deploy time, and the
// statement is idempotent, so replaying it is sound.
func pin0065(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile("0065_pin_volume_fallback_instances.up.sql")
	if err != nil {
		t.Fatalf("read migration 0065: %v", err)
	}
	stmt := sqlBody(string(raw))
	if !strings.Contains(stmt, "UPDATE instance_settings") {
		t.Fatalf("migration 0065 body does not contain the expected UPDATE:\n%s", stmt)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("run migration 0065 statement: %v", err)
	}
	var prov string
	must(t, pool.QueryRow(ctx, `SELECT storage_provider FROM instance_settings LIMIT 1`).Scan(&prov))
	return prov
}

// sqlBody strips comment lines and the BEGIN/COMMIT wrapper, leaving the single
// statement pgx can execute over the extended protocol.
func sqlBody(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "--"), trimmed == "",
			strings.EqualFold(trimmed, "BEGIN;"), strings.EqualFold(trimmed, "COMMIT;"):
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// seedSettings puts the instance_settings singleton at a known provider.
func seedSettings(t *testing.T, pool *pgxpool.Pool, provider string) {
	t.Helper()
	// id is a BOOLEAN singleton key (`VALUES (true, …)`), not an integer — the
	// same shape internal/settings.Store uses.
	_, err := pool.Exec(context.Background(), `
		INSERT INTO instance_settings (id, storage_provider) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET storage_provider = EXCLUDED.storage_provider`, provider)
	must(t, err)
}

// seedHomeWithProvider inserts one user_homes row with an explicit provider and
// tombstone state, which is the evidence the predicate reads.
func seedHomeWithProvider(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID, provider string, tombstoned bool) {
	t.Helper()
	gc := "NULL"
	if tombstoned {
		gc = "now()"
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref, gc_after)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'ref-'||$4, `+gc+`)`,
		userID, appID, hostID, provider)
	must(t, err)
}

// fallbackReliantInstance builds the state this migration exists for: 'auto',
// one host with no root anywhere, and a live volume-backed home.
func fallbackReliantInstance(t *testing.T, pool *pgxpool.Pool) (userID, appID, hostID string) {
	t.Helper()
	seedSettings(t, pool, "auto")
	userID = seedUser(t, pool, "pinme@example.com")
	appID = seedApp(t, pool, "Steam")
	hostID = seedHost(t, pool)
	seedHomeWithProvider(t, pool, userID, appID, hostID, "volume", false)
	return
}

// TestMigration0065PinsFallbackReliantInstance — the whole reason the migration
// exists. Default 'auto', no storage root anywhere, live volume-backed homes:
// this instance is working today ONLY because 'auto' used to mean volume, so it
// must be pinned to the setting that keeps meaning volume.
func TestMigration0065PinsFallbackReliantInstance(t *testing.T) {
	pool := testDB(t)
	fallbackReliantInstance(t, pool)

	if got := pin0065(t, pool); got != "volume" {
		t.Errorf("storage_provider = %q, want volume — this instance's live homes are all volume-backed "+
			"and no host has a root, so after the resolver change 'auto' would fail every launch", got)
	}
}

// TestMigration0065LeavesHealthyLocalInstanceAlone — the opposite error, and the
// more insidious one, because the instance keeps launching sessions either way:
// pinning here would silently send every FUTURE home to the legacy driver and
// cost the operator their library, with no failure to notice.
func TestMigration0065LeavesHealthyLocalInstanceAlone(t *testing.T) {
	pool := testDB(t)
	seedSettings(t, pool, "auto")
	userID := seedUser(t, pool, "healthy@example.com")
	hostID := seedHost(t, pool)
	// The realistic shape: an OLD volume home from before the root was set,
	// plus a newer local one. The instance is demonstrably resolving to local.
	seedHomeWithProvider(t, pool, userID, seedApp(t, pool, "Old"), hostID, "volume", false)
	seedHomeWithProvider(t, pool, userID, seedApp(t, pool, "New"), hostID, "local", false)

	if got := pin0065(t, pool); got != "auto" {
		t.Errorf("storage_provider = %q, want auto — a live local home proves this instance has a working "+
			"storage root, so pinning it to the legacy driver would regress it", got)
	}
}

// TestMigration0065LeavesRootedInstanceAlone — the "configured but not yet
// launched" case. The operator set a root five minutes ago; every existing home
// predates it and is volume-backed. Reading only the homes would pin them for
// work they have already done, so the predicate also looks at the two
// database-visible rungs of the root ladder.
func TestMigration0065LeavesRootedInstanceAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, pool *pgxpool.Pool, hostID string)
	}{
		{"admin per-host override", func(t *testing.T, pool *pgxpool.Pool, hostID string) {
			_, err := pool.Exec(context.Background(), `
				INSERT INTO host_settings (host_id, overrides)
				VALUES ($1::uuid, '{"home_root":"/data/homes"}'::jsonb)
				ON CONFLICT (host_id) DO UPDATE SET overrides = EXCLUDED.overrides`, hostID)
			must(t, err)
		}},
		{"agent-reported effective_settings", func(t *testing.T, pool *pgxpool.Pool, hostID string) {
			_, err := pool.Exec(context.Background(),
				`UPDATE hosts SET effective_settings = '{"home_root":"/data/homes"}'::jsonb WHERE id::text = $1`, hostID)
			must(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			_, _, hostID := fallbackReliantInstance(t, pool)
			tc.set(t, pool, hostID)

			if got := pin0065(t, pool); got != "auto" {
				t.Errorf("storage_provider = %q, want auto — a storage root is set, so this instance is "+
					"configured for local storage even though it has not created a local home yet", got)
			}
		})
	}
}

// TestMigration0065LeavesFreshInstallAlone — no homes at all. Nothing depends on
// the old fallback, so a fresh install must land on the new behaviour (a loud
// error plus the setup wizard), not be pre-pinned to legacy.
func TestMigration0065LeavesFreshInstallAlone(t *testing.T) {
	pool := testDB(t)
	seedSettings(t, pool, "auto")

	if got := pin0065(t, pool); got != "auto" {
		t.Errorf("storage_provider = %q, want auto — a fresh install has nothing to preserve", got)
	}
}

// TestMigration0065IgnoresTombstonedHomes — homes on their way out are not a
// reason to hold an instance on a legacy driver. Their backing stores are about
// to be reaped; there is nothing left to keep mountable.
func TestMigration0065IgnoresTombstonedHomes(t *testing.T) {
	pool := testDB(t)
	seedSettings(t, pool, "auto")
	userID := seedUser(t, pool, "tombstoned@example.com")
	seedHomeWithProvider(t, pool, userID, seedApp(t, pool, "Gone"), seedHost(t, pool), "volume", true)

	if got := pin0065(t, pool); got != "auto" {
		t.Errorf("storage_provider = %q, want auto — the only volume home is tombstoned", got)
	}
}

// TestMigration0065DoesNotTouchExplicitSettings — 'local' and 'volume' were
// chosen by a human. Neither ever resolved two ways, so neither can be relying
// on the fallback, and rewriting an operator's explicit choice on the strength
// of a data heuristic would be indefensible.
func TestMigration0065DoesNotTouchExplicitSettings(t *testing.T) {
	for _, provider := range []string{"local", "volume"} {
		t.Run(provider, func(t *testing.T) {
			pool := testDB(t)
			userID := seedUser(t, pool, "explicit@example.com")
			seedHomeWithProvider(t, pool, userID, seedApp(t, pool, "Steam"), seedHost(t, pool), "volume", false)
			seedSettings(t, pool, provider)

			if got := pin0065(t, pool); got != provider {
				t.Errorf("storage_provider = %q, want %q — an explicit choice is never rewritten", got, provider)
			}
		})
	}
}
