package agentws

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/migrations"
)

type storedIdentity struct {
	SourceCommit   *string
	BuiltAt        *time.Time
	InstallMode    *string
	UpdaterPresent *bool
}

func readIdentity(t *testing.T, pool *pgxpool.Pool, hostID string) storedIdentity {
	t.Helper()
	var got storedIdentity
	if err := pool.QueryRow(context.Background(),
		`SELECT source_commit, built_at, install_mode, updater_present
		 FROM hosts WHERE id::text = $1`, hostID).
		Scan(&got.SourceCommit, &got.BuiltAt, &got.InstallMode, &got.UpdaterPresent); err != nil {
		t.Fatalf("query identity: %v", err)
	}
	return got
}

func TestReplaceHostIdentityStoresAllFourFields(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	// A fresh host is identity-unknown: nothing has said anything yet.
	if got := readIdentity(t, pool, hostID); got.SourceCommit != nil || got.BuiltAt != nil ||
		got.InstallMode != nil || got.UpdaterPresent != nil {
		t.Fatalf("fresh host: got %+v, want all NULL", got)
	}

	id, dropped := identityFromRegister(RegisterMsg{
		SourceCommit:   strPtr("1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"),
		BuiltAt:        strPtr("2026-09-04T12:00:00Z"),
		InstallMode:    strPtr("registry"),
		UpdaterPresent: boolPtr(true),
	})
	if len(dropped) != 0 {
		t.Fatalf("valid identity was partly dropped: %v", dropped)
	}
	if err := s.replaceHostIdentity(context.Background(), hostID, id); err != nil {
		t.Fatalf("replace identity: %v", err)
	}

	got := readIdentity(t, pool, hostID)
	if got.SourceCommit == nil || *got.SourceCommit != "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567" {
		t.Errorf("source_commit = %v", got.SourceCommit)
	}
	if got.BuiltAt == nil || !got.BuiltAt.UTC().Equal(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("built_at = %v", got.BuiltAt)
	}
	if got.InstallMode == nil || *got.InstallMode != "registry" {
		t.Errorf("install_mode = %v", got.InstallMode)
	}
	if got.UpdaterPresent == nil || !*got.UpdaterPresent {
		t.Errorf("updater_present = %v", got.UpdaterPresent)
	}
}

// The rule this migration exists for: identity is REPLACED WHOLESALE, so an
// agent downgraded to a pre-amendment build reads as unknown rather than
// keeping a commit that describes nothing running. Deliberately unlike
// storage/codecs/readiness, which are keep-if-absent.
func TestReplaceHostIdentityNullsEveryAbsentFieldOnTheNextRegister(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	known, _ := identityFromRegister(RegisterMsg{
		SourceCommit:   strPtr("1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"),
		BuiltAt:        strPtr("2026-09-04T12:00:00Z"),
		InstallMode:    strPtr("source"),
		UpdaterPresent: boolPtr(false),
	})
	if err := s.replaceHostIdentity(context.Background(), hostID, known); err != nil {
		t.Fatalf("replace identity: %v", err)
	}
	// `false` is a real answer and must survive as false, not become NULL.
	if got := readIdentity(t, pool, hostID); got.UpdaterPresent == nil || *got.UpdaterPresent {
		t.Fatalf("updater_present after a false report = %v, want false", got.UpdaterPresent)
	}

	// Now a pre-amendment agent registers: no identity fields at all.
	old, _ := identityFromRegister(RegisterMsg{})
	if err := s.replaceHostIdentity(context.Background(), hostID, old); err != nil {
		t.Fatalf("replace identity (old agent): %v", err)
	}
	got := readIdentity(t, pool, hostID)
	if got.SourceCommit != nil || got.BuiltAt != nil || got.InstallMode != nil || got.UpdaterPresent != nil {
		t.Errorf("after a pre-amendment register: got %+v, want all NULL", got)
	}
}

// A value outside the closed vocabulary must never reach the column: the CHECK
// would refuse the write and take the whole registration down with it.
func TestReplaceHostIdentityDropsAnUnknownInstallModeRatherThanFailingTheWrite(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	id, dropped := identityFromRegister(RegisterMsg{
		SourceCommit: strPtr("1f0c1e0"), // short, and valid: 7-40 hex, stored as sent
		InstallMode:  strPtr("kubernetes"),
	})
	if len(dropped) != 1 || dropped[0] != "install_mode" {
		t.Fatalf("dropped = %v, want [install_mode]", dropped)
	}
	if err := s.replaceHostIdentity(context.Background(), hostID, id); err != nil {
		t.Fatalf("replace identity: %v", err)
	}
	got := readIdentity(t, pool, hostID)
	if got.InstallMode != nil {
		t.Errorf("install_mode = %v, want NULL", *got.InstallMode)
	}
	if got.SourceCommit == nil || *got.SourceCommit != "1f0c1e0" {
		t.Errorf("source_commit = %v, want the short sha stored exactly as sent", got.SourceCommit)
	}
}

// Migration 0074's other halves, asserted where they can actually be seen.
func TestMigration0074AddsThePlatformReleaseSurface(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// instance_settings gains two DEFAULTED columns, so the singleton row
	// upgrades in place with no seeding step.
	var channel, branch string
	if err := pool.QueryRow(ctx, `
		INSERT INTO instance_settings (id) VALUES (true)
		ON CONFLICT (id) DO UPDATE SET id = true
		RETURNING release_channel, release_edge_branch`).Scan(&channel, &branch); err != nil {
		t.Fatalf("instance_settings: %v", err)
	}
	if channel != "stable" || branch != "develop" {
		t.Errorf("defaults = %q/%q, want stable/develop", channel, branch)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE instance_settings SET release_channel = 'nightly'`); err == nil {
		t.Error("release_channel accepted a value outside (stable, edge)")
	}

	// platform_releases: the detector's idempotency key is (channel,
	// source_commit), and version is unique only where it is non-null.
	insert := `INSERT INTO platform_releases (channel, version, source_commit, built_at, schema_version)
	           VALUES ($1, $2, $3, now(), 74)`
	if _, err := pool.Exec(ctx, insert, "stable", "0.2.0", "aaaa"); err != nil {
		t.Fatalf("insert stable release: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "stable", "0.2.1", "aaaa"); err == nil {
		t.Error("re-detecting the same commit on the same channel inserted a duplicate")
	}
	// The same commit on the OTHER channel is legitimate (an edge build later tagged).
	if _, err := pool.Exec(ctx, insert, "edge", nil, "aaaa"); err != nil {
		t.Errorf("same commit on the other channel was refused: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "edge", nil, "bbbb"); err != nil {
		t.Errorf("a second NULL-version edge row was refused by the partial unique index: %v", err)
	}
	if _, err := pool.Exec(ctx, insert, "edge", "0.2.0", "cccc"); err == nil {
		t.Error("two rows claiming version 0.2.0 were allowed")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM platform_releases`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// The down migration, exercised for real but inside a transaction that is
// rolled back: an untested down is how a rollback discovers its own syntax
// error, and the alternative (golang-migrate Steps(-1) against the shared test
// database) would leave every later package running against a schema this test
// tore down.
func TestMigration0074DownDropsEverythingItAdded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// 0075's tables reference platform_releases, so its down runs first — the
	// order golang-migrate itself uses.
	down0075, err := migrations.FS.ReadFile("0075_platform_apply.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}
	down, err := migrations.FS.ReadFile("0074_platform_release_identity.down.sql")
	if err != nil {
		t.Fatalf("read down migration: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, string(down0075)); err != nil {
		t.Fatalf("down migration 0075: %v", err)
	}
	if _, err := tx.Exec(ctx, string(down)); err != nil {
		t.Fatalf("down migration: %v", err)
	}

	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'hosts'
		       AND column_name IN ('source_commit','built_at','install_mode','updater_present'))
		   OR (table_name = 'instance_settings'
		       AND column_name IN ('release_channel','release_edge_branch'))`).Scan(&n); err != nil {
		t.Fatalf("count columns: %v", err)
	}
	if n != 0 {
		t.Errorf("%d of the six added columns survived the down migration", n)
	}

	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_name = 'platform_releases'`).
		Scan(&n); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if n != 0 {
		t.Error("platform_releases survived the down migration")
	}
}
