package auth

// user_audit_test.go — the admin user-management routes were built without an
// auditor, so role changes, disables, quota edits and deletions left no trace
// (UI v3 amendment §3). Requires Postgres: make test-db.
//
// One row PER CHANGED FIELD, not one "user.updated": the three carry different
// severities and an operator reading the feed needs to know which happened.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/jackc/pgx/v5/pgxpool"
)

type auditRow struct {
	Actor   *string
	Action  string
	Target  string
	Details map[string]any
}

// auditRows reads the whole feed, oldest first.
func auditRows(t *testing.T, pool *pgxpool.Pool) []auditRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT actor_user_id::text, action, COALESCE(target_id, ''), details
		FROM admin_activity ORDER BY id`)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		var raw []byte
		if err := rows.Scan(&r.Actor, &r.Action, &r.Target, &raw); err != nil {
			t.Fatalf("scan activity: %v", err)
		}
		if err := json.Unmarshal(raw, &r.Details); err != nil {
			t.Fatalf("unmarshal details: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// newAuditedUserServer wires the admin user routes with a real audit store and
// returns the pool, an admin bearer token, and a second user's id to act on.
func newAuditedUserServer(t *testing.T) (*httptest.Server, *pgxpool.Pool, string, string, string) {
	t.Helper()
	pool := testDB(t)
	if _, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate activity: %v", err)
	}
	svc := testService(t, pool)
	ctx := context.Background()

	admin, err := svc.Register(ctx, "admin@audit.test", "adminuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, admin.ID); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	// A second admin, so demoting/deleting the subject is never the last-admin 409.
	subject, err := svc.Register(ctx, "subject@audit.test", "subjectuser", "quasar-fixture-pw-08")
	if err != nil {
		t.Fatalf("register subject: %v", err)
	}
	tok, err := svc.Login(ctx, "admin@audit.test", "quasar-fixture-pw-08", "")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}

	mux := http.NewServeMux()
	h := NewHandler(svc, audit.NewStore(pool))
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, pool, tok.Plaintext, admin.ID, subject.ID
}

// req builds a bearer-authenticated request with an optional JSON body.
func req(t *testing.T, method, url string, body any, bearer string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	r, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Authorization", "Bearer "+bearer)
	return r
}

func patchUser(t *testing.T, srv *httptest.Server, tok, id string, body any) {
	t.Helper()
	resp, _ := do(t, req(t, http.MethodPatch, srv.URL+"/v1/users/"+id, body, tok))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /v1/users/%s: got %d, want 200", id, resp.StatusCode)
	}
}

func TestUserMutationsAreAudited(t *testing.T) {
	srv, pool, tok, adminID, subjectID := newAuditedUserServer(t)

	patchUser(t, srv, tok, subjectID, map[string]any{"role": "admin"})
	patchUser(t, srv, tok, subjectID, map[string]any{"disabled": true})
	patchUser(t, srv, tok, subjectID, map[string]any{"disabled": false})
	patchUser(t, srv, tok, subjectID, map[string]any{"max_concurrent_sessions": 3})

	resp, _ := do(t, req(t, http.MethodDelete, srv.URL+"/v1/users/"+subjectID, nil, tok))
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /v1/users: got %d, want 204", resp.StatusCode)
	}

	rows := auditRows(t, pool)
	want := []string{"user.role_changed", "user.disabled", "user.enabled", "user.quota_changed", "user.deleted"}
	if len(rows) != len(want) {
		t.Fatalf("recorded %d rows (%v), want %d", len(rows), rowActions(rows), len(want))
	}
	for i, action := range want {
		if rows[i].Action != action {
			t.Errorf("row %d = %q, want %q", i, rows[i].Action, action)
		}
		if rows[i].Target != subjectID {
			t.Errorf("%s target_id = %q, want the subject %q", action, rows[i].Target, subjectID)
		}
		if rows[i].Actor == nil || *rows[i].Actor != adminID {
			t.Errorf("%s actor = %v, want the acting admin %q", action, rows[i].Actor, adminID)
		}
	}
	if got := rows[0].Details["role"]; got != "admin" {
		t.Errorf("user.role_changed details.role = %v, want admin", got)
	}
	if got := rows[3].Details["max_concurrent_sessions"]; got != float64(3) {
		t.Errorf("user.quota_changed details = %v, want max_concurrent_sessions 3", rows[3].Details)
	}
	// Nothing that could be a credential ever reaches details.
	for _, r := range rows {
		for _, k := range []string{"password", "password_hash", "email", "token"} {
			if _, ok := r.Details[k]; ok {
				t.Errorf("%s details carries %q", r.Action, k)
			}
		}
	}
}

// TestFailedUserMutationIsNotAudited: the feed records what happened, so a
// refused change must leave no row — otherwise an operator reads a demotion
// that never landed.
func TestFailedUserMutationIsNotAudited(t *testing.T) {
	srv, pool, tok, _, _ := newAuditedUserServer(t)

	missing := "00000000-0000-4000-8000-000000000000"
	resp, _ := do(t, req(t, http.MethodPatch, srv.URL+"/v1/users/"+missing,
		map[string]any{"role": "admin"}, tok))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PATCH a missing user: got %d, want 404", resp.StatusCode)
	}
	if rows := auditRows(t, pool); len(rows) != 0 {
		t.Errorf("a failed PATCH recorded %v", rowActions(rows))
	}
}

func rowActions(rows []auditRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Action)
	}
	return out
}
