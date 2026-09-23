package admission

import (
	"context"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL unset")
	}
	if err := migrate.Run(migrations.FS, url); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Re-run 0088 in a rollback-only transaction against the previous shape.
// A draining row is ambiguous: it must remain restricted after upgrade.
func TestMigrationPreservesAmbiguousLegacyDrain(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DROP TABLE host_admission_restrictions`); err != nil {
		t.Fatal(err)
	}
	var hostID string
	if err := tx.QueryRow(ctx, `INSERT INTO hosts (node_name,status) VALUES ('rh05-migration-drain','draining') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	sql, err := fs.ReadFile(migrations.FS, "0088_host_admission_restrictions.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0088: %v", err)
	}
	var kind, status string
	if err := tx.QueryRow(ctx, `SELECT owner_kind FROM host_admission_restrictions WHERE host_id=$1::uuid`, hostID).Scan(&kind); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid`, hostID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if kind != "legacy" || status != "draining" {
		t.Fatalf("after 0088: owner=%s status=%s", kind, status)
	}
}

// A fleet run's was_cordoned=false snapshot cannot reveal whether an admin
// also requested a drain after the run took its cordon. Migrating that snapshot
// must keep the host closed when the run later releases only its own hold.
func TestMigrationKeepsAmbiguousDrainAfterPlatformRelease(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DROP TABLE host_admission_restrictions`); err != nil {
		t.Fatal(err)
	}
	var hostID, releaseID, runID string
	if err := tx.QueryRow(ctx, `INSERT INTO hosts (node_name,status)
		VALUES ('rh05-platform-migration-drain','draining') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO platform_releases (channel,source_commit,built_at,schema_version)
		VALUES ('edge',gen_random_uuid()::text,now(),88) RETURNING id::text`).Scan(&releaseID); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `INSERT INTO platform_apply_runs (release_id,state,cordoned_hosts)
		VALUES ($1::uuid,'running',jsonb_build_array(jsonb_build_object(
			'host_id',$2::text,'was_cordoned',false))) RETURNING id::text`, releaseID, hostID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	sql, err := fs.ReadFile(migrations.FS, "0088_host_admission_restrictions.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply 0088: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM platform_apply_runs WHERE id=$1::uuid`, runID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM platform_releases WHERE id=$1::uuid`, releaseID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM hosts WHERE id=$1::uuid`, hostID)
	})

	holds := NewStore(pool)
	before, err := holds.List(ctx, hostID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[Kind]bool{}
	for _, restriction := range before {
		kinds[restriction.OwnerKind] = true
	}
	if len(before) != 2 || !kinds[Platform] || !kinds[Legacy] {
		t.Fatalf("migration owners = %+v, want independent platform and legacy holds", before)
	}
	if status, err := holds.Release(ctx, hostID, Owner{Kind: Platform, ID: runID}, true); err != nil || status != "draining" {
		t.Fatalf("platform release status = %q (%v), want draining", status, err)
	}
	afterPlatform, err := holds.List(ctx, hostID)
	if err != nil || len(afterPlatform) != 1 || afterPlatform[0].OwnerKind != Legacy {
		t.Fatalf("after platform release = %+v (%v), want legacy hold", afterPlatform, err)
	}
	if status, err := holds.ReleaseManual(ctx, hostID, true); err != nil || status != "online" {
		t.Fatalf("explicit uncordon status = %q (%v), want online", status, err)
	}
	if remaining, err := holds.List(ctx, hostID); err != nil || len(remaining) != 0 {
		t.Fatalf("after uncordon = %+v (%v), want no holds", remaining, err)
	}
}
