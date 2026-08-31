package storage

// migration_0069_test.go — the data coercion in
// migrations/0069_remove_volume_storage_driver.up.sql (#473 hard removal of
// the docker-volume managed-home driver, operator direction 2026-08-25).
//
// IT RUNS THE REAL SQL, the same discipline migration_0065_test.go follows and
// for the same reason: a hand-copied predicate would keep passing after the
// migration itself drifted from it.
//
// TEST_DATABASE_URL-gated like every other DB test here: `make test-db`.

import (
	"context"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// run0069 runs migration 0069's UPDATE against the current database state
// and returns the resulting storage_provider. Like pin0065, this replays an
// idempotent statement over seeded state — sound because the migration
// already ran once (against an empty database) by the time testDB returns.
func run0069(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile("0069_remove_volume_storage_driver.up.sql")
	if err != nil {
		t.Fatalf("read migration 0069: %v", err)
	}
	stmt := sqlBody(string(raw))
	if !strings.Contains(stmt, "UPDATE instance_settings") {
		t.Fatalf("migration 0069 body does not contain the expected UPDATE:\n%s", stmt)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("run migration 0069 statement: %v", err)
	}
	var prov string
	must(t, pool.QueryRow(ctx, `SELECT storage_provider FROM instance_settings LIMIT 1`).Scan(&prov))
	return prov
}

// TestMigration0069CoercesVolumeToLocal is the migration's whole job: a row
// left on the explicit legacy 'volume' setting becomes 'local'.
func TestMigration0069CoercesVolumeToLocal(t *testing.T) {
	pool := testDB(t)
	seedSettings(t, pool, "volume")

	if got := run0069(t, pool); got != "local" {
		t.Errorf("storage_provider after migration 0069 = %q, want local", got)
	}
}

// TestMigration0069LeavesAutoAndLocalAlone — the migration must be a targeted
// fix for the one removed value, not a blanket reset. 'auto' and 'local' were
// never the docker-volume driver and must survive byte-identical.
func TestMigration0069LeavesAutoAndLocalAlone(t *testing.T) {
	for _, prov := range []string{"auto", "local"} {
		t.Run(prov, func(t *testing.T) {
			pool := testDB(t)
			seedSettings(t, pool, prov)

			if got := run0069(t, pool); got != prov {
				t.Errorf("storage_provider after migration 0069 = %q, want unchanged %q", got, prov)
			}
		})
	}
}

// TestMigration0069IsIdempotent — replaying it (the same guarantee every
// migration in this codebase gives, since m.Up() can retry after a partial
// failure) must not error or change an already-coerced row.
func TestMigration0069IsIdempotent(t *testing.T) {
	pool := testDB(t)
	seedSettings(t, pool, "volume")

	run0069(t, pool)
	if got := run0069(t, pool); got != "local" {
		t.Errorf("storage_provider after re-running migration 0069 = %q, want local", got)
	}
}
