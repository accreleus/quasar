// provider_suspension_test.go — #456 / migration 0060: an explicit admin write
// of `enabled` is AUTHORITATIVE and clears library_discovery_suspended, so the
// library-discovery reconciler can never resurrect an app the operator turned
// off themselves (Alice review round 2, PR #460).
//
// This lives in internal/crud, not internal/images, because the path under test
// is the admin app-update path — and internal/crud sits above internal/images in
// the dependency graph, so the images package cannot reach it.
package crud

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// restoreSuspended is the reconciler's restore statement (images.Store.
// RestoreProviderApps), inlined because that package is below this one. If the
// two ever diverge this test stops proving anything about production — the
// statement is one line and pinned by images' own round-trip test; what THIS
// test owns is the interaction with the crud write.
const restoreSuspended = `UPDATE apps SET enabled = true, library_discovery_suspended = false
                           WHERE library_discovery_suspended = true`

// TestOperatorDisableClearsSuspensionMarker — the full sequence Alice described:
// discovery off suspends the provider app, the operator then disables it
// explicitly through the admin API, and turning discovery back on must NOT
// resurrect it.
func TestOperatorDisableClearsSuspensionMarker(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = 'admin@test.local'`); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var appID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (name, kind, library_provider, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, '{"gpu":true}'::jsonb)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}

	// Discovery off → the reconciler suspends it.
	if _, err := pool.Exec(ctx, `
		UPDATE apps SET enabled = false, library_discovery_suspended = true
		 WHERE library_provider <> '' AND enabled = true`); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if enabled, suspended := readFlags(t, pool, appID); enabled || !suspended {
		t.Fatalf("after suspend: enabled=%v suspended=%v, want false/true", enabled, suspended)
	}

	// The operator disables it explicitly through the admin API. It is already
	// off, so the visible state does not change — the MARKER does.
	resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"enabled": false}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin disable: want 200, got %d (%v)", resp.StatusCode, body)
	}
	enabled, suspended := readFlags(t, pool, appID)
	if suspended {
		t.Fatal("library_discovery_suspended survived an explicit admin write of enabled; the operator's intent must win")
	}
	if enabled {
		t.Fatal("app is enabled after an admin disable")
	}

	// Discovery back on → the restore pass must not touch it.
	if _, err := pool.Exec(ctx, restoreSuspended); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if enabled, _ := readFlags(t, pool, appID); enabled {
		t.Error("an app the operator disabled was resurrected by a discovery re-enable")
	}
}

// TestAdminEnableOfASuspendedAppSucceeds — the constraint half. 0060's CHECK
// makes suspended+enabled impossible, so re-enabling a suspended app WITHOUT
// clearing the marker in the same statement would fail with a constraint
// violation rather than a 200.
func TestAdminEnableOfASuspendedAppSucceeds(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = 'admin@test.local'`); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	var appID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (name, kind, library_provider, enabled, library_discovery_suspended)
		VALUES ('Steam', 'launcher', 'steam', false, true)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed suspended app: %v", err)
	}

	// Discovery ON: what this test owns is the 0060 CHECK interaction, and #534's
	// guard refuses this same PATCH while discovery is OFF (see
	// TestEnablingASuspendedProviderAppWhileDiscoveryOffIsRefused, which owns that
	// half). Setting the level explicitly also makes the test independent of
	// whatever the shared test database's instance_settings row was left at.
	setLibraryDiscovery(t, pool, true)

	resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"enabled": true}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin enable of a suspended app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	enabled, suspended := readFlags(t, pool, appID)
	if !enabled || suspended {
		t.Errorf("after admin enable: enabled=%v suspended=%v, want true/false", enabled, suspended)
	}
}

// TestSuspendedImpliesDisabledCheck — the durable guard behind both of the
// above: the database itself refuses an enabled+suspended row.
func TestSuspendedImpliesDisabledCheck(t *testing.T) {
	pool := testDB(t)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO apps (name, enabled, library_discovery_suspended)
		VALUES ('bad', true, true)`); err == nil {
		t.Error("an enabled + library_discovery_suspended row was accepted; the CHECK must refuse it")
	}
}

// --- #534: a provider write while discovery is off is refused, not silently
// undone; and a suspended app explains itself on the admin surface ------------

// TestCreateProviderAppWhileDiscoveryOffIsRefused is the #534 headline. Before
// the guard this create answered 201 with `enabled: true` for a row the
// reconciler disabled seconds later, after which every read was a bare 404.
func TestCreateProviderAppWhileDiscoveryOffIsRefused(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)
	setLibraryDiscovery(t, pool, false)

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":             "Steam",
		"kind":             "launcher",
		"library_provider": "steam",
	}, tok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("create a provider app with discovery off: want 409, got %d (%v)", resp.StatusCode, body)
	}
	if code := errorCode(body); code != "library_discovery_disabled" {
		t.Errorf("error code = %q, want library_discovery_disabled (%v)", code, body)
	}

	// FAIL CLOSED, ALL THE WAY: no row, no entitlement, no id handed back that
	// would stop resolving. A 409 that still created the app would be the same
	// defect wearing a different status code.
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM apps WHERE library_provider <> ''`).Scan(&n); err != nil {
		t.Fatalf("count provider apps: %v", err)
	}
	if n != 0 {
		t.Errorf("%d provider app(s) exist after a refused create; want 0", n)
	}
}

// TestCreateProviderAppWhileDiscoveryOnSucceeds is the other half of the gate:
// the refusal is keyed on the SETTING, and turning it on restores the ordinary
// create. Without this, "refuse provider apps" would pass just as well.
func TestCreateProviderAppWhileDiscoveryOnSucceeds(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)
	setLibraryDiscovery(t, pool, true)

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":             "Steam",
		"kind":             "launcher",
		"library_provider": "steam",
	}, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create a provider app with discovery on: want 201, got %d (%v)", resp.StatusCode, body)
	}
}

// TestSettingLibraryProviderWhileDiscoveryOffIsRefused — the PATCH route into the
// same trap: an ordinary enabled app promoted to a provider is suspended by the
// next reconcile pass exactly as a freshly created one would be.
func TestSettingLibraryProviderWhileDiscoveryOffIsRefused(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)
	setLibraryDiscovery(t, pool, false)

	var appID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind) VALUES ('Steam', 'launcher') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}

	resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"library_provider": "steam"}, tok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("promote to provider with discovery off: want 409, got %d (%v)", resp.StatusCode, body)
	}
	if code := errorCode(body); code != "library_discovery_disabled" {
		t.Errorf("error code = %q, want library_discovery_disabled (%v)", code, body)
	}
}

// TestEnablingASuspendedProviderAppWhileDiscoveryOffIsRefused — the recovery
// attempt an operator actually makes when the app has vanished. It USED to
// succeed and then be undone by the next reconcile trigger (a settings write, a
// catalog sync, or a restart), which is the same disappearance one step removed.
func TestEnablingASuspendedProviderAppWhileDiscoveryOffIsRefused(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)
	setLibraryDiscovery(t, pool, false)

	var appID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind, library_provider, enabled, library_discovery_suspended)
		VALUES ('Steam', 'launcher', 'steam', false, true)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed suspended app: %v", err)
	}

	resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"enabled": true}, tok)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("re-enable a suspended provider app with discovery off: want 409, got %d (%v)", resp.StatusCode, body)
	}
	if code := errorCode(body); code != "library_discovery_disabled" {
		t.Errorf("error code = %q, want library_discovery_disabled (%v)", code, body)
	}

	// The two ways OUT stay open — an operator must never need the instance-wide
	// switch just to edit or retire the app.
	if resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"name": "Steam (retired)"}, tok); resp.StatusCode != http.StatusOK {
		t.Errorf("an unrelated edit of a suspended provider app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	if resp, body := patch(t, srv.URL+"/v1/apps/"+appID, map[string]any{"library_provider": ""}, tok); resp.StatusCode != http.StatusOK {
		t.Errorf("clearing library_provider on a suspended app: want 200, got %d (%v)", resp.StatusCode, body)
	}
}

// TestAdminReadsSurfaceLibraryDiscoverySuspended is #534's observability half: a
// suspended app must EXPLAIN its `enabled: false` to an admin, on both admin
// reads, rather than leaving the operator to infer it from a log line they have
// no reason to look for.
func TestAdminReadsSurfaceLibraryDiscoverySuspended(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)

	var appID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind, library_provider, enabled, library_discovery_suspended)
		VALUES ('Steam', 'launcher', 'steam', false, true)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed suspended app: %v", err)
	}

	resp, body := getReq(t, srv.URL+"/v1/apps/"+appID, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin get of a suspended app: want 200, got %d (%v)", resp.StatusCode, body)
	}
	app, _ := body["app"].(map[string]any)
	if app["enabled"] != false {
		t.Errorf("enabled = %v, want false", app["enabled"])
	}
	if app["library_discovery_suspended"] != true {
		t.Errorf("library_discovery_suspended = %v, want true — a bare enabled:false does not say WHY (%v)",
			app["library_discovery_suspended"], app)
	}

	resp, body = getReq(t, srv.URL+"/v1/admin/apps", tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin list: want 200, got %d (%v)", resp.StatusCode, body)
	}
	items, _ := body["items"].([]any)
	var found bool
	for _, it := range items {
		item, _ := it.(map[string]any)
		if item["id"] != appID {
			continue
		}
		found = true
		if item["library_discovery_suspended"] != true {
			t.Errorf("admin list library_discovery_suspended = %v, want true (%v)",
				item["library_discovery_suspended"], item)
		}
	}
	if !found {
		t.Error("the suspended provider app is absent from GET /v1/admin/apps; the admin god view must still show it")
	}
}

// TestNonProviderAppReadsSuspendedFalse pins the additive field's default so the
// flag cannot start meaning something for ordinary apps.
func TestNonProviderAppReadsSuspendedFalse(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	tok := adminToken(t, pool, authSvc)

	var appID string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO apps (name, kind) VALUES ('Plain', 'game') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	_, body := getReq(t, srv.URL+"/v1/apps/"+appID, tok)
	app, _ := body["app"].(map[string]any)
	if app["library_discovery_suspended"] != false {
		t.Errorf("library_discovery_suspended = %v on an ordinary app, want false", app["library_discovery_suspended"])
	}
}

// errorCode digs the discriminator out of the standard error envelope.
func errorCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// setLibraryDiscovery writes the instance-wide master switch directly. Every
// #534 test states the level it wants, for two reasons: testDB does NOT truncate
// instance_settings, so the singleton carries whatever a previous test left; and
// migration 0020 deliberately does not SEED the row at all (the control plane
// does, at boot), so a fresh database has none. Hence the upsert.
func setLibraryDiscovery(t *testing.T, pool *pgxpool.Pool, on bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO instance_settings (id, library_discovery_enabled) VALUES (true, $1)
		ON CONFLICT (id) DO UPDATE SET library_discovery_enabled = EXCLUDED.library_discovery_enabled`,
		on); err != nil {
		t.Fatalf("set library_discovery_enabled=%v: %v", on, err)
	}
}

// adminToken registers, promotes and logs in the fixture admin.
func adminToken(t *testing.T, pool *pgxpool.Pool, authSvc *auth.Service) string {
	t.Helper()
	ctx := context.Background()
	if _, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = 'admin@test.local'`); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tok, err := authSvc.Login(ctx, "admin@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	return tok.Plaintext
}

func readFlags(t *testing.T, pool *pgxpool.Pool, appID string) (bool, bool) {
	t.Helper()
	var enabled, suspended bool
	if err := pool.QueryRow(context.Background(),
		`SELECT enabled, library_discovery_suspended FROM apps WHERE id::text = $1`, appID).
		Scan(&enabled, &suspended); err != nil {
		t.Fatalf("read app flags: %v", err)
	}
	return enabled, suspended
}
