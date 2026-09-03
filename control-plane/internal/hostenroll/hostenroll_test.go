package hostenroll

// Requires Postgres: make test-db.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE host_enrollments, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedAdmin(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('enroll-admin@x.io','enrolladmin','x','admin') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return id
}

// adminClient wires the real gate — the acting admin comes from a bearer token, not a
// stub context — and returns a function that performs one authenticated request.
func adminClient(t *testing.T, pool *pgxpool.Pool) func(method, path, body string) *http.Response {
	t.Helper()
	ctx := context.Background()
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	u, err := authSvc.Register(ctx, "enroll-http@t.local", "enrollhttp", "password12345")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role='admin' WHERE id::text=$1`, u.ID); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	tok, err := authSvc.Login(ctx, "enroll-http@t.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login: %v", err)
	}

	authHandler := auth.NewHandler(authSvc)
	mux := http.NewServeMux()
	NewHandler(NewStore(pool), audit.NewStore(pool)).Register(mux,
		func(next http.Handler) http.Handler {
			return authHandler.RequireAuth(authHandler.RequireAdmin(next))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return func(method, path, body string) *http.Response {
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
}

// An empty node_name is the dangerous silent success: MintParams reads "" as unbound, so
// it would hand back a token usable by ANY machine to a caller that asked to bind one.
func TestMintRejectsAnEmptyNodeName(t *testing.T) {
	pool := testDB(t)
	do := adminClient(t, pool)

	resp := do(http.MethodPost, "/v1/admin/hosts/enrollments", `{"node_name":""}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "validation_failed" {
		t.Fatalf("code = %q, want validation_failed", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "node_name") {
		t.Fatalf("message = %q, want it to name node_name", body.Error.Message)
	}
}

// Caps on both bounds of a fleet-join credential (control-api.md §Host enrollment tokens).
func TestMintCapsUsesAndLifetime(t *testing.T) {
	pool := testDB(t)
	do := adminClient(t, pool)

	far := time.Now().Add(31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	near := time.Now().Add(29 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"max_uses above the cap", `{"max_uses":101}`, http.StatusBadRequest},
		{"max_uses at the cap", `{"max_uses":100}`, http.StatusCreated},
		{"expires_at beyond 30 days", `{"expires_at":"` + far + `"}`, http.StatusBadRequest},
		{"expires_at within 30 days", `{"expires_at":"` + near + `"}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(http.MethodPost, "/v1/admin/hosts/enrollments", tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// last_used_at says a redemption happened; used_by_node_name says by whom (0073). It is
// the fact an operator needs when a token turns out to have been used unexpectedly.
func TestRedeemRecordsTheRedeemingNode(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	s := NewStore(pool)

	row, plaintext, err := s.Mint(ctx, MintParams{CreatedBy: admin, MaxUses: 2})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if row.UsedByNodeName != nil {
		t.Fatalf("freshly minted token already names a redeemer: %v", *row.UsedByNodeName)
	}
	if err := Redeem(ctx, pool, plaintext, "gpu-host-07"); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	list, err := s.List(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("listed %d rows, want 1", len(list))
	}
	if list[0].UsedByNodeName == nil || *list[0].UsedByNodeName != "gpu-host-07" {
		t.Fatalf("used_by_node_name = %v, want gpu-host-07", list[0].UsedByNodeName)
	}
}

// ?state=pending must list exactly the rows Redeem would accept, and the filter runs
// unqualified inside a LEFT JOIN with users — a column collision would fail here.
func TestListPendingMatchesWhatWouldRedeem(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	s := NewStore(pool)

	live, _, err := s.Mint(ctx, MintParams{CreatedBy: admin, Note: "live"})
	if err != nil {
		t.Fatalf("mint live: %v", err)
	}
	dead, _, err := s.Mint(ctx, MintParams{CreatedBy: admin, Note: "revoked"})
	if err != nil {
		t.Fatalf("mint revoked: %v", err)
	}
	if err := s.Revoke(ctx, dead.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if _, _, err := s.Mint(ctx, MintParams{CreatedBy: admin, ExpiresAt: &past, Note: "expired"}); err != nil {
		t.Fatalf("mint expired: %v", err)
	}

	pending, err := s.List(ctx, true)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != live.ID {
		t.Fatalf("pending = %d rows (%+v), want only the live one", len(pending), pending)
	}
	all, err := s.List(ctx, false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all = %d rows, want 3", len(all))
	}
}

// The row must outlive its minter (0073): tokens were minted by ephemeral DX admin
// identities and cascaded away mid-enrollment when the reaper deleted them.
func TestEnrollmentSurvivesItsMinter(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	ctx := context.Background()
	s := NewStore(pool)

	row, plaintext, err := s.Mint(ctx, MintParams{CreatedBy: admin, NodeName: "gpu-host-09"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, admin); err != nil {
		t.Fatalf("delete minter: %v", err)
	}

	list, err := s.List(ctx, true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != row.ID {
		t.Fatalf("the token vanished with its minter: %+v", list)
	}
	if list[0].CreatedByUserID != nil {
		t.Fatalf("created_by_user_id = %v, want null once the minter is gone", *list[0].CreatedByUserID)
	}
	// And it still redeems: the credential a host is mid-enrollment with must survive.
	if err := Redeem(ctx, pool, plaintext, "gpu-host-09"); err != nil {
		t.Fatalf("redeem after the minter was deleted: %v", err)
	}
}
