// migration_0059_db_test.go — #457: migration 0059 removes the 0047 default
// Steam runtime-preset seed ONLY when it is both unedited and unreferenced.
//
// It reuses migration_0047_db_test.go's scratch-database helpers (same package)
// for the reason that file documents: these tests step migrations to specific
// versions and must never do that to a database other suites share.
package library

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// version58 is the last migration before 0059; version59 is this one.
const (
	version58 = 58
	version59 = 59
)

// seedDescription is the EXACT description 0047 writes. 0059's "unedited" guard
// compares against it verbatim, so this constant is the test's copy of that
// contract — if the two ever disagree, the guard silently stops matching and the
// duplicate survives, which is the failure this pins.
const seedDescription = "Shipped default for the quasar-steam provider image — the container spec " +
	"Steam library discovery's provider app is expected to launch with. " +
	"Edit freely; this seed never overwrites a changed row."

// seedRowsAt59 counts the 0047 seed rows after migrating a scratch database to
// version 59, having applied mutate at version 58 (i.e. with the seed present
// and 0059 not yet run).
func seedRowsAt59(t *testing.T, mutate func(ctx context.Context, pool *pgxpool.Pool)) int {
	t.Helper()
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version58)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	// Sanity: the seed is there before 0059 runs, or the test proves nothing.
	var before int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&before))
	if before != 1 {
		t.Fatalf("0047 seed rows at version 58 = %d, want 1", before)
	}
	if mutate != nil {
		mutate(ctx, pool)
	}

	migrateTo47(t, m, version59)

	var after int
	must(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&after))
	return after
}

// TestMigration0059DeletesUneditedUnreferencedSeed — the #457 acceptance line:
// on a deployment that never touched or adopted the seed (every fresh box), 0059
// removes it, so enabling library discovery leaves exactly ONE Steam preset (the
// P5-managed one).
func TestMigration0059DeletesUneditedUnreferencedSeed(t *testing.T) {
	if got := seedRowsAt59(t, nil); got != 0 {
		t.Errorf("0047 seed rows after 0059 = %d, want 0 (unedited + unreferenced must be deleted)", got)
	}
}

// TestMigration0059KeepsAReferencedSeed — an app that actually launches through
// the seed keeps it. apps.runtime_preset_id is ON DELETE RESTRICT (0035), so an
// unguarded delete would not merely be rude, it would FAIL and crash-loop the
// control plane at boot.
func TestMigration0059KeepsAReferencedSeed(t *testing.T) {
	got := seedRowsAt59(t, func(ctx context.Context, pool *pgxpool.Pool) {
		must(t, execT(ctx, pool, `
			INSERT INTO apps (name, managed_home, runtime_preset_id)
			VALUES ('Steam', true, $1::uuid)`, defaultSteamPresetID))
	})
	if got != 1 {
		t.Errorf("referenced seed rows after 0059 = %d, want 1 (an adopted preset must survive)", got)
	}
}

// TestMigration0059KeepsASeedReferencedByAnInstalledImage — the OTHER referencing
// column (installed_images.runtime_preset_id, 0058). Its FK is SET NULL rather
// than RESTRICT, so an unguarded delete would succeed and SILENTLY unlink an
// installed image from the preset it launches with — the quieter, worse failure.
func TestMigration0059KeepsASeedReferencedByAnInstalledImage(t *testing.T) {
	got := seedRowsAt59(t, func(ctx context.Context, pool *pgxpool.Pool) {
		must(t, execT(ctx, pool, `
			INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, raw)
			VALUES ('steam', 1, 'Steam', 'prebuilt', 'v1', '{}'::jsonb)`))
		must(t, execT(ctx, pool, `
			INSERT INTO installed_images (image_id, version, runtime_preset_id)
			VALUES ('steam', 'v1', $1::uuid)`, defaultSteamPresetID))
	})
	if got != 1 {
		t.Errorf("image-linked seed rows after 0059 = %d, want 1", got)
	}
}

// TestMigration0059KeepsAnEditedSeed — 0047 promised "Edit freely; this seed
// never overwrites a changed row". Deleting an operator's edited row would break
// that promise harder than overwriting it would.
//
// ONE SUB-TEST PER EDITABLE COLUMN, and that exhaustiveness is the point (Alice
// review, PR #460): the first version of this migration compared only
// name/image/description, so an operator who had edited args, env, mounts,
// managed_home or home_container_path — a perfectly ordinary thing to do to a
// row the migration itself invited them to edit — had it DELETED. Every column
// an operator can change must appear here, or the same hole reopens the next
// time someone widens runtime_presets.
func TestMigration0059KeepsAnEditedSeed(t *testing.T) {
	edits := []struct {
		field string
		sql   string
	}{
		{"name", `UPDATE runtime_presets SET name = 'Steam (mine)' WHERE id = $1::uuid`},
		{"image", `UPDATE runtime_presets SET image = 'ghcr.io/operator/steam:pinned' WHERE id = $1::uuid`},
		{"description", `UPDATE runtime_presets SET description = 'ours now' WHERE id = $1::uuid`},
		{"args", `UPDATE runtime_presets SET args = '["-bigpicture"]'::jsonb WHERE id = $1::uuid`},
		{"env", `UPDATE runtime_presets SET env = '{"PUID":"1000","PGID":"100","UNAME":"quasar"}'::jsonb WHERE id = $1::uuid`},
		{"mounts", `UPDATE runtime_presets SET mounts = '["/games:/games"]'::jsonb WHERE id = $1::uuid`},
		{"managed_home", `UPDATE runtime_presets SET managed_home = false WHERE id = $1::uuid`},
		{"home_container_path", `UPDATE runtime_presets SET home_container_path = '/home/steam' WHERE id = $1::uuid`},
	}
	for _, e := range edits {
		t.Run(e.field, func(t *testing.T) {
			got := seedRowsAt59(t, func(ctx context.Context, pool *pgxpool.Pool) {
				must(t, execT(ctx, pool, e.sql, defaultSteamPresetID))
			})
			if got != 1 {
				t.Errorf("seed rows after 0059 with an edited %s = %d, want 1 (the operator's edit must survive)", e.field, got)
			}
		})
	}
}

// TestMigration0059KeepsAnImageManagedSeed — if something has adopted the seed
// row as a P5 managed preset (runtime_presets.managed_image_id, 0058), deleting
// it would remove a row the materializer owns and re-key on.
func TestMigration0059KeepsAnImageManagedSeed(t *testing.T) {
	got := seedRowsAt59(t, func(ctx context.Context, pool *pgxpool.Pool) {
		must(t, execT(ctx, pool, `
			INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, raw)
			VALUES ('steam', 1, 'Steam', 'prebuilt', 'v1', '{}'::jsonb)`))
		must(t, execT(ctx, pool, `
			UPDATE runtime_presets SET managed_image_id = 'steam' WHERE id = $1::uuid`, defaultSteamPresetID))
	})
	if got != 1 {
		t.Errorf("image-managed seed rows after 0059 = %d, want 1", got)
	}
}

// TestMigration0059SeedDescriptionMatchesTheMigration guards the guard: 0059's
// unedited test compares the description VERBATIM, so a drift between the text
// 0047 writes and the text 0059 matches would make the delete silently never
// fire. This asserts the shipped seed still carries exactly seedDescription.
func TestMigration0059SeedDescriptionMatchesTheMigration(t *testing.T) {
	url := scratchDB47(t)
	m := newMigrator47(t, url)
	migrateTo47(t, m, version58)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	var desc string
	must(t, pool.QueryRow(ctx,
		`SELECT description FROM runtime_presets WHERE id = $1::uuid`, defaultSteamPresetID).Scan(&desc))
	if desc != seedDescription {
		t.Errorf("0047 seed description drifted from the text 0059 matches on:\n got: %q\nwant: %q", desc, seedDescription)
	}
}
