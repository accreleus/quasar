package settings

// settings_audit_test.go — PATCH /v1/admin/settings was unrecorded (UI v3
// amendment §3). Requires Postgres: make test-db.
//
// The row carries CHANGED KEY NAMES and nothing else. allowed_origins is
// operator text and this surface will grow; a log that records values is one
// careless field away from being where a secret lands.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// auditedPatch wires the settings handler with a real audit store and returns a
// PATCH function plus the acting admin's id.
func auditedPatch(t *testing.T, pool *pgxpool.Pool) (func(body string) int, string) {
	t.Helper()
	ctx := context.Background()
	store := NewStore(pool)
	must(t, store.Seed(ctx, RegistrationClosed))
	must(t, execT(ctx, pool, `DELETE FROM admin_activity`))

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	must(t, err)
	u, err := authSvc.Register(ctx, "audit-admin@t.local", "auditadmin", "password12345")
	must(t, err)
	must(t, execT(ctx, pool, `UPDATE users SET role='admin' WHERE email='audit-admin@t.local'`))
	tok, err := authSvc.Login(ctx, "audit-admin@t.local", "password12345", "test")
	must(t, err)

	authHandler := auth.NewHandler(authSvc)
	mux := http.NewServeMux()
	NewHandler(store, audit.NewStore(pool)).Register(mux, func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return func(body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPatch, srv.URL+"/v1/admin/settings", strings.NewReader(body))
		must(t, err)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}, u.ID
}

func settingsAuditRows(t *testing.T, pool *pgxpool.Pool) []struct {
	Actor, Action, TargetType, Details string
} {
	t.Helper()
	type row = struct{ Actor, Action, TargetType, Details string }
	rows, err := pool.Query(context.Background(), `
		SELECT COALESCE(actor_user_id::text, ''), action, target_type, details::text
		FROM admin_activity ORDER BY id`)
	must(t, err)
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		must(t, rows.Scan(&r.Actor, &r.Action, &r.TargetType, &r.Details))
		out = append(out, r)
	}
	return out
}

func TestSettingsPatchIsAuditedByKeyNameOnly(t *testing.T) {
	pool := testDB(t)
	patch, admin := auditedPatch(t, pool)

	if code := patch(`{"registration_mode":"open","allowed_origins":["https://console.example.test"]}`); code != http.StatusOK {
		t.Fatalf("PATCH: got %d, want 200", code)
	}

	rows := settingsAuditRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("recorded %d rows, want 1", len(rows))
	}
	r := rows[0]
	if r.Action != "instance.settings.updated" || r.TargetType != "instance" {
		t.Fatalf("row = %s/%s, want instance.settings.updated/instance", r.Action, r.TargetType)
	}
	if r.Actor != admin {
		t.Errorf("actor = %q, want %q", r.Actor, admin)
	}

	var details struct {
		Keys []string `json:"keys"`
	}
	if err := json.Unmarshal([]byte(r.Details), &details); err != nil {
		t.Fatalf("unmarshal details: %v", err)
	}
	want := map[string]bool{"registration_mode": true, "allowed_origins": true}
	if len(details.Keys) != len(want) {
		t.Fatalf("keys = %v, want %v", details.Keys, want)
	}
	for _, k := range details.Keys {
		if !want[k] {
			t.Errorf("keys carries an unchanged field %q", k)
		}
	}
	// The VALUES must not be there — not the mode, not the origin.
	for _, forbidden := range []string{"open", "console.example.test"} {
		if strings.Contains(r.Details, forbidden) {
			t.Errorf("details leaked a value (%q): %s", forbidden, r.Details)
		}
	}
}

// TestRejectedSettingsPatchIsNotAudited: a 400 changed nothing, so the feed must
// not claim otherwise.
func TestRejectedSettingsPatchIsNotAudited(t *testing.T) {
	pool := testDB(t)
	patch, _ := auditedPatch(t, pool)

	if code := patch(`{"registration_mode":"whenever"}`); code != http.StatusBadRequest {
		t.Fatalf("invalid PATCH: got %d, want 400", code)
	}
	if rows := settingsAuditRows(t, pool); len(rows) != 0 {
		t.Errorf("a rejected PATCH recorded %d rows", len(rows))
	}
}
