// provider_entitlement_mode_db_test.go — #465. DB-backed: exercises the real
// entitlements table through the HTTP surface, same pattern as
// provider_suspension_test.go and entitlements_test.go's siblings.
package crud

import (
	"context"
	"net/http"
	"testing"
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

// TestSetProviderEntitlementModeUnknownProviderIs404 — the wizard's most
// likely race: calling this before EnsureProviderApp's async pass has created
// the app yet. Must be a clean 404, not a 500 or a silent no-op.
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

	resp, body := post(t, srv.URL+"/v1/admin/library-providers/steam/entitlement-mode",
		map[string]any{"mode": "all"}, tok.Plaintext)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d (%v)", resp.StatusCode, body)
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
