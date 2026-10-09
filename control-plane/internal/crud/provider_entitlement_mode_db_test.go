// provider_entitlement_mode_db_test.go — #465. DB-backed: exercises the real
// entitlements table through the HTTP surface, same pattern as
// provider_suspension_test.go and entitlements_test.go's siblings.
package crud

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// TestSetProviderEntitlementModeAll — enabling "all" after a prior "user"-only
// state replaces the personal grant with the everyone row (REPLACE semantics,
// the load-bearing behaviour this endpoint promises in its doc comment).
func TestSetProviderEntitlementModeAll(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	admin, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01")
	if err != nil {
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
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}
	// Pre-seed a personal grant that "all" must clear away.
	if _, err := pool.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, granted_by_user)
		VALUES ('user', $1::uuid, $2::uuid, 'admin', $1::uuid)`, admin.ID, appID); err != nil {
		t.Fatalf("seed prior entitlement: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "all"}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", resp.StatusCode, body)
	}
	em, ok := body["entitlement_mode"].(map[string]any)
	if !ok {
		t.Fatalf("no entitlement_mode in response: %v", body)
	}
	if em["mode"] != "all" {
		t.Errorf("mode = %v, want all", em["mode"])
	}
	if em["app_id"] != appID {
		t.Errorf("app_id = %v, want %v", em["app_id"], appID)
	}
	items, _ := em["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want exactly one row (the prior personal grant must be cleared)", items)
	}
	row := items[0].(map[string]any)
	if row["subject_type"] != "all" {
		t.Errorf("subject_type = %v, want all", row["subject_type"])
	}
	if row["subject_id"] != nil {
		t.Errorf("subject_id = %v, want null for an all-users row", row["subject_id"])
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE app_id::text = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("entitlements row count = %d, want 1 (the stale personal grant must be gone)", n)
	}
}

// TestSetProviderEntitlementModeUser — "user" entitles the ACTING admin
// specifically, replacing whatever was there (here: the create-time 'all'
// row EnsureProviderApp would have written).
func TestSetProviderEntitlementModeUser(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	admin, err := authSvc.Register(ctx, "admin@test.local", "admin", "quasar-fixture-pw-01")
	if err != nil {
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
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
		VALUES ('all', NULL, $1::uuid, 'provider', 'provider-app-ensure:steam')`, appID); err != nil {
		t.Fatalf("seed create-time all entitlement: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "user"}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", resp.StatusCode, body)
	}
	em := body["entitlement_mode"].(map[string]any)
	items := em["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want exactly one row", items)
	}
	row := items[0].(map[string]any)
	if row["subject_type"] != "user" {
		t.Errorf("subject_type = %v, want user", row["subject_type"])
	}
	if row["subject_id"] != admin.ID {
		t.Errorf("subject_id = %v, want the acting admin %v", row["subject_id"], admin.ID)
	}

	var subjectType string
	if err := pool.QueryRow(ctx, `SELECT subject_type FROM entitlements WHERE app_id::text = $1`, appID).Scan(&subjectType); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if subjectType != "user" {
		t.Errorf("stored subject_type = %q, want user", subjectType)
	}
}

// TestSetProviderEntitlementModeNone — "none" leaves the app with zero
// entitlement rows: present but invisible to everyone until an admin grants
// access, mirroring POST /v1/apps {"entitle":"none"}.
func TestSetProviderEntitlementModeNone(t *testing.T) {
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
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)
		RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO entitlements (subject_type, subject_id, app_id, granted_by, source_ref)
		VALUES ('all', NULL, $1::uuid, 'provider', 'provider-app-ensure:steam')`, appID); err != nil {
		t.Fatalf("seed create-time all entitlement: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "none"}, tok.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", resp.StatusCode, body)
	}
	em := body["entitlement_mode"].(map[string]any)
	items, _ := em["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("items = %v, want empty", items)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE app_id::text = $1`, appID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("entitlements row count = %d, want 0", n)
	}
}

// TestSetProviderEntitlementModeUnknownProviderIs404 — no app and no catalog
// image claims the provider: a clean 404, and nothing stored.
func TestSetProviderEntitlementModeUnknownProviderIs404(t *testing.T) {
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

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/nosuchprovider/entitlement-mode",
		map[string]any{"mode": "all"}, tok.Plaintext)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%v)", resp.StatusCode, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pending_provider_entitlement_modes`).Scan(&n); err != nil {
		t.Fatalf("count stored modes: %v", err)
	}
	if n != 0 {
		t.Errorf("stored modes = %d, want 0 for an unknown provider", n)
	}
}

// seedProviderCatalogImage makes the catalog name a provider without creating
// its app — the state between enabling discovery and EnsureProviderApp.
func seedProviderCatalogImage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, provider string) {
	t.Helper()
	id := "entitlement-mode-" + provider
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, library_provider, raw)
		VALUES ($1, 1, 'Provider', 'prebuilt', 'v1', 'registry.example.test/provider:v1', $2, '{}'::jsonb)`, id, provider); err != nil {
		t.Fatalf("seed catalog image: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM image_catalog WHERE id = $1`, id) })
}

// TestSetProviderEntitlementModeStoredBeforeTheAppExists — amendment 21 (#490):
// before the provider app exists the mode is stored (202) for EnsureProviderApp
// rather than lost to a 404; a second request replaces it, and both are audited.
func TestSetProviderEntitlementModeStoredBeforeTheAppExists(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newAuditedTestServer(t, pool)
	ctx := context.Background()
	tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
	var adminID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'admin@test.local'`).Scan(&adminID); err != nil {
		t.Fatalf("admin id: %v", err)
	}
	seedProviderCatalogImage(t, ctx, pool, "steam")

	for _, mode := range []string{"none", "user"} {
		resp, body := post(t, srv.URL+"/v1/admin/library-providers/Steam/entitlement-mode",
			map[string]any{"mode": mode}, tok)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("mode %s: want 202, got %d (%v)", mode, resp.StatusCode, body)
		}
		pm, ok := body["pending_entitlement_mode"].(map[string]any)
		if !ok || pm["provider"] != "steam" || pm["mode"] != mode {
			t.Fatalf("mode %s: pending_entitlement_mode = %v", mode, body)
		}
		details, _ := auditDetails(t, pool, "app.entitlement.set_mode")
		if details["mode"] != mode || details["pending"] != true {
			t.Errorf("mode %s: audit details = %v", mode, details)
		}
	}

	var mode string
	var requestedBy *string
	if err := pool.QueryRow(ctx, `
		SELECT mode, requested_by::text FROM pending_provider_entitlement_modes WHERE provider = 'steam'`).
		Scan(&mode, &requestedBy); err != nil {
		t.Fatalf("read stored mode: %v", err)
	}
	if mode != "user" || requestedBy == nil || *requestedBy != adminID {
		t.Errorf("stored = (%q, %v), want (user, %s): the later request replaces the earlier one", mode, requestedBy, adminID)
	}
	var apps int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE library_provider = 'steam'`).Scan(&apps); err != nil {
		t.Fatalf("count apps: %v", err)
	}
	if apps != 0 {
		t.Errorf("provider apps = %d, want 0: storing a mode must not create the app", apps)
	}
}

// TestSetProviderEntitlementModeOnAnExistingAppClearsAStoredMode — once the app
// exists the route applies immediately (200), and a stored request it
// supersedes is dropped so it can never be applied later.
func TestSetProviderEntitlementModeOnAnExistingAppClearsAStoredMode(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")

	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)`); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO pending_provider_entitlement_modes (provider, mode) VALUES ('steam', 'all')`); err != nil {
		t.Fatalf("seed stored mode: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "none"}, tok)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d (%v)", resp.StatusCode, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pending_provider_entitlement_modes`).Scan(&n); err != nil {
		t.Fatalf("count stored modes: %v", err)
	}
	if n != 0 {
		t.Errorf("stored modes = %d, want 0 after an immediate apply", n)
	}
}

// TestSetProviderEntitlementModeInvalidModeIs400 — an unrecognised mode value
// must be rejected, not silently coerced.
func TestSetProviderEntitlementModeInvalidModeIs400(t *testing.T) {
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

	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)`); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "everyone"}, tok.Plaintext)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%v)", resp.StatusCode, body)
	}
}

// TestSetProviderEntitlementModeRequiresAdmin — server-enforced, per
// CLAUDE.md invariant #6: a non-admin bearer token is refused regardless of
// what the client believes about its own role.
func TestSetProviderEntitlementModeRequiresAdmin(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()

	if _, err := authSvc.Register(ctx, "user@test.local", "user", "quasar-fixture-pw-01"); err != nil {
		t.Fatalf("register user: %v", err)
	}
	tok, err := authSvc.Login(ctx, "user@test.local", "quasar-fixture-pw-01", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO apps (name, kind, library_provider, enabled, managed_home, runtime_spec)
		VALUES ('Steam', 'launcher', 'steam', true, true, '{"gpu":true}'::jsonb)`); err != nil {
		t.Fatalf("seed provider app: %v", err)
	}

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "all"}, tok.Plaintext)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %d (%v)", resp.StatusCode, body)
	}
}

// TestStoredModeSurvivesAMixedCaseCatalogProvider — the catalog's
// library_provider has no lowercase CHECK. A mode stored through the route for
// "steam" must still be found when EnsureProviderApp runs for a catalog row
// saying "Steam": same lock key, same DELETE key, so no fall-back to 'all'.
func TestStoredModeSurvivesAMixedCaseCatalogProvider(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
	var adminID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'admin@test.local'`).Scan(&adminID); err != nil {
		t.Fatalf("admin id: %v", err)
	}

	const imageID = "entitlement-mode-mixed-case"
	seedInstalledProviderImage(t, ctx, pool, imageID, "Steam")

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "user"}, tok)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d (%v)", resp.StatusCode, body)
	}

	created, err := images.NewStoreWithFetcher(pool, nil).EnsureProviderApp(ctx, imageID, "Steam")
	if err != nil || !created {
		t.Fatalf("EnsureProviderApp = (%v, %v), want a created app", created, err)
	}

	rows, err := pool.Query(ctx, `
		SELECT e.subject_type, e.subject_id::text FROM entitlements e
		JOIN apps a ON a.id = e.app_id WHERE a.library_provider = 'steam'`)
	if err != nil {
		t.Fatalf("read entitlements: %v", err)
	}
	type grant struct {
		subjectType string
		subjectID   *string
	}
	var grants []grant
	for rows.Next() {
		var g grant
		if err := rows.Scan(&g.subjectType, &g.subjectID); err != nil {
			t.Fatalf("scan entitlement: %v", err)
		}
		grants = append(grants, g)
	}
	rows.Close()
	if len(grants) != 1 || grants[0].subjectType != "user" || grants[0].subjectID == nil || *grants[0].subjectID != adminID {
		t.Fatalf("entitlements = %+v, want exactly ('user', %s) and no 'all' row", grants, adminID)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pending_provider_entitlement_modes`).Scan(&left); err != nil {
		t.Fatalf("count stored modes: %v", err)
	}
	if left != 0 {
		t.Errorf("stored modes after create = %d, want 0", left)
	}
}

// TestStoredModeAppliesToAProviderAppMadeByHand — an admin can make the
// provider app themselves (POST /v1/apps, or PATCH /v1/apps setting
// library_provider), after which EnsureProviderApp never runs its create path.
// That write must consume the stored mode like EnsureProviderApp does; an
// explicit entitle on POST wins and drops it.
func TestStoredModeAppliesToAProviderAppMadeByHand(t *testing.T) {
	for _, tc := range []struct {
		name, stored string
		write        func(t *testing.T, srv, tok string, pool *pgxpool.Pool) string
		want         []string // subject_type of each grant, "user" meaning the admin
	}{
		{"POST without entitle", "none", func(t *testing.T, srv, tok string, _ *pgxpool.Pool) string {
			return postProviderApp(t, srv, tok, map[string]any{})
		}, nil},
		{"POST with explicit entitle", "none", func(t *testing.T, srv, tok string, _ *pgxpool.Pool) string {
			return postProviderApp(t, srv, tok, map[string]any{"entitle": "all"})
		}, []string{"all"}},
		{"PATCH setting library_provider", "user", func(t *testing.T, srv, tok string, pool *pgxpool.Pool) string {
			var appID string
			if err := pool.QueryRow(context.Background(), `
				INSERT INTO apps (name, kind, enabled, runtime_spec) VALUES ('Steam', 'launcher', true, '{}'::jsonb)
				RETURNING id::text`).Scan(&appID); err != nil {
				t.Fatalf("seed app: %v", err)
			}
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO entitlements (subject_type, app_id, granted_by) VALUES ('all', $1::uuid, 'admin')`, appID); err != nil {
				t.Fatalf("seed all grant: %v", err)
			}
			resp, body := patch(t, srv+"/v1/apps/"+appID, map[string]any{"library_provider": "steam"}, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PATCH: want 200, got %d (%v)", resp.StatusCode, body)
			}
			return appID
		}, []string{"user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			srv, authSvc := newTestServer(t, pool)
			ctx := context.Background()
			tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
			setLibraryDiscovery(t, pool, true)
			seedProviderCatalogImage(t, ctx, pool, "steam")

			resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
				map[string]any{"mode": tc.stored}, tok)
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("store mode: want 202, got %d (%v)", resp.StatusCode, body)
			}
			appID := tc.write(t, srv.URL, tok, pool)

			var got []string
			rows, err := pool.Query(ctx, `
				SELECT e.subject_type FROM entitlements e
				LEFT JOIN users u ON u.id = e.subject_id
				WHERE e.app_id = $1::uuid AND (e.subject_type = 'all' OR u.email = 'admin@test.local')
				ORDER BY 1`, appID)
			if err != nil {
				t.Fatalf("read entitlements: %v", err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, s)
			}
			rows.Close()
			var total int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM entitlements WHERE app_id = $1::uuid`, appID).Scan(&total); err != nil {
				t.Fatalf("count entitlements: %v", err)
			}
			if total != len(tc.want) || strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("entitlements = %v (%d rows), want %v", got, total, tc.want)
			}
			var left int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM pending_provider_entitlement_modes`).Scan(&left); err != nil {
				t.Fatalf("count stored modes: %v", err)
			}
			if left != 0 {
				t.Errorf("stored modes = %d, want 0", left)
			}
		})
	}
}

func postProviderApp(t *testing.T, srv, tok string, extra map[string]any) string {
	t.Helper()
	req := map[string]any{"name": "Steam", "kind": "launcher", "library_provider": "steam"}
	for k, v := range extra {
		req[k] = v
	}
	resp, body := post(t, srv+"/v1/apps", req, tok)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /v1/apps: want 201, got %d (%v)", resp.StatusCode, body)
	}
	app, _ := body["app"].(map[string]any)
	id, _ := app["id"].(string)
	return id
}

// seedInstalledProviderImage is a catalog image claiming provider, adopted, so
// images.EnsureProviderApp can create its app.
func seedInstalledProviderImage(t *testing.T, ctx context.Context, pool *pgxpool.Pool, imageID, provider string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, library_provider, raw)
		VALUES ($1, 1, 'Steam', 'prebuilt', 'v1', 'registry.example.test/steam:v1', $2, '{}'::jsonb)`, imageID, provider); err != nil {
		t.Fatalf("seed catalog image: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM image_catalog WHERE id = $1`, imageID) })
	if _, err := pool.Exec(ctx, `
		INSERT INTO installed_images (image_id, version, registry_ref) VALUES ($1, 'v1', 'registry.example.test/steam:v1')`, imageID); err != nil {
		t.Fatalf("seed adoption: %v", err)
	}
}

// TestOneProviderAppPerProvider — EnsureProviderApp and the entitlement-mode
// route address "the" provider app, so POST/PATCH /v1/apps refuse a second
// one under the shared lock, in either order of arrival; an app that already
// is the provider's (including a duplicate made before the rule) stays
// editable.
func TestOneProviderAppPerProvider(t *testing.T) {
	const imageID = "one-provider-app"
	for _, tc := range []struct {
		name     string
		wantApps int
		run      func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srv, tok string)
	}{
		{"EnsureProviderApp first, then POST is 409", 1, func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srv, tok string) {
			ensureProviderApp(t, ctx, pool, imageID, true)
			resp, body := post(t, srv+"/v1/apps", map[string]any{"name": "Steam 2", "kind": "launcher", "library_provider": "steam"}, tok)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("second provider app: want 409, got %d (%v)", resp.StatusCode, body)
			}
		}},
		{"EnsureProviderApp first, then PATCH is 409", 1, func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srv, tok string) {
			ensureProviderApp(t, ctx, pool, imageID, true)
			other := seedPlainApp(t, ctx, pool, "")
			resp, body := patch(t, srv+"/v1/apps/"+other, map[string]any{"library_provider": "steam"}, tok)
			if resp.StatusCode != http.StatusConflict {
				t.Fatalf("PATCH into a second provider app: want 409, got %d (%v)", resp.StatusCode, body)
			}
		}},
		{"POST first, then EnsureProviderApp adopts it", 1, func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srv, tok string) {
			postProviderApp(t, srv, tok, map[string]any{})
			ensureProviderApp(t, ctx, pool, imageID, false)
		}},
		{"an app that already is the provider's stays editable", 2, func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, srv, tok string) {
			first := seedPlainApp(t, ctx, pool, "steam")
			seedPlainApp(t, ctx, pool, "steam") // a duplicate made before the rule
			resp, body := patch(t, srv+"/v1/apps/"+first, map[string]any{"library_provider": "steam", "name": "Steam (main)"}, tok)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("PATCH the existing provider app: want 200, got %d (%v)", resp.StatusCode, body)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			srv, authSvc := newTestServer(t, pool)
			ctx := context.Background()
			tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
			setLibraryDiscovery(t, pool, true)
			seedInstalledProviderImage(t, ctx, pool, imageID, "steam")

			tc.run(t, ctx, pool, srv.URL, tok)
			if got := countSteamApps(t, ctx, pool); got != tc.wantApps {
				t.Errorf("steam apps = %d, want %d", got, tc.wantApps)
			}
		})
	}
}

// TestFailedAllowListWriteKeepsTheStoredMode — the allow-list commits with the
// app, its grants and the consumed stored mode, or none of them do: a create
// that fails late must not have used up the restriction EnsureProviderApp
// would otherwise apply.
func TestFailedAllowListWriteKeepsTheStoredMode(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO pending_provider_entitlement_modes (provider, mode) VALUES ('steam', 'none')`); err != nil {
		t.Fatalf("store mode: %v", err)
	}
	kind, provider := "launcher", "steam"
	missingProfile := "00000000-0000-0000-0000-000000000490"
	s := &store{pool: pool}
	if _, err := s.createApp(ctx, "Steam", "", nil, &kind, nil, nil, nil, &provider,
		nil, nil, nil, nil, nil, nil, nil, false, "", nil, "", nil, "", []string{missingProfile}, ""); err == nil {
		t.Fatal("createApp with a launch profile that does not exist: want an error")
	}
	if n := countSteamApps(t, ctx, pool); n != 0 {
		t.Errorf("steam apps = %d, want 0: the create must roll back whole", n)
	}
	var mode string
	if err := pool.QueryRow(ctx, `SELECT mode FROM pending_provider_entitlement_modes WHERE provider = 'steam'`).Scan(&mode); err != nil || mode != "none" {
		t.Errorf("stored mode = (%q, %v), want none still stored", mode, err)
	}
}

func ensureProviderApp(t *testing.T, ctx context.Context, pool *pgxpool.Pool, imageID string, wantCreated bool) {
	t.Helper()
	created, err := images.NewStoreWithFetcher(pool, nil).EnsureProviderApp(ctx, imageID, "steam")
	if err != nil || created != wantCreated {
		t.Fatalf("EnsureProviderApp = (%v, %v), want created=%v", created, err, wantCreated)
	}
}

func seedPlainApp(t *testing.T, ctx context.Context, pool *pgxpool.Pool, provider string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (name, kind, enabled, runtime_spec, library_provider)
		VALUES ('App', 'launcher', true, '{}'::jsonb, $1) RETURNING id::text`, provider).Scan(&id); err != nil {
		t.Fatalf("seed app: %v", err)
	}
	return id
}

func countSteamApps(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE library_provider = 'steam'`).Scan(&n); err != nil {
		t.Fatalf("count steam apps: %v", err)
	}
	return n
}

// TestClearingTheProviderAppWhileDiscoveryIsOn — un-marking the only provider
// app while discovery runs would have EnsureProviderApp create a new one open
// to everyone, so it is 409 provider_enabled; with discovery off, or another
// app still claiming the provider, the clear goes through.
func TestClearingTheProviderAppWhileDiscoveryIsOn(t *testing.T) {
	for _, tc := range []struct {
		name       string
		discovery  bool
		duplicate  bool
		wantStatus int
	}{
		{"discovery on, only provider app", true, false, http.StatusConflict},
		{"discovery off", false, false, http.StatusOK},
		{"discovery on, another provider app remains", true, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			srv, authSvc := newTestServer(t, pool)
			ctx := context.Background()
			tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
			setLibraryDiscovery(t, pool, tc.discovery)
			app := seedPlainApp(t, ctx, pool, "steam")
			if tc.duplicate {
				seedPlainApp(t, ctx, pool, "steam")
			}

			resp, body := patch(t, srv.URL+"/v1/apps/"+app, map[string]any{"library_provider": ""}, tok)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("clear library_provider: want %d, got %d (%v)", tc.wantStatus, resp.StatusCode, body)
			}
			var provider string
			if err := pool.QueryRow(ctx, `SELECT library_provider FROM apps WHERE id = $1::uuid`, app).Scan(&provider); err != nil {
				t.Fatalf("read app: %v", err)
			}
			if tc.wantStatus == http.StatusConflict {
				if code := errorCode(body); code != "provider_enabled" || provider != "steam" {
					t.Errorf("refused clear: code %q, library_provider %q; want provider_enabled and steam kept", code, provider)
				}
			} else if provider != "" {
				t.Errorf("library_provider = %q, want cleared", provider)
			}
		})
	}
}

// TestLockProviderAppNormalizesTheProvider — every caller normalizes today;
// the lock must not depend on it, or "Steam" and "steam" would not exclude
// each other.
func TestLockProviderAppNormalizesTheProvider(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := images.LockProviderApp(ctx, tx, " Steam "); err != nil {
		t.Fatalf("lock: %v", err)
	}
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin other: %v", err)
	}
	defer other.Rollback(ctx) //nolint:errcheck
	var got bool
	if err := other.QueryRow(ctx,
		`SELECT pg_try_advisory_xact_lock(hashtext('quasar_provider_app:' || 'steam')::bigint)`).Scan(&got); err != nil {
		t.Fatalf("try lock: %v", err)
	}
	if got {
		t.Error(`a lock taken for " Steam " did not exclude "steam"`)
	}
}

// TestClearRacingADesignationIsRefused — PATCH A designates a plain app as
// steam and is held open; PATCH B clears the same app. B must wait for A and
// then see the app as the provider's only app (409), not read the old empty
// provider, skip the guard and clear it after A commits.
func TestClearRacingADesignationIsRefused(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
	setLibraryDiscovery(t, pool, true)
	app := seedPlainApp(t, ctx, pool, "")

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer a.Rollback(ctx) //nolint:errcheck
	if err := images.LockProviderApp(ctx, a, "steam"); err != nil {
		t.Fatalf("A lock: %v", err)
	}
	if _, err := a.Exec(ctx, `UPDATE apps SET library_provider = 'steam' WHERE id = $1::uuid`, app); err != nil {
		t.Fatalf("A designate: %v", err)
	}

	type result struct {
		status int
		body   map[string]any
	}
	done := make(chan result, 1)
	go func() {
		resp, body := patch(t, srv.URL+"/v1/apps/"+app, map[string]any{"library_provider": ""}, tok)
		done <- result{resp.StatusCode, body}
	}()

	// B must be parked on a lock before A commits, or this is not the race.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("poll waiters: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PATCH B never waited on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}

	got := <-done
	if got.status != http.StatusConflict || errorCode(got.body) != "provider_enabled" {
		t.Fatalf("racing clear: want 409 provider_enabled, got %d (%v)", got.status, got.body)
	}
	var provider string
	if err := pool.QueryRow(ctx, `SELECT library_provider FROM apps WHERE id = $1::uuid`, app).Scan(&provider); err != nil {
		t.Fatalf("read app: %v", err)
	}
	if provider != "steam" {
		t.Errorf("library_provider = %q, want steam kept", provider)
	}
}

// TestRefusedPatchLeavesTheAllowList — the allow-list commits with the rest of
// the patch, so a clear refused with 409 must not have widened the app's menu.
func TestRefusedPatchLeavesTheAllowList(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newTestServer(t, pool)
	ctx := context.Background()
	tok := adminBearer(t, ctx, pool, authSvc, "admin@test.local", "admin")
	setLibraryDiscovery(t, pool, true)
	app := seedPlainApp(t, ctx, pool, "steam")
	if _, err := pool.Exec(ctx, `
		INSERT INTO app_launch_profiles (app_id, launch_profile_id) VALUES ($1::uuid, '720p60')`, app); err != nil {
		t.Fatalf("seed allow-list: %v", err)
	}

	resp, body := patch(t, srv.URL+"/v1/apps/"+app,
		map[string]any{"library_provider": "", "launchable_profile_ids": []string{}}, tok)
	if resp.StatusCode != http.StatusConflict || errorCode(body) != "provider_enabled" {
		t.Fatalf("want 409 provider_enabled, got %d (%v)", resp.StatusCode, body)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_launch_profiles WHERE app_id = $1::uuid`, app).Scan(&n); err != nil {
		t.Fatalf("count allow-list: %v", err)
	}
	if n != 1 {
		t.Errorf("allow-list rows = %d, want 1: a refused patch must not clear it", n)
	}
}
