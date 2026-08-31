package session

// DB-backed UI-P3 runtime-preset tests: they exercise the merge where it
// actually happens — GetLaunchApp, the single app-resolution step shared by
// LaunchByProfile, swapper.Swap and LaunchConsoleSession. Skipped without
// TEST_DATABASE_URL; all tests share one DB and truncate in setup (-p 1).

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func insertPreset(t *testing.T, pool *pgxpool.Pool, name, image, args, env, mounts string, managedHome bool, homePath string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO runtime_presets (name, image, args, env, mounts, managed_home, home_container_path)
		VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, $6, $7)
		RETURNING id::text`,
		name, image, args, env, mounts, managedHome, homePath).Scan(&id))
	return id
}

func insertAppWithSpec(t *testing.T, pool *pgxpool.Pool, name, spec string, presetID *string, managedHome bool, homePath string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, runtime_spec, runtime_preset_id, managed_home, home_container_path)
		VALUES ($1, $2::jsonb, $3, $4, $5) RETURNING id::text`,
		name, spec, presetID, managedHome, homePath).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

// rawRuntimeSpec reads apps.runtime_spec straight out of the column — exactly
// what GetLaunchApp returned before UI-P3 existed (its old SELECT was a plain
// `SELECT runtime_spec FROM apps`, with no join and no post-processing).
func rawRuntimeSpec(t *testing.T, pool *pgxpool.Pool, appID string) []byte {
	t.Helper()
	var raw json.RawMessage
	must(t, pool.QueryRow(context.Background(),
		`SELECT runtime_spec FROM apps WHERE id::text = $1`, appID).Scan(&raw))
	return raw
}

// TestRuntimeSpecUnchangedWithoutPreset is THE regression that matters for
// UI-P3: an app with no runtime preset must dispatch a runtime_spec
// BYTE-IDENTICAL to what it dispatched before runtime presets existed.
//
// It asserts that directly rather than by eyeballing a shape: the bytes
// GetLaunchApp hands to the dispatch path must equal the raw JSONB column bytes,
// with no decode/re-encode round trip in between (a round trip would reorder
// keys and renormalize numbers — still semantically equal, but not byte-equal,
// and the whole promise here is "nothing about this app's launch changed").
// The managed-home fields must likewise pass through untouched.
func TestRuntimeSpecUnchangedWithoutPreset(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)

	// A deliberately awkward spec: several keys, nested structure, an unknown
	// future key, numbers, and an env map — everything a re-encode would disturb.
	const spec = `{"image":"quasar-agent-dev:latest","args":["weston-terminal","--x"],` +
		`"env":{"ZED":"1","ALPHA":"2"},"mounts":["/data/a:/a","/data/b:/b:ro"],` +
		`"gpu":true,"future":{"n":3,"list":[1,2,3]}}`
	appID := insertAppWithSpec(t, pool, "no-preset-app", spec, nil, true, "/home/custom")

	want := rawRuntimeSpec(t, pool, appID)

	app, err := store.GetLaunchApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}

	if !bytes.Equal(want, app.RuntimeSpec) {
		t.Fatalf("runtime_spec is NOT byte-identical for an app with no preset\n"+
			" got: %s\nwant: %s", app.RuntimeSpec, want)
	}
	if app.RuntimePresetID != nil {
		t.Fatalf("runtime_preset_id: want nil, got %q", *app.RuntimePresetID)
	}
	if !app.ManagedHome || app.HomeContainerPath != "/home/custom" {
		t.Fatalf("managed-home fields changed: managed=%v path=%q", app.ManagedHome, app.HomeContainerPath)
	}
}

// The same guarantee for the most common shape of all: the schema default '{}'.
func TestRuntimeSpecUnchangedWithoutPresetEmptySpec(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)

	appID := insertAppWithSpec(t, pool, "empty-spec-app", `{}`, nil, false, defaultHomeContainerPath)
	want := rawRuntimeSpec(t, pool, appID)

	app, err := store.GetLaunchApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}
	if !bytes.Equal(want, app.RuntimeSpec) {
		t.Fatalf("empty runtime_spec is not byte-identical: got %s want %s", app.RuntimeSpec, want)
	}
}

// End-to-end through GetLaunchApp: every merge rule applied to real rows, with
// the conflicting env key and the duplicate mount both present.
func TestGetLaunchAppMergesPreset(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)

	presetID := insertPreset(t, pool, "steam-proton", "ghcr.io/quasar/steam:latest",
		`["--preset-arg"]`,
		`{"LOG_LEVEL":"info","PROTON_VERSION":"9.0"}`,
		`["/data/steam-cache:/shared"]`,
		false, defaultHomeContainerPath)

	appID := insertAppWithSpec(t, pool, "steam-app",
		`{"image":"","args":["--app-arg"],"env":{"LOG_LEVEL":"debug","APP_ONLY":"1"},`+
			`"mounts":["/data/app:/shared"],"gpu":true}`,
		&presetID, false, defaultHomeContainerPath)

	app, err := store.GetLaunchApp(context.Background(), appID)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}
	if app.RuntimePresetID == nil || *app.RuntimePresetID != presetID {
		t.Fatalf("runtime_preset_id not carried: %v", app.RuntimePresetID)
	}

	var got map[string]any
	if err := json.Unmarshal(app.RuntimeSpec, &got); err != nil {
		t.Fatalf("merged spec is not valid JSON (%s): %v", app.RuntimeSpec, err)
	}

	if got["image"] != "ghcr.io/quasar/steam:latest" {
		t.Fatalf("image: blank app image must inherit the preset's, got %v", got["image"])
	}
	if !equalPresetStrings(got["args"], []string{"--preset-arg", "--app-arg"}) {
		t.Fatalf("args: want preset-first append, got %v", got["args"])
	}
	// Conflicting env key resolves to the APP's value; non-conflicting keys survive.
	env, _ := got["env"].(map[string]any)
	if env["LOG_LEVEL"] != "debug" {
		t.Fatalf("env LOG_LEVEL: app value must win, got %v", env["LOG_LEVEL"])
	}
	if env["PROTON_VERSION"] != "9.0" || env["APP_ONLY"] != "1" {
		t.Fatalf("env lost a key: %v", env)
	}
	// Duplicate container path: BOTH mounts appear, preset first, no dedupe.
	if !equalPresetStrings(got["mounts"], []string{"/data/steam-cache:/shared", "/data/app:/shared"}) {
		t.Fatalf("mounts: want both entries on /shared, preset first, got %v", got["mounts"])
	}
	if got["gpu"] != true {
		t.Fatalf("unrelated key gpu was dropped: %v", got)
	}
}

// The whole reason the merge is at launch and not at save: editing the preset
// must change the NEXT LAUNCH of every app using it, with no app edit.
func TestPresetEditPropagatesToNextLaunch(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	presetID := insertPreset(t, pool, "bench", "quasar-dev:v1", `[]`, `{"MODE":"bench"}`, `[]`,
		false, defaultHomeContainerPath)
	appID := insertAppWithSpec(t, pool, "bench-app", `{}`, &presetID, false, defaultHomeContainerPath)

	app, err := store.GetLaunchApp(ctx, appID)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}
	var before map[string]any
	json.Unmarshal(app.RuntimeSpec, &before) //nolint:errcheck
	if before["image"] != "quasar-dev:v1" {
		t.Fatalf("before: image %v", before["image"])
	}

	// Edit the PRESET only — the app row is not touched.
	if _, err := pool.Exec(ctx,
		`UPDATE runtime_presets SET image = 'quasar-dev:v2' WHERE id::text = $1`, presetID); err != nil {
		t.Fatalf("edit preset: %v", err)
	}

	app, err = store.GetLaunchApp(ctx, appID)
	if err != nil {
		t.Fatalf("GetLaunchApp after edit: %v", err)
	}
	var after map[string]any
	json.Unmarshal(app.RuntimeSpec, &after) //nolint:errcheck
	if after["image"] != "quasar-dev:v2" {
		t.Fatalf("a preset edit did not reach the next launch: image %v", after["image"])
	}
}

// The storage half, end to end: a preset that provisions a managed home makes an
// app that opted out of one inherit it, at the preset's container path.
func TestGetLaunchAppMergesManagedHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	presetID := insertPreset(t, pool, "home-preset", "img:1", `[]`, `{}`, `[]`, true, "/srv/home")
	appID := insertAppWithSpec(t, pool, "home-app", `{}`, &presetID, false, defaultHomeContainerPath)

	app, err := store.GetLaunchApp(ctx, appID)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}
	if !app.ManagedHome {
		t.Fatalf("managed_home: preset's default must apply, got false")
	}
	if app.HomeContainerPath != "/srv/home" {
		t.Fatalf("home_container_path: want the preset's /srv/home, got %q", app.HomeContainerPath)
	}

	// An app with its own non-default path keeps it.
	appID2 := insertAppWithSpec(t, pool, "home-app-2", `{}`, &presetID, true, "/opt/quasar-home")
	app2, err := store.GetLaunchApp(ctx, appID2)
	if err != nil {
		t.Fatalf("GetLaunchApp: %v", err)
	}
	if app2.HomeContainerPath != "/opt/quasar-home" {
		t.Fatalf("app override lost: %q", app2.HomeContainerPath)
	}
}

func equalPresetStrings(got any, want []string) bool {
	list, ok := got.([]any)
	if !ok || len(list) != len(want) {
		return false
	}
	for i, v := range list {
		if v != want[i] {
			return false
		}
	}
	return true
}
