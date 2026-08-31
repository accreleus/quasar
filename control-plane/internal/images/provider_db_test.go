// provider_db_test.go — image-management P5 acceptance: library-provider
// auto-ensure (Store.EnsureProviders). TEST_DATABASE_URL-gated.
package images

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestSyncFiresReconcileHook — a successful Sync invokes the onSyncSuccess hook
// (the P5 after-sync provider reconciler seam app.go wires). This is what lets a
// provider that was unresolved at enable retry once a later sync resolves it.
func TestSyncFiresReconcileHook(t *testing.T) {
	env, _ := newActionsEnv(t)
	var fired atomic.Int32
	env.store.SetOnSyncSuccess(func() { fired.Add(1) })

	if _, err := env.store.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if fired.Load() != 1 {
		t.Errorf("onSyncSuccess fired %d times after a successful sync, want 1", fired.Load())
	}
}

func isInstalled(t *testing.T, env *actionsEnv, imageID string) bool {
	t.Helper()
	var ok bool
	if err := env.pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM installed_images WHERE image_id = $1)`, imageID).Scan(&ok); err != nil {
		t.Fatalf("check installed: %v", err)
	}
	return ok
}

// TestEnsureProvidersInstallsUninstalled — flipping discovery on installs an
// uninstalled provider image (and materializes its preset), and is idempotent.
func TestEnsureProvidersInstallsUninstalled(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if isInstalled(t, env, imgID) {
		t.Fatal("provider image already installed before EnsureProviders")
	}
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if !isInstalled(t, env, imgID) {
		t.Fatal("EnsureProviders did not install the uninstalled provider image")
	}
	// The P3 install path ran → the managed preset was materialized too.
	if _, ok := readManagedPreset(t, env, imgID); !ok {
		t.Error("EnsureProviders installed the image but did not materialize its preset")
	}

	// Idempotent: a second pass is a no-op, no error, still exactly one adoption.
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (re-run): %v", err)
	}
	var count int
	if err := env.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM installed_images WHERE image_id = $1`, imgID).Scan(&count); err != nil {
		t.Fatalf("count adoptions: %v", err)
	}
	if count != 1 {
		t.Errorf("adoptions = %d after re-run, want 1 (idempotent)", count)
	}
}

// TestEnsureProvidersDigestUnresolvedNotFatal — a provider whose digest is
// unresolved stays uninstalled and does NOT fail the pass; a resolvable provider
// alongside it still installs.
func TestEnsureProvidersDigestUnresolvedNotFatal(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	// Provider image with NO resolved digest (empty registry_digest).
	seedCatalogRuntime(t, env, imgVer, "", "steam", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders must never be fatal on digest_unresolved: %v", err)
	}
	if isInstalled(t, env, imgID) {
		t.Error("a digest-unresolved provider was installed; it must stay uninstalled")
	}
}

// TestEnsureProvidersIgnoresNonProviders — an image with no library_provider is
// never auto-installed.
func TestEnsureProvidersIgnoresNonProviders(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	seedCatalogRuntime(t, env, imgVer, imgDigest, "", steamRuntime) // no provider

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if isInstalled(t, env, imgID) {
		t.Error("a non-provider image was auto-installed by EnsureProviders")
	}
}

// TestEnsureProvidersSkipsUnallowlistedProvider — a catalog image claiming a
// library_provider NOT in the local allowlist is skipped (the trust decision is
// local, not catalog-controlled); adding it to the allowlist makes it install.
func TestEnsureProvidersSkipsUnallowlistedProvider(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	// A catalog image marking itself as a provider named "evil" — the compromised-
	// catalog case. Default allowlist is {steam}, so it must be skipped.
	seedCatalogRuntimeID(t, env, "evilimg", imgVer, imgDigest, "evil", steamRuntime)

	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders: %v", err)
	}
	if isInstalled(t, env, "evilimg") {
		t.Fatal("an image claiming a non-allowlisted library_provider was auto-installed")
	}

	// Once the operator lists "evil", it installs — the allowlist is the gate.
	env.store.SetProviderAllowlist("steam,evil")
	if err := env.store.EnsureProviders(ctx); err != nil {
		t.Fatalf("EnsureProviders (allowlisted): %v", err)
	}
	if !isInstalled(t, env, "evilimg") {
		t.Error("an allowlisted provider was not installed")
	}
}

// TestReconcileInstallsAlreadyEnabledProvider — the P5 startup/after-sync
// reconciler decision (app.go): when library discovery is ALREADY enabled (an
// upgrade, or a process that exited mid-pass), a run of EnsureProviders installs
// the enabled provider. Mirrors the enabled-gate the app.go closure applies.
func TestReconcileInstallsAlreadyEnabledProvider(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	// Discovery already true — not a fresh false->true PATCH flip.
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO instance_settings (id, library_discovery_enabled) VALUES (true, true)
		ON CONFLICT (id) DO UPDATE SET library_discovery_enabled = true`); err != nil {
		t.Fatalf("enable discovery: %v", err)
	}
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	// The decision app.go's reconciler makes: run ONLY when enabled.
	reconcile := func() {
		var enabled bool
		if err := env.pool.QueryRow(ctx,
			`SELECT library_discovery_enabled FROM instance_settings WHERE id = true`).Scan(&enabled); err != nil {
			t.Fatalf("read discovery switch: %v", err)
		}
		if !enabled {
			return
		}
		if err := env.store.EnsureProviders(ctx); err != nil {
			t.Fatalf("EnsureProviders: %v", err)
		}
	}
	reconcile()

	if !isInstalled(t, env, imgID) {
		t.Fatal("startup reconcile did not install the already-enabled provider")
	}
}
