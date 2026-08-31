// preset_db_test.go — image-management P5 acceptance: runtime-preset
// materialization on install/update, and the invariant that an admin-authored
// preset is never overwritten. TEST_DATABASE_URL-gated like every DB test here.
package images

import (
	"context"
	"encoding/json"
	"testing"
)

// seedCatalogRuntime upserts a prebuilt catalog row with a resolved digest, an
// optional library_provider, and a runtime block — the shape install/update
// materialize a managed preset from.
func seedCatalogRuntime(t *testing.T, env *actionsEnv, version, digest, provider, runtimeJSON string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, registry_digest, runtime, library_provider, raw)
		VALUES ($1, 1, 'Steam', 'prebuilt', $2, $3, $4, $5::jsonb, NULLIF($6,''), '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			version = EXCLUDED.version,
			registry_digest = EXCLUDED.registry_digest,
			runtime = EXCLUDED.runtime,
			library_provider = EXCLUDED.library_provider
	`, imgID, version, imgRef, digest, runtimeJSON, provider); err != nil {
		t.Fatalf("seed image_catalog runtime: %v", err)
	}
}

// managedPreset reads the managed runtime_presets row for imageID (if any).
type managedPresetRow struct {
	ID                string
	Name              string
	Args              string
	Env               string
	Mounts            string
	ManagedHome       bool
	HomeContainerPath string
	Network           string
}

func readManagedPreset(t *testing.T, env *actionsEnv, imageID string) (managedPresetRow, bool) {
	t.Helper()
	var p managedPresetRow
	err := env.pool.QueryRow(context.Background(), `
		SELECT id::text, name, args::text, env::text, mounts::text, managed_home, home_container_path, network
		FROM runtime_presets WHERE managed_image_id = $1`, imageID).
		Scan(&p.ID, &p.Name, &p.Args, &p.Env, &p.Mounts, &p.ManagedHome, &p.HomeContainerPath, &p.Network)
	if err != nil {
		return managedPresetRow{}, false
	}
	return p, true
}

func installedPresetLink(t *testing.T, env *actionsEnv, imageID string) *string {
	t.Helper()
	var id *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT runtime_preset_id::text FROM installed_images WHERE image_id = $1`, imageID).Scan(&id); err != nil {
		t.Fatalf("read installed_images.runtime_preset_id: %v", err)
	}
	return id
}

const steamRuntime = `{"preset_name":"Steam","gpu":true,"no_new_privileges":false,"managed_home":true,"home_container_path":"/home/quasar","args":["-foo"],"env":{"K":"V"},"mounts":["/m:/m"]}`

// TestInstallMaterializesManagedPreset — the P5 acceptance line: installing a
// runtime-bearing image materializes a managed runtime_presets row from the
// manifest mapping and links it on installed_images.runtime_preset_id.
func TestInstallMaterializesManagedPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed runtime_presets row materialized")
	}
	if p.Name != "Steam" {
		t.Errorf("preset name = %q, want Steam (preset_name)", p.Name)
	}
	if !p.ManagedHome {
		t.Errorf("preset managed_home = false, want true")
	}
	if p.HomeContainerPath != "/home/quasar" {
		t.Errorf("preset home_container_path = %q, want /home/quasar", p.HomeContainerPath)
	}
	assertJSONEq(t, "args", p.Args, `["-foo"]`)
	assertJSONEq(t, "env", p.Env, `{"K":"V"}`)
	assertJSONEq(t, "mounts", p.Mounts, `["/m:/m"]`)

	link := installedPresetLink(t, env, imgID)
	if link == nil || *link != p.ID {
		t.Errorf("installed_images.runtime_preset_id = %v, want %q", link, p.ID)
	}

	// The #432 rule: no_new_privileges is NOT a runtime_presets column; it must
	// not have leaked into the preset. runtime_presets has no such column, so the
	// only assertion possible is that materialization succeeded WITHOUT it — which
	// the successful row above proves. The mapped columns above are exhaustive.

	// Envelope surfaces the link (openapi CatalogImage.runtime_preset_id).
	img, err := env.store.ImageByID(context.Background(), imgID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if img.RuntimePresetID == nil || *img.RuntimePresetID != p.ID {
		t.Errorf("envelope runtime_preset_id = %v, want %q", img.RuntimePresetID, p.ID)
	}
}

// TestUpdateRematerializesSameManagedPreset — an update re-materializes the SAME
// managed row (keyed on managed_image_id), reflecting the new manifest runtime.
func TestUpdateRematerializesSameManagedPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	before, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after install")
	}

	// New catalog version + a changed runtime block (managed_home off, new args).
	const runtime2 = `{"preset_name":"Steam","managed_home":false,"home_container_path":"/data","args":["-bar"],"env":{},"mounts":[]}`
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", runtime2)

	applied, _, err := env.store.Update(context.Background(), imgID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !applied {
		t.Fatal("update reported applied=false; want true (version moved)")
	}

	after, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after update")
	}
	if after.ID != before.ID {
		t.Errorf("update created a NEW preset row: before=%q after=%q, want the same row", before.ID, after.ID)
	}
	if after.ManagedHome {
		t.Errorf("preset managed_home = true, want false (re-materialized)")
	}
	if after.HomeContainerPath != "/data" {
		t.Errorf("preset home_container_path = %q, want /data", after.HomeContainerPath)
	}
	assertJSONEq(t, "args", after.Args, `["-bar"]`)
}

// TestManagedPresetNeverOverwritesAdminPreset — an admin-authored preset
// (managed_image_id NULL) with a colliding name is never touched; the managed
// row is a separate row with a disambiguated name.
func TestManagedPresetNeverOverwritesAdminPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()

	// Admin preset named "Steam", hand-authored (managed_image_id NULL).
	var adminID string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO runtime_presets (name, args, managed_image_id)
		VALUES ('Steam', '["--admin"]'::jsonb, NULL) RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatalf("seed admin preset: %v", err)
	}

	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime) // preset_name "Steam"
	if _, err := env.store.Install(ctx, imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Admin row is untouched.
	var adminName, adminArgs string
	var adminManaged *string
	if err := env.pool.QueryRow(ctx,
		`SELECT name, args::text, managed_image_id FROM runtime_presets WHERE id::text = $1`, adminID).
		Scan(&adminName, &adminArgs, &adminManaged); err != nil {
		t.Fatalf("re-read admin preset: %v", err)
	}
	if adminName != "Steam" || adminManaged != nil {
		t.Errorf("admin preset mutated: name=%q managed_image_id=%v, want Steam/nil", adminName, adminManaged)
	}
	assertJSONEq(t, "admin args", adminArgs, `["--admin"]`)

	// Managed row is a DIFFERENT row, disambiguated name, linked.
	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset materialized alongside the admin one")
	}
	if p.ID == adminID {
		t.Fatal("managed preset IS the admin preset — must be a separate row")
	}
	if p.Name == "Steam" {
		t.Errorf("managed preset name = %q; must be disambiguated from the admin 'Steam'", p.Name)
	}
	link := installedPresetLink(t, env, imgID)
	if link == nil || *link != p.ID {
		t.Errorf("installed_images.runtime_preset_id = %v, want the managed row %q", link, p.ID)
	}
}

// TestInstallNoRuntimeBlockNoPreset — an image with an empty runtime block
// materializes nothing and leaves the link NULL.
func TestInstallNoRuntimeBlockNoPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "", `{}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, ok := readManagedPreset(t, env, imgID); ok {
		t.Error("materialized a preset for an image with no runtime block")
	}
	if link := installedPresetLink(t, env, imgID); link != nil {
		t.Errorf("installed_images.runtime_preset_id = %q, want NULL", *link)
	}
}

// seedCatalogRuntimeID is seedCatalogRuntime for an arbitrary image id — the
// second catalog row the name-race test needs.
func seedCatalogRuntimeID(t *testing.T, env *actionsEnv, id, version, digest, provider, runtimeJSON string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, registry_digest, runtime, library_provider, raw)
		VALUES ($1, 1, $2, 'prebuilt', $3, $4, $5, $6::jsonb, NULLIF($7,''), '{}'::jsonb)
		ON CONFLICT (id) DO UPDATE SET
			version = EXCLUDED.version,
			registry_digest = EXCLUDED.registry_digest,
			runtime = EXCLUDED.runtime,
			library_provider = EXCLUDED.library_provider
	`, id, id, version, imgRef, digest, runtimeJSON, provider); err != nil {
		t.Fatalf("seed image_catalog %q: %v", id, err)
	}
}

// TestInstallRejectsDangerousMount — a manifest runtime mount that targets the
// docker socket (or "/", or exposes "/") is rejected AT INSTALL, rolling back the
// whole install (no adoption, no preset) rather than materializing a preset that
// would launch a container escape.
func TestInstallRejectsDangerousMount(t *testing.T) {
	dangerous := []string{
		`{"preset_name":"Steam","mounts":["/var/run/docker.sock:/var/run/docker.sock"]}`,
		`{"preset_name":"Steam","mounts":["/run/docker.sock:/x"]}`,
		`{"preset_name":"Steam","mounts":["/host:/"]}`,
		`{"preset_name":"Steam","mounts":["/:/host"]}`,
		// The socket's parent directory carries the socket.
		`{"preset_name":"Steam","mounts":["/var/run:/hostrun"]}`,
		`{"preset_name":"Steam","mounts":["/run:/hostrun"]}`,
		`{"preset_name":"Steam","mounts":["/var:/hostvar"]}`,
		`{"preset_name":"Steam","mounts":["/var/lib/docker:/hostdocker"]}`,
		`{"preset_name":"Steam","mounts":["/proc/1/root:/host"]}`,
		`{"preset_name":"Steam","mounts":["/sys:/hostsys"]}`,
		`{"preset_name":"Steam","mounts":["/etc:/hostetc"]}`,
		`{"preset_name":"Steam","mounts":["/opt/../var/run/docker.sock:/s"]}`,
	}
	for _, rt := range dangerous {
		t.Run(rt, func(t *testing.T) {
			env, _ := newActionsEnv(t)
			seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", rt)

			if _, err := env.store.Install(context.Background(), imgID, false); err == nil {
				t.Fatal("Install accepted a dangerous mount; want an error that rolls back the install")
			}
			if isInstalled(t, env, imgID) {
				t.Error("install of a dangerous mount was NOT rolled back (adoption row exists)")
			}
			if _, ok := readManagedPreset(t, env, imgID); ok {
				t.Error("a managed preset was materialized for a dangerous mount")
			}
		})
	}
}

// TestInstallAcceptsSafeMount — a mount with options and an ordinary host path is
// allowed (the policy is narrow: catalog images legitimately bind host paths).
func TestInstallAcceptsSafeMount(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam",
		`{"preset_name":"Steam","mounts":["/opt/games:/games:ro"]}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install of a safe mount failed: %v", err)
	}
	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset for a safe mount")
	}
	assertJSONEq(t, "mounts", p.Mounts, `["/opt/games:/games:ro"]`)
}

// TestInstallRejectsNonStringArgs / Env — a runtime args element or env value
// that is not a string is rejected at install (would break the launch decode),
// rolling back the install.
func TestInstallRejectsNonStringArgs(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{"preset_name":"Steam","args":[1,2]}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err == nil {
		t.Fatal("Install accepted non-string args; want an error")
	}
	if isInstalled(t, env, imgID) {
		t.Error("install of non-string args was not rolled back")
	}
}

func TestInstallRejectsNonStringEnv(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{"preset_name":"Steam","env":{"K":1}}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err == nil {
		t.Fatal("Install accepted non-string env value; want an error")
	}
	if isInstalled(t, env, imgID) {
		t.Error("install of non-string env was not rolled back")
	}
}

// TestUpdateRuntimeDropNullsLink — an update whose new manifest DROPS its runtime
// block re-points installed_images.runtime_preset_id to NULL (the old managed
// preset ROW is left for admin cleanup, only the link is nulled).
func TestUpdateRuntimeDropNullsLink(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
	if _, err := env.store.Install(ctx, imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	if link := installedPresetLink(t, env, imgID); link == nil {
		t.Fatal("expected a linked preset after installing a runtime-bearing image")
	}
	before, _ := readManagedPreset(t, env, imgID)

	// New version, runtime block dropped ('{}').
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", `{}`)
	applied, _, err := env.store.Update(ctx, imgID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !applied {
		t.Fatal("update reported applied=false; want true (version moved)")
	}
	if link := installedPresetLink(t, env, imgID); link != nil {
		t.Errorf("runtime_preset_id = %q after a runtime-drop update, want NULL", *link)
	}
	// The old preset ROW is intentionally left behind for admin cleanup.
	if _, ok := readManagedPreset(t, env, imgID); !ok {
		t.Errorf("the obsolete managed preset row %q was deleted; it must be left for admin cleanup", before.ID)
	}
}

// TestManagedPresetNameRaceRetry — two concurrent installs of images whose
// preset_name collides both succeed with DISTINCT names: the name-allocation race
// is resolved via the bounded savepoint retry, never a 500 that rolls back a
// valid install.
func TestManagedPresetNameRaceRetry(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	const idA, idB = "steam", "steam-b"
	const rt = `{"preset_name":"Steam","args":["-x"],"env":{},"mounts":[]}`
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", rt)         // id "steam"
	seedCatalogRuntimeID(t, env, idB, imgVer, imgDigest2, "steam", rt) // id "steam-b"

	errs := make(chan error, 2)
	for _, id := range []string{idA, idB} {
		go func(id string) {
			_, err := env.store.Install(ctx, id, false)
			errs <- err
		}(id)
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent install failed (name race not resolved): %v", err)
		}
	}

	pa, okA := readManagedPreset(t, env, idA)
	pb, okB := readManagedPreset(t, env, idB)
	if !okA || !okB {
		t.Fatalf("expected both managed presets materialized: a=%v b=%v", okA, okB)
	}
	if pa.Name == pb.Name {
		t.Errorf("both managed presets got the same name %q; the race retry must disambiguate", pa.Name)
	}
}

func assertJSONEq(t *testing.T, field, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("%s: got is not JSON %q: %v", field, got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: want is not JSON %q: %v", field, want, err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Errorf("%s = %s, want %s", field, got, want)
	}
}

// TestInstallMaterializesPresetNetwork — §S2: a manifest runtime block that
// declares `network` materializes it onto the managed preset's `network` column.
// This is how the Steam image carries "I need the internet on first boot" (#463)
// with it to every host, instead of an operator flipping
// QUASAR_CONTAINER_NETWORK and opening the network for every app on that box.
func TestInstallMaterializesPresetNetwork(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam",
		`{"preset_name":"Steam","network":"bridge"}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset materialized")
	}
	if p.Network != "bridge" {
		t.Errorf("preset network = %q, want bridge", p.Network)
	}
}

// A runtime block with no `network` materializes "" — inherit the agent's host
// default, i.e. the hardened `none`. This is the case for every other image and
// it must stay byte-identical to pre-S2 behaviour.
func TestInstallWithoutNetworkInheritsDefault(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset materialized")
	}
	if p.Network != "" {
		t.Errorf(`preset network = %q, want "" (inherit)`, p.Network)
	}
}

// TestInstallRejectsUnknownNetwork — an out-of-set network fails the INSTALL and
// rolls it back, exactly like a dangerous mount. The manifest is cached verbatim
// at sync and never validated there, so this is the one place a bad value is
// caught before it can become `docker run --network <value>` on a host.
func TestInstallRejectsUnknownNetwork(t *testing.T) {
	// `host` leads the list ON PURPOSE (review, Alice round 2 on PR #464). It is
	// the case that matters most here: a manifest is authored on another machine,
	// fetched over the network, and installed by an admin approving an APP — so a
	// manifest that could declare `host` would remove the container's network
	// namespace on every host that installs it, exposing host loopback services
	// and letting the app bind host ports. This is the same class of rule as the
	// docker-socket mount rejection above, and strictly the more portable risk.
	for _, rt := range []string{
		`{"preset_name":"Steam","network":"host"}`,
		`{"preset_name":"Steam","network":"container:quasar-control"}`,
		`{"preset_name":"Steam","network":"my-net"}`,
		`{"preset_name":"Steam","network":"Bridge"}`,
	} {
		t.Run(rt, func(t *testing.T) {
			env, _ := newActionsEnv(t)
			seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", rt)
			if _, err := env.store.Install(context.Background(), imgID, false); err == nil {
				t.Fatal("Install accepted an unknown network; want an error that rolls back the install")
			}
			if isInstalled(t, env, imgID) {
				t.Error("install with a bad network was NOT rolled back (adoption row exists)")
			}
			if _, ok := readManagedPreset(t, env, imgID); ok {
				t.Error("a managed preset was materialized despite a bad network")
			}
		})
	}
}

// TestUpdateClearsDroppedNetwork — re-materialization must be able to CLEAR the
// network too: an image whose new manifest drops the field reverts its managed
// preset to "" (inherit) rather than keeping a stale `bridge` forever.
func TestUpdateClearsDroppedNetwork(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam",
		`{"preset_name":"Steam","network":"bridge"}`)
	if _, err := env.store.Install(context.Background(), imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", `{"preset_name":"Steam"}`)
	if applied, _, err := env.store.Update(context.Background(), imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}
	p, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after update")
	}
	if p.Network != "" {
		t.Errorf(`preset network = %q after the manifest dropped it, want "" (inherit)`, p.Network)
	}
}

// steamManifestAtVersion renders a one-image manifest document naming imgID at
// version, with runtimeJSON as its raw runtime block — the shape a
// fixtureFetcher-backed Store.Sync needs to exercise the real fetch→validate→
// upsert path (not a direct SQL seed) for #470's sync-time reconciliation.
func steamManifestAtVersion(version, runtimeJSON string) string {
	return `{
		"manifest_version": 1,
		"images": [
			{"id":"` + imgID + `","display_name":"Steam","kind":"prebuilt","version":"` + version + `",
			 "registry_ref":"` + imgRef + `","runtime":` + runtimeJSON + `}
		]
	}`
}

// TestSyncRematerializesManagedPresetOnSameVersionRuntimeChange — #470: a
// manifest runtime-block change at an UNCHANGED catalog version must NOT stay
// cosmetic for an already-INSTALLED image. Before this fix, Sync only wrote
// image_catalog and the managed preset (and hence a launching session's
// container config, e.g. network) kept the stale block until an
// uninstall+reinstall.
func TestSyncRematerializesManagedPresetOnSameVersionRuntimeChange(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()

	// Install at V1 with no network declared (inherits "").
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{"preset_name":"Steam","args":["-a"]}`)
	if _, err := env.store.Install(ctx, imgID, false); err != nil {
		t.Fatalf("install: %v", err)
	}
	before, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after install")
	}
	if before.Network != "" {
		t.Fatalf("preset network = %q before the drift, want \"\" (inherit)", before.Network)
	}
	linkBefore := installedPresetLink(t, env, imgID)
	if linkBefore == nil {
		t.Fatal("no runtime_preset_id link after install")
	}

	// A catalog SYNC (not an admin Update/Install) at the SAME version, whose
	// manifest now declares network=bridge — the #470 steam repro (network
	// gained at the same version, Steam kept dying offline).
	manifest := steamManifestAtVersion(imgVer, `{"preset_name":"Steam","args":["-a"],"network":"bridge"}`)
	syncStore := NewStoreWithFetcher(env.pool, fixtureFetcher{data: []byte(manifest)})
	syncStore.SetLogger(testLog())
	syncEnv, err := syncStore.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if syncEnv.SyncError != nil {
		t.Fatalf("sync_error: got %q, want nil", *syncEnv.SyncError)
	}

	// The catalog's version must genuinely be unchanged — otherwise this test
	// would be exercising Update's version-bump path, not sync's same-version
	// path #470 is about.
	img, err := syncStore.ImageByID(ctx, imgID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if img.Version != imgVer {
		t.Fatalf("catalog version moved to %q; want it to stay %q (same-version drift only)", img.Version, imgVer)
	}

	after, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset after sync")
	}
	if after.ID != before.ID {
		t.Errorf("sync created a NEW preset row: before=%q after=%q, want the SAME managed row re-materialized", before.ID, after.ID)
	}
	if after.Network != "bridge" {
		t.Errorf("preset network = %q after a same-version runtime-block sync, want bridge (re-materialized, not stale)", after.Network)
	}
	linkAfter := installedPresetLink(t, env, imgID)
	if linkAfter == nil || *linkAfter != after.ID {
		t.Errorf("runtime_preset_id = %v after sync, want %q", linkAfter, after.ID)
	}
}

// TestSyncOnNonInstalledImageStaysCosmetic — the control case #470 must not
// regress: a runtime-block change for an image that is NOT installed has no
// managed preset to repair, and must not create one out of nowhere. Only
// image_catalog moves.
func TestSyncOnNonInstalledImageStaysCosmetic(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()

	// Seed the catalog row directly (no Install) so imgID exists in
	// image_catalog but has no installed_images row at all.
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{"preset_name":"Steam"}`)
	if isInstalled(t, env, imgID) {
		t.Fatal("test setup: image must not be installed")
	}

	manifest := steamManifestAtVersion(imgVer, `{"preset_name":"Steam","network":"bridge"}`)
	syncStore := NewStoreWithFetcher(env.pool, fixtureFetcher{data: []byte(manifest)})
	syncStore.SetLogger(testLog())
	syncEnv, err := syncStore.Sync(ctx)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if syncEnv.SyncError != nil {
		t.Fatalf("sync_error: got %q, want nil", *syncEnv.SyncError)
	}

	if isInstalled(t, env, imgID) {
		t.Error("a sync on a non-installed image's runtime block installed it")
	}
	if _, ok := readManagedPreset(t, env, imgID); ok {
		t.Error("a managed preset was materialized for a non-installed image; the change must stay cosmetic")
	}

	img, err := syncStore.ImageByID(ctx, imgID)
	if err != nil {
		t.Fatalf("ImageByID: %v", err)
	}
	if img.RuntimePresetID != nil {
		t.Errorf("runtime_preset_id = %v for a non-installed image, want nil", img.RuntimePresetID)
	}
}
