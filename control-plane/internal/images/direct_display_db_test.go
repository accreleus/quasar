// direct_display_db_test.go — #580: an app that existed before its image's
// manifest declared direct_display gains the key when the adoption moves to a
// manifest that does. TEST_DATABASE_URL-gated.
package images

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

const (
	plainSteamRuntime  = `{"preset_name":"Steam","gpu":true,"no_new_privileges":false}`
	directSteamRuntime = `{"preset_name":"Steam","gpu":true,"no_new_privileges":false,"direct_display":true}`
)

// seedApp inserts an operator-made app; presetID "" means no preset.
func seedApp(t *testing.T, env *actionsEnv, name, presetID, spec string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind, runtime_preset_id, runtime_spec)
		VALUES ($1, 'desktop', NULLIF($2, '')::uuid, $3::jsonb) RETURNING id::text`,
		name, presetID, spec).Scan(&id); err != nil {
		t.Fatalf("seed app %s: %v", name, err)
	}
	return id
}

// directDisplay reads an app's runtime_spec.direct_display; nil when absent.
func directDisplay(t *testing.T, env *actionsEnv, appID string) any {
	t.Helper()
	var raw []byte
	if err := env.pool.QueryRow(context.Background(),
		`SELECT runtime_spec FROM apps WHERE id::text = $1`, appID).Scan(&raw); err != nil {
		t.Fatalf("read app %s: %v", appID, err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", raw, err)
	}
	return spec["direct_display"]
}

// directFixture is an install whose apps all predate the manifest's key: the
// provider app and an operator's app on the managed preset, plus the three
// rows the stamp must leave alone.
type directFixture struct {
	provider, onPreset, refused, repointed, ownImage, ownImageOnPreset string
}

func seedDirectFixture(t *testing.T, env *actionsEnv) directFixture {
	t.Helper()
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", plainSteamRuntime)
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok || app.PresetID == nil {
		t.Fatalf("provider app not created on the managed preset: %+v", app)
	}
	var ownPreset string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO runtime_presets (name, image) VALUES ('Operator Steam', 'ghcr.io/operator/steam:own')
		RETURNING id::text`).Scan(&ownPreset); err != nil {
		t.Fatalf("create operator preset: %v", err)
	}
	f := directFixture{
		provider:  app.ID,
		onPreset:  seedApp(t, env, "On the managed preset", *app.PresetID, `{"gpu":true}`),
		refused:   seedApp(t, env, "Operator said no", *app.PresetID, `{"gpu":true,"direct_display":false}`),
		repointed: seedApp(t, env, "Operator preset", ownPreset, `{"gpu":true}`),
		ownImage:  seedApp(t, env, "Own image", "", `{"image":"ghcr.io/operator/steam:own"}`),
		// Linked to the managed preset, but it launches its own image (#583).
		ownImageOnPreset: seedApp(t, env, "Own image on the managed preset", *app.PresetID, `{"image":"ghcr.io/operator/steam:own"}`),
	}
	for name, id := range map[string]string{"provider": f.provider, "on preset": f.onPreset} {
		if got := directDisplay(t, env, id); got != nil {
			t.Fatalf("%s app has direct_display %v before the manifest declares it", name, got)
		}
	}
	return f
}

func (f directFixture) assertStamped(t *testing.T, env *actionsEnv) {
	t.Helper()
	for _, tt := range []struct {
		name, id string
		want     any
	}{
		{"the provider app", f.provider, true},
		{"an operator's app on the managed preset", f.onPreset, true},
		{"an app whose operator set false", f.refused, false},
		{"an app on the operator's own preset", f.repointed, nil},
		{"an app on its own image", f.ownImage, nil},
		{"an app on the managed preset that sets its own image", f.ownImageOnPreset, nil},
	} {
		if got := directDisplay(t, env, tt.id); got != tt.want {
			t.Errorf("%s: direct_display = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestUpdateStampsDirectDisplayOnAppsOfTheManagedPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	f := seedDirectFixture(t, env)

	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", directSteamRuntime)
	// The catalog is ahead of the adoption: nothing runs the declaring image yet.
	if err := stampDirectDisplay(context.Background(), env.pool, testLog(), imgID); err != nil {
		t.Fatalf("stampDirectDisplay: %v", err)
	}
	if got := directDisplay(t, env, f.provider); got != nil {
		t.Fatalf("provider app stamped %v from a catalog version it has not adopted", got)
	}

	if applied, _, err := env.store.Update(context.Background(), imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}
	f.assertStamped(t, env)
}

// The manifest can gain the key at an unchanged version, which no Update sees.
func TestSyncStampsDirectDisplayOnASameVersionRuntimeChange(t *testing.T) {
	env, _ := newActionsEnv(t)
	f := seedDirectFixture(t, env)

	syncStore := NewStoreWithFetcher(env.pool, fixtureFetcher{data: []byte(steamManifestAtVersion(imgVer, directSteamRuntime))})
	syncStore.SetLogger(testLog())
	if res, err := syncStore.Sync(context.Background()); err != nil || res.SyncError != nil {
		t.Fatalf("sync: err=%v sync_error=%v", err, res.SyncError)
	}
	f.assertStamped(t, env)
}

// Uninstall leaves the managed preset and its apps; the reinstall reuses it.
func TestReinstallStampsDirectDisplay(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	f := seedDirectFixture(t, env)

	if err := env.store.Uninstall(ctx, imgID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", directSteamRuntime)
	if _, err := env.store.Install(ctx, imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	f.assertStamped(t, env)
}

// #583: a preset gained in the SAME Update is seen by the stamp. The image had
// no runtime block, so Install materialized no preset and the provider app
// launched its own image; the update's re-adoption materializes the preset and
// migrateProviderAppOnUpdate re-links the app onto it. The stamp runs after that
// re-link, in the same transaction, so that app gains direct_display without a
// second pass (the actions.go comment claims this; this test holds it).
func TestUpdateStampsDirectDisplayWhenTheUpdateGainsThePreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)

	// No runtime block: no managed preset, and the provider app carries the
	// adopted ref in runtime_spec.image (providerRuntimeSpec's needsImage case).
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{}`)
	if _, err := env.store.Install(ctx, imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok || app.PresetID != nil {
		t.Fatalf("provider app = %+v, want one outside any preset", app)
	}
	var before map[string]any
	if err := json.Unmarshal(app.RuntimeSpec, &before); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", app.RuntimeSpec, err)
	}
	if before["image"] != imgDigest || before["direct_display"] != nil {
		t.Fatalf("before the update: runtime_spec = %v, want the adopted ref and no direct_display", before)
	}

	// The catalog gains a runtime block that declares direct_display; Update
	// re-adopts, materializes the preset and re-links the app in one transaction.
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", directSteamRuntime)
	var buf bytes.Buffer
	env.store.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}
	preset, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after the update gained one")
	}
	var after providerAppRow
	if err := env.pool.QueryRow(ctx, `
		SELECT runtime_preset_id::text, runtime_spec FROM apps WHERE id::text = $1`, app.ID).
		Scan(&after.PresetID, &after.RuntimeSpec); err != nil {
		t.Fatalf("read provider app: %v", err)
	}
	if after.PresetID == nil || *after.PresetID != preset.ID {
		t.Fatalf("provider app preset = %v, want the gained preset %q", after.PresetID, preset.ID)
	}
	if got := directDisplay(t, env, app.ID); got != true {
		t.Errorf("provider app: direct_display = %v, want true", got)
	}
	var spec map[string]any
	if err := json.Unmarshal(after.RuntimeSpec, &spec); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", after.RuntimeSpec, err)
	}
	if _, hasImage := spec["image"]; hasImage {
		t.Errorf("provider app still sets runtime_spec.image %v; it would override the gained preset", spec["image"])
	}
	if log := buf.String(); !strings.Contains(log, "stamped direct_display") || !strings.Contains(log, "apps=1") {
		t.Errorf("want one log line counting the 1 stamped app, got:\n%s", log)
	}
}

// #583: a state that converged without an adoption move is stamped by the sync
// itself, once for the whole catalog, and the rewrite leaves one log line with
// the row count. The runtime block is unchanged across the sync, so
// reconcileRuntimeDrift stamps nothing: only the unscoped pass can.
func TestSyncStampsDirectDisplayWithoutAnAdoptionMove(t *testing.T) {
	env, _ := newActionsEnv(t)
	f := seedDirectFixture(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", directSteamRuntime)

	var buf bytes.Buffer
	syncStore := NewStoreWithFetcher(env.pool, fixtureFetcher{data: []byte(steamManifestAtVersion(imgVer, directSteamRuntime))})
	syncStore.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	if res, err := syncStore.Sync(context.Background()); err != nil || res.SyncError != nil {
		t.Fatalf("sync: err=%v sync_error=%v", err, res.SyncError)
	}
	f.assertStamped(t, env)
	if log := buf.String(); !strings.Contains(log, "stamped direct_display") || !strings.Contains(log, "apps=2") {
		t.Errorf("want one log line counting the 2 stamped apps, got:\n%s", log)
	}
}
