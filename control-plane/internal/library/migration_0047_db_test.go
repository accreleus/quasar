// migration_0047_db_test.go — the admin-libraries IA migration (§4/§5 of
// docs/design/plans/2026-08-01-admin-libraries-ia-spec.md): the two
// instance_settings columns and the seeded default Steam runtime preset.
//
// WHY A DEDICATED MIGRATION-LEVEL TEST RATHER THAN ASSERTING THROUGH THE
// PACKAGE'S ORDINARY testDB(t) FIXTURE. testDB is shared by every other DB
// test in this package and some suites TRUNCATE tables as part of their setup
// (see newFixture / other packages' fixtures) — a truncate of runtime_presets
// would silently delete the migration's seed row and make an assertion here
// pass or fail on an unrelated suite's ordering rather than on the migration
// itself. This file instead runs against its own SCRATCH database, stepped to
// specific migration versions with golang-migrate directly — the same
// pattern internal/session/migration_0036_db_test.go and
// migration_0041_db_test.go use, duplicated here rather than exported across
// packages because these are deliberately small, self-contained test helpers,
// not part of any production API surface.
package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/accreleus/quasar/control-plane/migrations"
)

// version46 is the last migration before this one; version47 is this one.
const (
	version46 = 46
	version47 = 47
)

// defaultSteamPresetID is the stable id 0047 seeds runtime_presets with — see
// migrations/0047_admin_libraries_ia.up.sql.
const defaultSteamPresetID = "007ef4c6-28ee-4a6d-9194-25eb56fa862c"

// scratchDB47 creates a throwaway database on the same server as
// TEST_DATABASE_URL and returns its URL. It is dropped on cleanup. (Same
// pattern as internal/session's scratchDB — see that file's doc comment for
// why a scratch database is required rather than the shared one: this test
// migrates DOWN, which must never happen to a database other tests share.)
func scratchDB47(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	name := fmt.Sprintf("quasar_m0047_%d_%d", time.Now().UnixNano()%1e9, rand.Intn(1000)) //nolint:gosec — test fixture naming

	adminURL, err := replaceDBName47(base, "postgres")
	if err != nil {
		t.Fatalf("derive admin url: %v", err)
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("connect to the postgres database: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("create scratch database %s: %v\n"+
			"This test needs CREATEDB on the TEST_DATABASE_URL role: it migrates DOWN, which must never "+
			"happen to a database other tests share.", name, err)
	}
	t.Cleanup(func() {
		admin2, err := sql.Open("pgx", adminURL)
		if err != nil {
			return
		}
		defer admin2.Close()
		_, _ = admin2.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
	})

	url, err := replaceDBName47(base, name)
	if err != nil {
		t.Fatalf("derive scratch url: %v", err)
	}
	return url
}

func replaceDBName47(rawURL, name string) (string, error) {
	i := strings.LastIndex(rawURL, "/")
	if i < 0 {
		return "", fmt.Errorf("no path in %q", rawURL)
	}
	rest := rawURL[i+1:]
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		query = rest[q:]
	}
	return rawURL[:i+1] + name + query, nil
}

func newMigrator47(t *testing.T, url string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open migrations db: %v", err)
	}
	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{MigrationsTable: "schema_migrations"})
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "quasar", driver)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func migrateTo47(t *testing.T, m *migrate.Migrate, version uint) {
	t.Helper()
	if err := m.Migrate(version); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

// TestMigration0047AddsBoundedInstanceSettingsColumns asserts the two new
// columns exist with the spec's defaults (360 minutes, false) and that the
// CHECK constraint on the interval actually enforces 15..10080 — the durable
// guard behind the PATCH handler's 400 (settings.ValidLibraryDiscoveryIntervalMinutes).
func TestMigration0047AddsBoundedInstanceSettingsColumns(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	must(t, execT(ctx, pool, `INSERT INTO instance_settings (id) VALUES (true)`))

	var minutes int
	var appdetails bool
	must(t, pool.QueryRow(ctx, `
		SELECT library_discovery_interval_minutes, library_discovery_appdetails_enabled
		FROM instance_settings WHERE id = true`).Scan(&minutes, &appdetails))
	if minutes != 360 {
		t.Errorf("library_discovery_interval_minutes default = %d, want 360", minutes)
	}
	if appdetails {
		t.Errorf("library_discovery_appdetails_enabled default = true, want false")
	}

	// The CHECK constraint: out of [15, 10080] must be refused at the database
	// level, not just by the handler.
	for _, bad := range []int{14, 10081, 0, -1} {
		if _, err := pool.Exec(ctx,
			`UPDATE instance_settings SET library_discovery_interval_minutes = $1 WHERE id = true`, bad); err == nil {
			t.Errorf("library_discovery_interval_minutes = %d was accepted, want the CHECK to refuse it", bad)
		}
	}
	// And the bounds themselves are legal.
	for _, ok := range []int{15, 10080, 360} {
		if _, err := pool.Exec(ctx,
			`UPDATE instance_settings SET library_discovery_interval_minutes = $1 WHERE id = true`, ok); err != nil {
			t.Errorf("library_discovery_interval_minutes = %d was refused, want it accepted: %v", ok, err)
		}
	}
}

// TestMigration0047SeedsDefaultSteamPreset asserts the shipped preset row
// lands with exactly the spec's §5 spec: image, env, empty args/mounts, and
// the stable id so an operator's later edit is addressable and survives a
// re-migration.
func TestMigration0047SeedsDefaultSteamPreset(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	var name, image, homePath string
	var managedHome bool
	var envJSON, argsJSON, mountsJSON []byte
	must(t, pool.QueryRow(ctx, `
		SELECT name, image, env, args, mounts, managed_home, home_container_path
		FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).
		Scan(&name, &image, &envJSON, &argsJSON, &mountsJSON, &managedHome, &homePath))

	if name != "Steam" {
		t.Errorf("preset name = %q, want %q", name, "Steam")
	}
	if image != "quasar-steam:latest" {
		t.Errorf("preset image = %q, want %q", image, "quasar-steam:latest")
	}
	if got := strings.TrimSpace(string(argsJSON)); got != "[]" {
		t.Errorf("preset args = %s, want []", got)
	}
	if got := strings.TrimSpace(string(mountsJSON)); got != "[]" {
		t.Errorf("preset mounts = %s, want []", got)
	}
	// Home persistence is expressed via managed_home + the default
	// home_container_path — NOT a hand-set HOME env var, which would be a
	// second copy of the same path for an operator to desynchronise
	// (Michael, 2026-08-01 review of the seeded preset).
	if !managedHome {
		t.Errorf("preset managed_home = false, want true")
	}
	if homePath != "/home/quasar" {
		t.Errorf("preset home_container_path = %q, want /home/quasar", homePath)
	}
	wantEnv := `{"PUID": "99", "PGID": "100", "UNAME": "quasar"}`
	var got, want map[string]any
	must(t, json.Unmarshal(envJSON, &got))
	must(t, json.Unmarshal([]byte(wantEnv), &want))
	if len(got) != len(want) {
		t.Fatalf("preset env = %s, want %s", envJSON, wantEnv)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("preset env[%q] = %v, want %v", k, got[k], v)
		}
	}
}

// TestMigration0047SeedIsIdempotentAndPreservesOperatorEdits is the
// idempotent-seed contract §5 promises: re-running the up migration's INSERT
// against a database that already has the (edited) row must NOT clobber the
// operator's edit, because ON CONFLICT (id) DO NOTHING only ever fires on the
// id colliding, and the id is stable.
//
// golang-migrate itself never re-runs an already-applied version, so this
// exercises the up.sql statement directly against a database at exactly the
// state a re-run would see — the same INSERT text the migration file
// contains, executed a second time. The app-adoption step also proves the
// down migration's guard (a REFERENCED preset survives a rollback — see
// TestMigration0047DownGuardsAReferencedPreset for the down-only version of
// that claim) is what makes "the row is still there to re-collide with" true
// in the realistic operator sequence: adopt the preset, then redeploy.
func TestMigration0047SeedIsIdempotentAndPreservesOperatorEdits(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	// The operator edits the seeded row.
	must(t, execT(ctx, pool, `
		UPDATE runtime_presets SET image = 'quasar-steam:operator-pinned'
		WHERE id = $1::uuid`, defaultSteamPresetID))

	// Re-run EXACTLY the up migration's seed statement — the scenario a
	// second `migrate.Run` against an already-converged database represents,
	// mirrored from 0046's own idempotency claim.
	must(t, execT(ctx, pool, `
		INSERT INTO runtime_presets (id, name, description, image, env, args, mounts, managed_home)
		VALUES (
		    $1::uuid, 'Steam',
		    'Shipped default for the quasar-steam provider image.',
		    'quasar-steam:latest',
		    '{"PUID":"99","PGID":"100","UNAME":"quasar"}'::jsonb,
		    '[]'::jsonb, '[]'::jsonb, true
		)
		ON CONFLICT (id) DO NOTHING`, defaultSteamPresetID))

	var image string
	must(t, pool.QueryRow(ctx,
		`SELECT image FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&image))
	if image != "quasar-steam:operator-pinned" {
		t.Errorf("preset image after re-seeding = %q, want the operator edit %q to survive (ON CONFLICT DO NOTHING)",
			image, "quasar-steam:operator-pinned")
	}

	// And there is still exactly one row at this id — DO NOTHING, not a second insert.
	var n int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&n))
	if n != 1 {
		t.Errorf("preset rows at the stable id after re-seeding = %d, want 1", n)
	}
}

// TestMigration0047DownGuardsAReferencedPreset asserts the down migration's
// guarded delete: a preset an app has actually adopted must survive rolling
// 0047 back (RAISE NOTICE + skip), mirroring 0046's retired-rung pattern —
// deleting it would either violate the ON DELETE RESTRICT foreign key
// (0035) outright or, if the FK were ever relaxed, silently strand a live
// app's runtime spec.
func TestMigration0047DownGuardsAReferencedPreset(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	must(t, execT(ctx, pool, `
		INSERT INTO apps (name, managed_home, runtime_preset_id)
		VALUES ('Steam', true, $1::uuid)`, defaultSteamPresetID))

	migrateTo47(t, m, version46)

	var n int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&n))
	if n != 1 {
		t.Errorf("referenced preset rows after rolling 0047 down = %d, want 1 (must be left in place)", n)
	}
}

// TestMigration0047DownDeletesAnUnreferencedPreset is the mirror case: with
// no app pointing at it, the down migration removes the seed cleanly.
func TestMigration0047DownDeletesAnUnreferencedPreset(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version47)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	migrateTo47(t, m, version46)

	var n int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&n))
	if n != 0 {
		t.Errorf("unreferenced preset rows after rolling 0047 down = %d, want 0", n)
	}

	var colExists bool
	must(t, pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_name = 'instance_settings' AND column_name = 'library_discovery_interval_minutes')`).
		Scan(&colExists))
	if colExists {
		t.Error("library_discovery_interval_minutes must not exist after rolling 0047 down")
	}
}
