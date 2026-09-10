// launch_profile_db_test.go — #171: an app created or edited in the console
// against an IMAGE-MANAGED runtime preset carries that image's declared launch
// profile, whatever the request said. The live shape that failed: the editor
// sent `gpu:false` for a KDE app on the managed "KDE Desktop" preset and the
// image refused to start on a software Vulkan renderer.
package crud

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// kdeRuntime is the kde-desktop catalog entry's runtime block as synced on the
// gpu-test host when #171 was found.
const kdeRuntime = `{"env": {}, "gpu": true, "args": [], "mounts": [], "network": "bridge",
	"preset_name": "KDE Desktop", "managed_home": true, "no_new_privileges": false,
	"home_container_path": "/home/quasar", "systempaths_unconfined": true}`

// consoleSpec is byte-for-byte what the admin app editor sends for a new app.
const consoleSpec = `{"env": {}, "gpu": false, "args": [], "image": "", "mounts": []}`

// seedCatalogImageWithRuntime upserts an image_catalog row. testDB does not
// clear image_catalog between tests and ids are a TEXT primary key, so every
// test here uses its own id AND the insert tolerates a repeat.
func seedCatalogImageWithRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool, imageID, runtime string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, raw, runtime)
		VALUES ($1, 1, $1, 'prebuilt', '1.0', '{}'::jsonb, $2::jsonb)
		ON CONFLICT (id) DO UPDATE SET runtime = EXCLUDED.runtime
	`, imageID, runtime); err != nil {
		t.Fatalf("seed image_catalog row %q: %v", imageID, err)
	}
}

func storedSpec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, appID string) map[string]any {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT runtime_spec FROM apps WHERE id::text = $1`, appID).Scan(&raw); err != nil {
		t.Fatalf("read runtime_spec: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", raw, err)
	}
	return m
}

func wantKDEProfile(t *testing.T, spec map[string]any, where string) {
	t.Helper()
	if spec["gpu"] != true {
		t.Errorf("%s: gpu = %v, want true (the image's preflight refuses a software renderer)", where, spec["gpu"])
	}
	if spec["no_new_privileges"] != false {
		t.Errorf("%s: no_new_privileges = %v, want false", where, spec["no_new_privileges"])
	}
	if spec["systempaths_unconfined"] != true {
		t.Errorf("%s: systempaths_unconfined = %v, want true", where, spec["systempaths_unconfined"])
	}
}

func createPreset(t *testing.T, srvURL, bearer, name string) string {
	t.Helper()
	resp, body := post(t, srvURL+"/v1/admin/runtime-presets", map[string]any{"name": name}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset %q: want 201, got %d (%v)", name, resp.StatusCode, body)
	}
	return body["runtime_preset"].(map[string]any)["id"].(string)
}

// managedKDEPreset seeds a catalog image with the KDE runtime block under
// imageID and returns a preset managed by it.
func managedKDEPreset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srvURL, bearer, imageID string) string {
	t.Helper()
	seedCatalogImageWithRuntime(t, ctx, pool, imageID, kdeRuntime)
	presetID := createPreset(t, srvURL, bearer, "KDE Desktop "+imageID)
	markPresetManaged(t, ctx, pool, presetID, imageID)
	return presetID
}

// TestConsoleAppOnManagedPresetCarriesImageLaunchProfile is THE #171 regression.
func TestConsoleAppOnManagedPresetCarriesImageLaunchProfile(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@171.test", "admin171")
	presetID := managedKDEPreset(t, ctx, pool, srv.URL, bearer, "kde-171-create")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":              "KDE",
		"kind":              "desktop",
		"runtime_preset_id": presetID,
		"runtime_spec":      json.RawMessage(consoleSpec),
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create KDE app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	got := storedSpec(t, ctx, pool, appID)
	wantKDEProfile(t, got, "after create")
	// The app's own keys survive the stamp.
	if _, ok := got["env"]; !ok {
		t.Errorf("after create: env dropped: %v", got)
	}

	// The console re-sends gpu:false on every save. The stamp runs on every
	// spec write, so a later PATCH cannot undo it.
	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"runtime_spec": json.RawMessage(`{"env":{"KDE_DEBUG":"1"},"gpu":false,"args":[],"image":"","mounts":[]}`),
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch spec: want 200, got %d (%v)", resp.StatusCode, body)
	}
	got = storedSpec(t, ctx, pool, appID)
	wantKDEProfile(t, got, "after spec patch")
	if got["env"].(map[string]any)["KDE_DEBUG"] != "1" {
		t.Errorf("after spec patch: the app's env edit was lost: %v", got)
	}
}

// TestMovingAnAppOntoAManagedPresetStampsItsStoredSpec: a PATCH that changes
// only runtime_preset_id re-evaluates the STORED spec against the new preset.
func TestMovingAnAppOntoAManagedPresetStampsItsStoredSpec(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@171move.test", "admin171move")
	managed := managedKDEPreset(t, ctx, pool, srv.URL, bearer, "kde-171-move")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":         "Loose",
		"runtime_spec": json.RawMessage(consoleSpec),
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)
	if got := storedSpec(t, ctx, pool, appID); got["gpu"] != false {
		t.Fatalf("precondition: a no-preset app keeps what it was sent; gpu = %v", got["gpu"])
	}

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"runtime_preset_id": managed}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch preset: %d (%v)", resp.StatusCode, body)
	}
	wantKDEProfile(t, storedSpec(t, ctx, pool, appID), "after moving onto the managed preset")
}

// TestPlainPresetAndNoPresetLeaveTheSpecAlone: the stamp is scoped to
// IMAGE-MANAGED presets. An admin's hand-made preset and a preset-less app
// store exactly what was sent — including gpu:false — so nothing about those
// apps' launches changes.
func TestPlainPresetAndNoPresetLeaveTheSpecAlone(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@171plain.test", "admin171plain")

	plain := createPreset(t, srv.URL, bearer, "hand-made")

	for _, tc := range []struct {
		name   string
		preset *string
	}{
		{"plain preset", &plain},
		{"no preset", nil},
	} {
		req := map[string]any{"name": "app-" + tc.name, "runtime_spec": json.RawMessage(consoleSpec)}
		if tc.preset != nil {
			req["runtime_preset_id"] = *tc.preset
		}
		resp, body := post(t, srv.URL+"/v1/apps", req, bearer)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: create: %d (%v)", tc.name, resp.StatusCode, body)
		}
		appID := body["app"].(map[string]any)["id"].(string)
		got := storedSpec(t, ctx, pool, appID)
		if got["gpu"] != false {
			t.Errorf("%s: gpu = %v, want the false that was sent", tc.name, got["gpu"])
		}
		for _, k := range []string{"no_new_privileges", "systempaths_unconfined"} {
			if _, present := got[k]; present {
				t.Errorf("%s: %s written though no image declares it: %v", tc.name, k, got)
			}
		}
	}
}

// TestDerivedTileIsNeverStamped: a tile carries identity only; its spec must
// stay {} (apps_derived_shape_ck) and it launches its parent's stamped spec.
// The parent is an ordinary console app on the managed preset — a parent need
// not be a provider app (validParentApp), and using one keeps this test clear
// of the library-discovery gate.
func TestDerivedTileIsNeverStamped(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@171tile.test", "admin171tile")
	managed := managedKDEPreset(t, ctx, pool, srv.URL, bearer, "kde-171-tile")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":              "Parent",
		"runtime_preset_id": managed,
		"runtime_spec":      json.RawMessage(`{"gpu":false}`),
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create parent: %d (%v)", resp.StatusCode, body)
	}
	parentID := body["app"].(map[string]any)["id"].(string)
	wantKDEProfile(t, storedSpec(t, ctx, pool, parentID), "parent")

	resp, body = post(t, srv.URL+"/v1/apps", map[string]any{
		"name":            "A Tile",
		"parent_app_id":   parentID,
		"external_source": "steam",
		"external_id":     "480",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tile: %d (%v)", resp.StatusCode, body)
	}
	tileID := body["app"].(map[string]any)["id"].(string)
	if got := storedSpec(t, ctx, pool, tileID); len(got) != 0 {
		t.Errorf("tile runtime_spec = %v, want {}", got)
	}
}

// TestProviderAppIsLeftToTheProvider: a library-provider app carries the values
// the provider copied at install (images.providerRuntimeSpec) against the image
// version it adopted. Neither create nor a later admin edit re-stamps it from
// the live catalog — the same line migration 0082 draws.
func TestProviderAppIsLeftToTheProvider(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "admin@171prov.test", "admin171prov")
	setLibraryDiscovery(t, pool, true)
	managed := managedKDEPreset(t, ctx, pool, srv.URL, bearer, "kde-171-provider")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":              "Provider",
		"kind":              "launcher",
		"library_provider":  "steam",
		"runtime_preset_id": managed,
		"runtime_spec":      json.RawMessage(`{"gpu":false}`),
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create provider app: %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)
	if got := storedSpec(t, ctx, pool, appID); got["gpu"] != false {
		t.Errorf("create: provider app was stamped (gpu = %v); the provider owns these values", got["gpu"])
	}

	resp, body = patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{
		"runtime_spec": json.RawMessage(`{"gpu":false,"env":{"X":"1"}}`),
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch provider app: %d (%v)", resp.StatusCode, body)
	}
	if got := storedSpec(t, ctx, pool, appID); got["gpu"] != false {
		t.Errorf("patch: provider app was stamped (gpu = %v)", got["gpu"])
	}
}
