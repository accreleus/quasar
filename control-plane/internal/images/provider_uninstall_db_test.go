// provider_uninstall_db_test.go — #471 acceptance: uninstalling a
// library-provider image while discovery is enabled is refused (409
// provider_enabled) instead of being silently undone by the next catalog
// sync's provider auto-ensure; while disabled, the uninstall proceeds and a
// subsequent sync's reconcile does NOT reinstall it. TEST_DATABASE_URL-gated.
package images

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// setLibraryDiscoveryEnabled is the same direct-SQL seed provider_db_test.go's
// TestReconcileInstallsAlreadyEnabledProvider uses — no internal/settings
// import from this test package either.
func setLibraryDiscoveryEnabled(t *testing.T, env *actionsEnv, enabled bool) {
	t.Helper()
	if _, err := env.pool.Exec(context.Background(), `
		INSERT INTO instance_settings (id, library_discovery_enabled) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET library_discovery_enabled = $1`, enabled); err != nil {
		t.Fatalf("set library_discovery_enabled=%v: %v", enabled, err)
	}
}

// reconcileLikeAppGo mirrors the app.go after-sync provider reconciler: run
// EnsureProviders ONLY when discovery is currently enabled. Wired as the
// Store's onSyncSuccess hook, it is the production shape of "catalog sync's
// provider auto-ensure" the issue describes.
func reconcileLikeAppGo(t *testing.T, env *actionsEnv) func() {
	t.Helper()
	ctx := context.Background()
	return func() {
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
}

// TestUninstallRefusedWhileProviderEnabled is the #471 fix's core case: with
// library discovery enabled, DELETE .../install on the provider image is
// refused with 409 provider_enabled naming the provider's display name and
// pointing at Settings, and the image stays installed.
func TestUninstallRefusedWhileProviderEnabled(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	setLibraryDiscoveryEnabled(t, env, true)

	code, body := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/install", "")
	if code != http.StatusConflict {
		t.Fatalf("uninstall while provider enabled: status %d body %s, want 409", code, body)
	}
	if got := errCode(t, body); got != "provider_enabled" {
		t.Fatalf("error code: got %q want provider_enabled", got)
	}
	if !strings.Contains(string(body), "Steam") {
		t.Errorf("409 body does not name the provider (Steam): %s", body)
	}
	if !strings.Contains(string(body), "Settings") {
		t.Errorf("409 body does not point the operator at Settings: %s", body)
	}
	if !isInstalled(t, env, imgID) {
		t.Fatal("uninstall was refused but the image was uninstalled anyway")
	}

	// A not-actually-installed id must still answer 404, never 409, even while
	// discovery is enabled — existence is decided before providerhood.
	code2, body2 := env.do(t, http.MethodDelete, "/v1/admin/images/nonexistent/install", "")
	if code2 != http.StatusNotFound {
		t.Fatalf("uninstall of a never-installed id: status %d body %s, want 404", code2, body2)
	}
	if got := errCode(t, body2); got != "not_installed" {
		t.Fatalf("error code: got %q want not_installed", got)
	}
}

// TestUninstallAllowedWhileProviderDisabledStaysUninstalled is #471's other
// half: with discovery disabled, the uninstall succeeds (204), and a
// subsequent catalog sync's reconcile — the exact mechanism that used to
// silently reinstall the image 4s later — leaves it uninstalled because the
// reconciler's own enabled-gate (app.go) never runs EnsureProviders while
// discovery is off.
func TestUninstallAllowedWhileProviderDisabledStaysUninstalled(t *testing.T) {
	env, _ := newActionsEnv(t)
	ctx := context.Background()
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)
	env.store.SetOnSyncSuccess(reconcileLikeAppGo(t, env))

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	// Discovery was never enabled in this test — the default posture (SHIP-DARK).

	code, body := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/install", "")
	if code != http.StatusNoContent {
		t.Fatalf("uninstall while provider disabled: status %d body %s, want 204", code, body)
	}
	if isInstalled(t, env, imgID) {
		t.Fatal("uninstall reported success but the adoption row is still there")
	}

	// The operator's next "Sync catalog" click — same trigger the issue
	// reproduces the bug from.
	if _, err := env.store.Sync(ctx); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if isInstalled(t, env, imgID) {
		t.Fatal("sync's provider auto-ensure reinstalled the image although discovery is disabled — #471 regression")
	}
}

// TestUninstallRefusedNamesEnabledProvider re-enabling discovery AFTER an
// uninstall must not retroactively matter to that already-completed action —
// this just pins the reverse: enabling discovery, then immediately trying to
// uninstall again while it's an active provider, refuses every time (not just
// the first), so the guard is a standing check, not a one-shot latch.
func TestUninstallRefusedNamesEnabledProviderRepeatedly(t *testing.T) {
	env, _ := newActionsEnv(t)
	seedCatalogRuntime(t, env, imgVer, imgDigest, "steam", steamRuntime)

	if code, body := env.do(t, http.MethodPost, "/v1/admin/images/"+imgID+"/install", `{"lazy":true}`); code != http.StatusCreated {
		t.Fatalf("install: status %d body %s", code, body)
	}
	setLibraryDiscoveryEnabled(t, env, true)

	for i := 0; i < 2; i++ {
		code, body := env.do(t, http.MethodDelete, "/v1/admin/images/"+imgID+"/install", "")
		if code != http.StatusConflict {
			t.Fatalf("attempt %d: status %d body %s, want 409", i, code, body)
		}
	}
	if !isInstalled(t, env, imgID) {
		t.Fatal("image was uninstalled despite the guard")
	}
}
