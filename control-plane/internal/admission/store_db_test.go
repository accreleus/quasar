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
