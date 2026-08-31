package invites

// invites_audit_test.go — minting and revoking an invite were unrecorded (UI v3
// amendment §3). Requires Postgres: make test-db.
//
// The one thing that must never appear in the row: the plaintext code. It is
// shown to the minter exactly once, and admin_activity is readable by every
// admin forever.

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

func activityRows(t *testing.T, pool *pgxpool.Pool) []struct {
	Actor   string
	Action  string
	Target  string
	Details map[string]any
} {
	t.Helper()
	type row = struct {
		Actor   string
		Action  string
		Target  string
		Details map[string]any
	}
	rows, err := pool.Query(context.Background(), `
		SELECT COALESCE(actor_user_id::text, ''), action, COALESCE(target_id, ''), details
		FROM admin_activity ORDER BY id`)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
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

func TestInviteMintAndRevokeAreAudited(t *testing.T) {
	pool := testDB(t)
	if _, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate activity: %v", err)
	}
	ctx := context.Background()

	// The real gate, as every other handler test in this repo wires it: the
	// acting admin has to come from a bearer token, not a stub context.
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	u, err := authSvc.Register(ctx, "invites-admin@t.local", "invitesadmin", "password12345")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, u.ID); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	tok, err := authSvc.Login(ctx, "invites-admin@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	admin := u.ID

	authHandler := auth.NewHandler(authSvc)
	mux := http.NewServeMux()
	NewHandler(NewStore(pool), "", audit.NewStore(pool)).Register(mux,
		func(next http.Handler) http.Handler {
			return authHandler.RequireAuth(authHandler.RequireAdmin(next))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	do := func(method, path, body string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}

	resp := do(http.MethodPost, "/v1/admin/invites",
		`{"role":"user","max_uses":3,"note":"for the ops team"}`)
	var minted struct {
		Invite struct {
			ID   string `json:"id"`
			Code string `json:"code"`
		} `json:"invite"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil {
		t.Fatalf("decode mint: %v", err)
	}
	resp.Body.Close()
	if minted.Invite.Code == "" {
		t.Fatal("fixture is wrong: mint returned no code")
	}

	do(http.MethodDelete, "/v1/admin/invites/"+minted.Invite.ID, "").Body.Close()

	rows := activityRows(t, pool)
	if len(rows) != 2 {
		t.Fatalf("recorded %d rows, want 2 (mint + revoke)", len(rows))
	}
	if rows[0].Action != "invite.minted" || rows[1].Action != "invite.revoked" {
		t.Fatalf("actions = %q/%q, want invite.minted/invite.revoked", rows[0].Action, rows[1].Action)
	}
	for _, r := range rows {
		if r.Actor != admin {
			t.Errorf("%s actor = %q, want %q", r.Action, r.Actor, admin)
		}
		if r.Target != minted.Invite.ID {
			t.Errorf("%s target_id = %q, want %q", r.Action, r.Target, minted.Invite.ID)
		}
	}
	if rows[0].Details["role"] != "user" || rows[0].Details["max_uses"] != float64(3) {
		t.Errorf("invite.minted details = %v, want role=user max_uses=3", rows[0].Details)
	}
	// The code, the hash, and the operator's free-text note all stay out.
	raw, _ := json.Marshal(rows[0].Details)
	for _, forbidden := range []string{minted.Invite.Code, "for the ops team", "code_hash"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("invite.minted details leaked %q: %s", forbidden, raw)
		}
	}
}
