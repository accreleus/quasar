// migration_0048_db_test.go — the scan-observability + backfill amendment's
// migration (protocol/control-api.md "Amendment — scan observability +
// backfill (2026-08-01, same-day follow-on)", protocol pin 4dd4691): the
// eight outcome-count columns on library_scans.
//
// A DEDICATED SCRATCH-DATABASE TEST, same pattern (and same reasoning) as
// migration_0047_db_test.go: testDB(t) is shared by every other DB test in
// this package, and a truncate elsewhere could make an assertion here pass or
// fail on an unrelated suite's ordering rather than on the migration itself.
package library

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// version48 names the migration boundary this file steps across. version47
// is already declared in migration_0047_db_test.go.
const version48 = 48

// TestMigration0048AddsZeroDefaultCountColumns asserts the eight count
// columns exist, default to zero, and are NOT NULL — the "pre-0048 rows read
// zero, which a UI presents as not recorded" contract, and the "genuinely
// zero" case a freshly-failed scan legitimately produces, both rest on this
// default existing at all.
func TestMigration0048AddsZeroDefaultCountColumns(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version48)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	// A minimal row: users/apps/hosts satisfied with bare inserts, mirroring
	// the shape newFixture uses elsewhere in this package.
	var userID, appID, hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash, role)
		VALUES ('m48@t.local','m48','x','user') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name, library_provider, managed_home)
		VALUES ('Steam', 'steam', true) RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('m48-host', 'online', 'deadbeef') RETURNING id::text`).Scan(&hostID))

	var scanID string
	must(t, pool.QueryRow(ctx, `INSERT INTO library_scans (user_id, app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'pending') RETURNING id::text`,
		userID, appID, hostID).Scan(&scanID))

	var observed, suppressed, created, disabled, granted, revoked, rejected, backfilled int
	must(t, pool.QueryRow(ctx, `
		SELECT observed, suppressed, created, disabled, granted, revoked, rejected, backfilled
		  FROM library_scans WHERE id::text = $1`, scanID).
		Scan(&observed, &suppressed, &created, &disabled, &granted, &revoked, &rejected, &backfilled))

	for name, got := range map[string]int{
		"observed": observed, "suppressed": suppressed, "created": created,
		"disabled": disabled, "granted": granted, "revoked": revoked,
		"rejected": rejected, "backfilled": backfilled,
	} {
		if got != 0 {
			t.Errorf("library_scans.%s default = %d, want 0", name, got)
		}
	}

	// NOT NULL: an explicit NULL write must be refused at the database level.
	if _, err := pool.Exec(ctx, `UPDATE library_scans SET backfilled = NULL WHERE id::text = $1`, scanID); err == nil {
		t.Error("library_scans.backfilled accepted NULL, want NOT NULL to refuse it")
	}
}

// TestMigration0048DownDropsCountColumns is the down-migration mirror: after
// rolling back, none of the eight columns exist.
func TestMigration0048DownDropsCountColumns(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version48)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	for _, col := range []string{"observed", "suppressed", "created", "disabled",
		"granted", "revoked", "rejected", "backfilled"} {
		var exists bool
		must(t, pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'library_scans' AND column_name = $1)`, col).Scan(&exists))
		if exists {
			t.Errorf("library_scans.%s still exists after rolling 0048 down", col)
		}
	}
}
