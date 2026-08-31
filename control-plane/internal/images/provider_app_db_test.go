// provider_app_db_test.go — #456 acceptance: enabling a library provider
// creates the provider APP, not just the image and the preset; the create is
// idempotent and never touches an operator's edits; and the disable/enable pair
// is a reversible enabled-flag flip, never a delete. TEST_DATABASE_URL-gated.
package images

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// providerAppRow is the slice of the apps row these tests assert on.
type providerAppRow struct {
	ID          string
	Name        string
	Description string
	Kind        string
	Origin      string
	Enabled     bool
	Suspended   bool
	ManagedHome bool
	CoverURL    *string
	PresetID    *string
	RuntimeSpec []byte
}

// clearApps empties apps before a provider-app test. ensureDB truncates the P2/P3
// image tables but NOT apps (no FK relates them), and every one of these tests
// asserts on "the provider app" in the singular.
func clearApps(t *testing.T, env *actionsEnv) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `TRUNCATE apps CASCADE`); err != nil {
		t.Fatalf("truncate apps: %v", err)
	}
}

func readProviderApp(t *testing.T, env *actionsEnv, provider string) (providerAppRow, bool) {
	t.Helper()
	var a providerAppRow
	err := env.pool.QueryRow(context.Background(), `
		SELECT id::text, name, description, kind, origin, enabled, library_discovery_suspended, managed_home,
		       cover_url, runtime_preset_id::text, runtime_spec
		  FROM apps WHERE library_provider = $1`, provider).
		Scan(&a.ID, &a.Name, &a.Description, &a.Kind, &a.Origin, &a.Enabled, &a.Suspended, &a.ManagedHome,
			&a.CoverURL, &a.PresetID, &a.RuntimeSpec)
	if err != nil {
		return providerAppRow{}, false
	}
	return a, true
}

func countProviderApps(t *testing.T, env *actionsEnv, provider string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM apps WHERE library_provider = $1`, provider).Scan(&n); err != nil {
		t.Fatalf("count provider apps: %v", err)
	}
	return n
}

// TestEnsureProvidersCreatesTheProviderApp — the #456 acceptance line. A fresh
// instance whose operator enables library discovery ends up with a LAUNCHABLE
// provider app: marked with library_provider, linked to the materialized preset,
// and carrying the app-level runtime knobs (gpu / no_new_privileges) that ride
// runtime_spec rather than the preset.
func TestEnsureProvidersCreatesTheProviderApp(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}

	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app was created (this is #456: apps stayed empty after enabling the integration)")
	}
	if app.Name != "Steam" {
		t.Errorf("provider app name = %q, want the catalog display_name %q", app.Name, "Steam")
	}
	if app.Kind != "launcher" {
		t.Errorf("provider app kind = %q, want launcher", app.Kind)
	}
	if app.Origin != "manual" {
		t.Errorf("provider app origin = %q, want manual ('discovered' is reserved for scan-created tiles)", app.Origin)
	}
	if !app.Enabled {
		t.Error("provider app is disabled on creation, want enabled")
	}
	if !app.ManagedHome {
		t.Error("provider app managed_home = false, want the manifest's true")
	}

	// It points at the SAME managed preset P5 materialized and linked on the
	// adoption — that is what makes it launchable.
	preset, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset materialized")
	}
	if app.PresetID == nil || *app.PresetID != preset.ID {
		t.Errorf("provider app runtime_preset_id = %v, want the managed preset %q", app.PresetID, preset.ID)
	}

	// runtime_spec carries the app-level knobs only. no_new_privileges=false is
	// load-bearing (#432: Steam re-escalates via sudo). image is ABSENT — the
	// preset supplies it at launch, which is what lets an image update reach this
	// app with no app edit.
	var spec map[string]any
	if err := json.Unmarshal(app.RuntimeSpec, &spec); err != nil {
		t.Fatalf("decode runtime_spec %s: %v", app.RuntimeSpec, err)
	}
	if spec["gpu"] != true {
		t.Errorf("runtime_spec.gpu = %v, want true", spec["gpu"])
	}
	if spec["no_new_privileges"] != false {
		t.Errorf("runtime_spec.no_new_privileges = %v, want false (#432)", spec["no_new_privileges"])
	}
	if _, present := spec["image"]; present {
		t.Errorf("runtime_spec pins an image (%v); it must inherit the preset's adopted ref instead", spec["image"])
	}
	// A repo-relative artwork path is NOT a render URL and must be dropped.
	if app.CoverURL != nil {
		t.Errorf("cover_url = %q, want NULL for a repo-relative manifest artwork path", *app.CoverURL)
	}
}

// TestProviderPresetCarriesTheAdoptedRef — the image-resolution half of #456: the
// managed preset's image is the ADOPTED digest ref (what ensure put on the hosts
// and what session placement matches), never the local-only tag the 0047 seed
// used, and an update moves it.
func TestProviderPresetCarriesTheAdoptedRef(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if got := presetImage(t, env, imgID); got != imgDigest {
		t.Fatalf("managed preset image = %q, want the adopted digest %q", got, imgDigest)
	}

	// A catalog move + update re-materializes the SAME row at the NEW ref, so the
	// provider app follows the image with no app edit.
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", steamRuntime)
	if _, _, err := env.store.Update(ctx, imgID); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := presetImage(t, env, imgID); got != imgDigest2 {
		t.Errorf("managed preset image after update = %q, want %q", got, imgDigest2)
	}
}

func presetImage(t *testing.T, env *actionsEnv, imageID string) string {
	t.Helper()
	var image string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT image FROM runtime_presets WHERE managed_image_id = $1`, imageID).Scan(&image); err != nil {
		t.Fatalf("read managed preset image: %v", err)
	}
	return image
}

// TestEnsureProviderAppIsIdempotentAndPreservesOperatorEdits — a second pass (and
// every startup/after-sync reconcile) must neither duplicate the app nor
// overwrite what the operator changed about it.
func TestEnsureProviderAppIsIdempotentAndPreservesOperatorEdits(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}

	// The operator renames it and turns it off.
	if _, err := env.pool.Exec(ctx,
		`UPDATE apps SET name = 'Steam (operator)', enabled = false WHERE id = $1::uuid`, app.ID); err != nil {
		t.Fatalf("operator edit: %v", err)
	}

	// A reconcile pass (startup / after-sync) runs again.
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (re-run): %v", err)
	}

	if n := countProviderApps(t, env, "steam"); n != 1 {
		t.Fatalf("provider apps after a second pass = %d, want 1 (no duplicate)", n)
	}
	after, _ := readProviderApp(t, env, "steam")
	if after.Name != "Steam (operator)" {
		t.Errorf("provider app name = %q, want the operator's edit preserved", after.Name)
	}
	if after.Enabled {
		t.Error("a reconcile re-enabled an app the operator disabled on purpose; only the explicit enable transition may do that")
	}
}

// TestProviderAppSuspendRestoreRoundTrip — the discovery off/on levels: off
// flips `enabled` off and DELETES NOTHING (deleting would cascade every
// discovered tile, 0044), and on restores exactly the same row.
func TestProviderAppSuspendRestoreRoundTrip(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	before, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}

	n, err := env.store.SuspendProviderApps(ctx)
	if err != nil {
		t.Fatalf("SuspendProviderApps: %v", err)
	}
	if n != 1 {
		t.Errorf("SuspendProviderApps suspended %d apps, want 1", n)
	}
	disabled, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("the provider app was DELETED by the off level; it must only be disabled")
	}
	if disabled.Enabled {
		t.Error("provider app still enabled after SuspendProviderApps")
	}
	if !disabled.Suspended {
		t.Error("library_discovery_suspended = false after a suspend; the marker is what makes the restore safe")
	}

	// Back to the on level. Same row, enabled again, marker cleared, still one.
	if _, err := env.store.RestoreProviderApps(ctx); err != nil {
		t.Fatalf("RestoreProviderApps: %v", err)
	}
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (restore pass): %v", err)
	}
	again, _ := readProviderApp(t, env, "steam")
	if !again.Enabled {
		t.Error("provider app not restored by RestoreProviderApps")
	}
	if again.Suspended {
		t.Error("library_discovery_suspended still true after a restore")
	}
	if again.ID != before.ID {
		t.Errorf("provider app id changed across suspend/restore: %q → %q (a new row, not a restore)", before.ID, again.ID)
	}
	if n := countProviderApps(t, env, "steam"); n != 1 {
		t.Errorf("provider apps after on→off→on = %d, want 1", n)
	}
}

// TestSuspendProviderAppsNamesTheAppsItSuspended (#534) — the suspend pass is
// the ONLY notice an operator gets that an app they can no longer read was
// disabled by the reconciler, and "apps=1" was not enough to act on: it named
// neither the app nor the remedy, so a launch answering `404 app not found`
// could not be matched to it. This pins both onto the line.
func TestSuspendProviderAppsNamesTheAppsItSuspended(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}

	var buf bytes.Buffer
	env.store.SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if _, err := env.store.SuspendProviderApps(ctx); err != nil {
		t.Fatalf("SuspendProviderApps: %v", err)
	}

	line := buf.String()
	if !strings.Contains(line, app.ID) {
		t.Errorf("the suspend log does not name the app id %q — an operator cannot match it to a 404 at launch:\n%s", app.ID, line)
	}
	if !strings.Contains(line, "library_discovery_enabled") {
		t.Errorf("the suspend log does not name the setting that reverses it:\n%s", line)
	}
}

// TestOperatorDisabledProviderAppStaysOff — the Alice-review defect (PR #460).
// An operator who disables the provider app in /admin must find it STILL off
// after toggling library discovery off and back on: the suspend pass must not
// relabel their "off" as ours, and the restore pass must not pick it up.
func TestOperatorDisabledProviderAppStaysOff(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}
	// The operator disables it by hand (what the admin API does).
	if _, err := env.pool.Exec(ctx,
		`UPDATE apps SET enabled = false WHERE id = $1::uuid`, app.ID); err != nil {
		t.Fatalf("operator disable: %v", err)
	}

	// Discovery off…
	if n, err := env.store.SuspendProviderApps(ctx); err != nil {
		t.Fatalf("SuspendProviderApps: %v", err)
	} else if n != 0 {
		t.Errorf("SuspendProviderApps touched %d already-disabled apps, want 0 (an operator's off must not be relabelled as ours)", n)
	}
	suspended, _ := readProviderApp(t, env, "steam")
	if suspended.Suspended {
		t.Fatal("an operator-disabled app was marked library_discovery_suspended; the next enable would resurrect it")
	}

	// …and back on.
	if _, err := env.store.RestoreProviderApps(ctx); err != nil {
		t.Fatalf("RestoreProviderApps: %v", err)
	}
	after, _ := readProviderApp(t, env, "steam")
	if after.Enabled {
		t.Error("an app the operator disabled came back on after a discovery off/on cycle; it must stay off")
	}
}

// TestEnsureProviderAppSkipsAnUninstalledProvider — a provider whose image never
// installed (digest unresolved, #442) gets NO app: an app pointing at nothing
// would be a launch failure presented as a working tile.
func TestEnsureProviderAppSkipsAnUninstalledProvider(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, "", "steam", steamRuntime) // no resolved digest

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if n := countProviderApps(t, env, "steam"); n != 0 {
		t.Errorf("provider apps for an uninstalled provider = %d, want 0", n)
	}
}

// setArtwork overwrites the catalog row's artwork object — the untrusted,
// catalog-supplied value cover_url would come from.
func setArtwork(t *testing.T, env *actionsEnv, artworkJSON string) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE image_catalog SET artwork = $2::jsonb WHERE id = $1`, imgID, artworkJSON); err != nil {
		t.Fatalf("set artwork: %v", err)
	}
}

// TestProviderAppArtworkIsHostAllowlisted — the manifest is untrusted input and
// cover_url lands in an <img src> in an operator's browser, so an artwork URL is
// accepted ONLY when it is https AND its host is in the P2 image-source
// allowlist (QUASAR_IMAGE_REGISTRY_HOSTS, default ghcr.io). Everything else is
// dropped to NULL and renders the gradient tile.
func TestProviderAppArtworkIsHostAllowlisted(t *testing.T) {
	cases := []struct {
		name    string
		artwork string
		want    *string
	}{
		{"relative path (the normal manifest case)", `{"tile":"images/quasar-steam/artwork/tile.png"}`, nil},
		{"off-allowlist host", `{"tile":"https://evil.example/tile.png"}`, nil},
		{"allowlisted host over plaintext http", `{"tile":"http://ghcr.io/art/tile.png"}`, nil},
		{"no artwork at all", `{}`, nil},
		{"https on an allowlisted host", `{"tile":"https://ghcr.io/art/tile.png"}`, strPtr("https://ghcr.io/art/tile.png")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, _ := newActionsEnv(t)
			ctx := context.Background()
			clearApps(t, env)
			seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
			setArtwork(t, env, c.artwork)

			if err := env.store.EnsureProviders(ctx); err != nil {
				t.Fatalf("EnsureProviders: %v", err)
			}
			app, ok := readProviderApp(t, env, "steam")
			if !ok {
				t.Fatal("no provider app created")
			}
			switch {
			case c.want == nil && app.CoverURL != nil:
				t.Errorf("cover_url = %q, want NULL", *app.CoverURL)
			case c.want != nil && (app.CoverURL == nil || *app.CoverURL != *c.want):
				t.Errorf("cover_url = %v, want %q", app.CoverURL, *c.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }

// TestPresetLessProviderAppFollowsTheAdoptedRefOnUpdate — a provider app with NO
// preset names the image in its own runtime_spec, so an image update that moves
// the adopted ref must move the app with it (Alice review, PR #460). Left
// behind, the app would name a ref no host is ensured at and every launch would
// be refused by the placement filter.
//
// And the operator-intent half: an app whose image the operator pinned by hand
// no longer matches the adopted ref and is therefore left exactly as they set it.
func TestPresetLessProviderAppFollowsTheAdoptedRefOnUpdate(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{}`) // no runtime block → no preset

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if got := providerAppImage(t, env, "steam"); got != imgDigest {
		t.Fatalf("provider app image = %q, want the adopted ref %q", got, imgDigest)
	}

	// The catalog moves and the admin updates: the app follows.
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", `{}`)
	if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}
	if got := providerAppImage(t, env, "steam"); got != imgDigest2 {
		t.Errorf("provider app image after update = %q, want the new adopted ref %q", got, imgDigest2)
	}

	// The operator pins their own image; a later update must NOT overwrite it.
	const pinned = "ghcr.io/operator/steam@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	if _, err := env.pool.Exec(ctx,
		`UPDATE apps SET runtime_spec = jsonb_set(runtime_spec, '{image}', to_jsonb($1::text), true)
		  WHERE library_provider = 'steam'`, pinned); err != nil {
		t.Fatalf("operator pin: %v", err)
	}
	const imgVer3 = "2026.08.10"
	const imgDigest3 = "ghcr.io/accreleus/quasar-steam@sha256:33333333333333333333333333333333333333333333333333333333333333cc"
	seedCatalogRuntime(t, env, imgVer3, imgDigest3, "steam", `{}`)
	if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
		t.Fatalf("update 3: applied=%v err=%v", applied, err)
	}
	if got := providerAppImage(t, env, "steam"); got != pinned {
		t.Errorf("provider app image after an update = %q, want the operator's pin %q to survive", got, pinned)
	}
}

// TestProviderAppFollowsARuntimeBlockDropped — the manifest LOSES its runtime
// block on an update. installed_images unlinks from the old preset; the app must
// unlink too and take the new ref into its own runtime_spec, or it launches
// against a preset holding a ref no host is ensured at (Alice review round 2).
func TestProviderAppFollowsARuntimeBlockDropped(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime) // WITH a runtime block

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok || app.PresetID == nil {
		t.Fatalf("provider app not created with a preset link: %+v", app)
	}

	// The new manifest version drops the runtime block.
	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", `{}`)
	if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}

	after, _ := readProviderApp(t, env, "steam")
	if after.PresetID != nil {
		t.Errorf("provider app still linked to preset %q after the manifest dropped its runtime block", *after.PresetID)
	}
	if got := providerAppImage(t, env, "steam"); got != imgDigest2 {
		t.Errorf("provider app image = %q, want the new adopted ref %q", got, imgDigest2)
	}
	if link := installedPresetLink(t, env, imgID); link != nil {
		t.Errorf("installed_images.runtime_preset_id = %v, want NULL", link)
	}
}

// TestProviderAppFollowsARuntimeBlockAdded — the mirror crossing: the manifest
// GAINS a runtime block. The app must adopt the new preset and DROP its own
// image key, or its stale image would override the preset's new ref at launch
// (the app wins on image in mergeRuntimePreset).
func TestProviderAppFollowsARuntimeBlockAdded(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{}`) // NO runtime block

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok || app.PresetID != nil {
		t.Fatalf("provider app not created preset-less: %+v", app)
	}

	seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", steamRuntime)
	if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
		t.Fatalf("update: applied=%v err=%v", applied, err)
	}

	preset, ok := readManagedPreset(t, env, imgID)
	if !ok {
		t.Fatal("no managed preset materialized by the update")
	}
	after, _ := readProviderApp(t, env, "steam")
	if after.PresetID == nil || *after.PresetID != preset.ID {
		t.Errorf("provider app runtime_preset_id = %v, want the new managed preset %q", after.PresetID, preset.ID)
	}
	if got := providerAppImage(t, env, "steam"); got != "" {
		t.Errorf("provider app runtime_spec.image = %q, want it REMOVED so the preset's ref wins", got)
	}
}

// TestRuntimeBlockTransitionsLeaveOperatorEditsAlone — every predicate in the
// transition migration is on the OLD value, so an app the operator re-pointed
// (their own preset) or pinned (their own image) matches nothing and is
// untouched by either crossing.
func TestRuntimeBlockTransitionsLeaveOperatorEditsAlone(t *testing.T) {
	t.Run("operator-pinned image across block-added", func(t *testing.T) {
		env, _ := newActionsEnv(t)
		ctx := context.Background()
		clearApps(t, env)
		seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{}`)
		if err := env.store.EnsureProviders(ctx); err != nil {
			t.Fatalf("EnsureProviders: %v", err)
		}
		const pinned = "ghcr.io/operator/steam@sha256:2222222222222222222222222222222222222222222222222222222222222222"
		if _, err := env.pool.Exec(ctx,
			`UPDATE apps SET runtime_spec = jsonb_set(runtime_spec, '{image}', to_jsonb($1::text), true)
			  WHERE library_provider = 'steam'`, pinned); err != nil {
			t.Fatalf("operator pin: %v", err)
		}

		seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", steamRuntime)
		if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
			t.Fatalf("update: applied=%v err=%v", applied, err)
		}
		after, _ := readProviderApp(t, env, "steam")
		if after.PresetID != nil {
			t.Error("an operator-pinned app was re-pointed at the new managed preset; it must be left alone")
		}
		if got := providerAppImage(t, env, "steam"); got != pinned {
			t.Errorf("provider app image = %q, want the operator's pin %q", got, pinned)
		}
	})

	t.Run("operator preset across block-dropped", func(t *testing.T) {
		env, _ := newActionsEnv(t)
		ctx := context.Background()
		clearApps(t, env)
		seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
		if err := env.store.EnsureProviders(ctx); err != nil {
			t.Fatalf("EnsureProviders: %v", err)
		}
		// The operator re-points the app at a preset of their own.
		var ownPreset string
		if err := env.pool.QueryRow(ctx, `
			INSERT INTO runtime_presets (name, image) VALUES ('Operator Steam', 'ghcr.io/operator/steam:own')
			RETURNING id::text`).Scan(&ownPreset); err != nil {
			t.Fatalf("create operator preset: %v", err)
		}
		if _, err := env.pool.Exec(ctx,
			`UPDATE apps SET runtime_preset_id = $1::uuid WHERE library_provider = 'steam'`, ownPreset); err != nil {
			t.Fatalf("operator re-point: %v", err)
		}

		seedCatalogRuntime(t, env, imgVer2, imgDigest2, "steam", `{}`)
		if applied, _, err := env.store.Update(ctx, imgID); err != nil || !applied {
			t.Fatalf("update: applied=%v err=%v", applied, err)
		}
		after, _ := readProviderApp(t, env, "steam")
		if after.PresetID == nil || *after.PresetID != ownPreset {
			t.Errorf("provider app runtime_preset_id = %v, want the operator's preset %q left in place", after.PresetID, ownPreset)
		}
		if got := providerAppImage(t, env, "steam"); got != "" {
			t.Errorf("an image key was written onto an operator-pointed app: %q", got)
		}
	})
}

func providerAppImage(t *testing.T, env *actionsEnv, provider string) string {
	t.Helper()
	var image *string
	if err := env.pool.QueryRow(context.Background(),
		`SELECT runtime_spec->>'image' FROM apps WHERE library_provider = $1`, provider).Scan(&image); err != nil {
		t.Fatalf("read provider app image: %v", err)
	}
	if image == nil {
		return ""
	}
	return *image
}

// TestProviderAppNamesTheImageWhenThereIsNoPreset — an image with NO runtime
// block materializes no preset, so nothing else would name an image at launch;
// in that case (and only that case) the adopted ref goes into the app's own
// runtime_spec.
func TestProviderAppNamesTheImageWhenThereIsNoPreset(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", `{}`) // no runtime block

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created for a runtime-less provider image")
	}
	if app.PresetID != nil {
		t.Errorf("runtime_preset_id = %v, want NULL (the image carries no runtime block)", app.PresetID)
	}
	var spec map[string]any
	if err := json.Unmarshal(app.RuntimeSpec, &spec); err != nil {
		t.Fatalf("decode runtime_spec: %v", err)
	}
	if spec["image"] != imgDigest {
		t.Errorf("runtime_spec.image = %v, want the adopted ref %q", spec["image"], imgDigest)
	}
}

// --- entitlement ensure (first-run-experience spec §S3, closing the #456
// follow-on gap: enabling a provider created the app but no entitlement, so
// the operator had to hand-grant their own account) ------------------------

type providerAppEntitlementRow struct {
	SubjectType string
	SubjectID   *string
	GrantedBy   string
	SourceRef   string
}

func readAppEntitlements(t *testing.T, env *actionsEnv, appID string) []providerAppEntitlementRow {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
		SELECT subject_type, subject_id::text, granted_by, source_ref
		  FROM entitlements WHERE app_id = $1::uuid`, appID)
	if err != nil {
		t.Fatalf("read entitlements: %v", err)
	}
	defer rows.Close()
	var out []providerAppEntitlementRow
	for rows.Next() {
		var r providerAppEntitlementRow
		if err := rows.Scan(&r.SubjectType, &r.SubjectID, &r.GrantedBy, &r.SourceRef); err != nil {
			t.Fatalf("scan entitlement: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate entitlements: %v", err)
	}
	return out
}

func seedTestUser(t *testing.T, env *actionsEnv, email, username string) string {
	t.Helper()
	var id string
	if err := env.pool.QueryRow(context.Background(), `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ($1, $2, 'x', 'user') RETURNING id::text`, email, username).Scan(&id); err != nil {
		t.Fatalf("seed test user: %v", err)
	}
	return id
}

// TestEnsureProviderAppGrantsAllEntitlementOnCreate — a fresh enable creates
// the app AND a subject_type='all' entitlement for it in the same pass, with
// granted_by='provider' (the automated-ensure convention library/store.go
// already uses for scan-written rows, distinct from an operator's explicit
// 'admin' grant in the admin UI).
func TestEnsureProviderAppGrantsAllEntitlementOnCreate(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}

	ents := readAppEntitlements(t, env, app.ID)
	if len(ents) != 1 {
		t.Fatalf("entitlements for the new provider app = %d, want 1: %+v", len(ents), ents)
	}
	if ents[0].SubjectType != "all" {
		t.Errorf("entitlement subject_type = %q, want all", ents[0].SubjectType)
	}
	if ents[0].SubjectID != nil {
		t.Errorf("entitlement subject_id = %v, want NULL for subject_type=all", ents[0].SubjectID)
	}
	if ents[0].GrantedBy != "provider" {
		t.Errorf("entitlement granted_by = %q, want provider", ents[0].GrantedBy)
	}
}

// TestEnsureProviderAppReconcileDoesNotResurrectDeletedEntitlement — the
// operator-intent discipline from spec §S3: once the app exists, the
// entitlement ensure must run ONLY on the create path. An operator who
// revokes the entitlement (leaving the app enabled) must find it still gone
// after every later reconcile (startup, after-sync, a second settings
// toggle) — mirroring how EnsureProviderApp already never touches an existing
// app's name/preset/runtime_spec/enabled (TestEnsureProviderAppIsIdempotent...
// above).
func TestEnsureProviderAppReconcileDoesNotResurrectDeletedEntitlement(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}
	if n := len(readAppEntitlements(t, env, app.ID)); n != 1 {
		t.Fatalf("entitlements before revoke = %d, want 1", n)
	}

	// The operator revokes it (what DELETE /v1/admin/apps/{id}/entitlements/{id} does).
	if _, err := env.pool.Exec(ctx, `DELETE FROM entitlements WHERE app_id = $1::uuid`, app.ID); err != nil {
		t.Fatalf("operator revoke: %v", err)
	}

	// Reconcile passes: startup-equivalent re-run, twice.
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (reconcile 1): %v", err)
	}
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (reconcile 2): %v", err)
	}

	if n := len(readAppEntitlements(t, env, app.ID)); n != 0 {
		t.Errorf("entitlements after reconcile = %d, want 0 (an operator's revoke must never be resurrected by a reconcile)", n)
	}
}

// TestProviderAppAllEntitlementMakesItVisibleToAnyUser — confirms the
// subject_type='all' row EnsureProviderApp writes is actually what the user
// library's entitlement filter (crud/store.go entitledSQL, hand-copied at
// internal/crud/entitlements.go entitledToApp, internal/session/store.go
// IsEntitled, and internal/session/scheduler.go scheduleAttempt) reads: an
// EXISTS over entitlements matching (subject_type='all') OR
// (subject_type='user' AND subject_id=caller). This test reproduces that
// exact predicate (images cannot import crud — crud imports images) against
// an ARBITRARY user who was never mentioned anywhere in the enable flow, to
// prove "all" really does mean "current and future users", not merely "every
// user who exists right now".
func TestProviderAppAllEntitlementMakesItVisibleToAnyUser(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	clearApps(t, env)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	app, ok := readProviderApp(t, env, "steam")
	if !ok {
		t.Fatal("no provider app created")
	}

	// A user created AFTER the provider was enabled — "future users" per the
	// spec's reading of subject_type='all'.
	futureUser := seedTestUser(t, env, "future@t.local", "future")

	var visible bool
	if err := env.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM apps
			WHERE apps.id = $1::uuid
			  AND apps.enabled = true
			  AND EXISTS (
				SELECT 1 FROM entitlements e
				WHERE e.app_id = apps.id
				  AND (e.subject_type = 'all'
				       OR (e.subject_type = 'user' AND e.subject_id = $2::uuid))
			  )
		)`, app.ID, futureUser).Scan(&visible); err != nil {
		t.Fatalf("library visibility query: %v", err)
	}
	if !visible {
		t.Error("provider app not visible to an arbitrary (future) user despite a subject_type='all' entitlement")
	}
}
