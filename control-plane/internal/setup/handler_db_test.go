// handler_db_test.go — the first-run setup surface exercised end-to-end against
// a real Postgres and the REAL RequireAuth→RequireAdmin chain: a claim must
// create an admin whose returned token then passes the same admin gate every
// other admin endpoint uses (CLAUDE.md invariant #6 — hiding admin UI is never
// the access control, so the gate itself must be real here).
package setup

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/internal/settings"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// dbHarness wires the real setup Service (backed by auth.Service + settings.Store)
// plus a tiny admin-only probe endpoint behind the real admin gate, on one mux.
type dbHarness struct {
	pool    *pgxpool.Pool
	setupS  *Service
	authSvc *auth.Service
	srv     *httptest.Server
	token   string
}

func newDBHarness(t *testing.T, token string) *dbHarness {
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, auth_tokens, instance_settings RESTART IDENTITY CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)
	settingsStore := settings.NewStore(pool)
	// Seed the instance_settings singleton so the setup_completed_at column can be
	// exercised (SetupCompleted also handles a missing row, covered by the empty-DB
	// assertion below).
	if err := settingsStore.Seed(ctx, settings.RegistrationClosed); err != nil {
		t.Fatalf("seed instance_settings: %v", err)
	}

	setupS := NewService(authSvc, settingsStore, token, quietLog())

	mux := http.NewServeMux()
	// The REAL admin gate, exactly as app.go composes it — so POST
	// /v1/setup/complete's 401/403 behaviour under test is the production chain,
	// not a stand-in (CLAUDE.md invariant #6).
	admin := func(next http.Handler) http.Handler {
		return authHandler.RequireAuth(authHandler.RequireAdmin(next))
	}
	setupS.Register(mux, admin)
	// A minimal admin-only probe: proves the claim token clears the real
	// RequireAuth→RequireAdmin gate.
	mux.Handle("GET /v1/admin/probe", admin(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &dbHarness{pool: pool, setupS: setupS, authSvc: authSvc, srv: srv, token: token}
}

func (h *dbHarness) claim(t *testing.T, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/setup/claim", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(TokenHeader, token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *dbHarness) status(t *testing.T) statusResponse {
	t.Helper()
	res, err := http.Get(h.srv.URL + "/v1/setup/status")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status code = %d, want 200", res.StatusCode)
	}
	var out statusResponse
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out
}

// TestStatusReflectsAdminAndCompletion drives GET /v1/setup/status against a
// real DB across the three meaningful states.
func TestStatusReflectsAdminAndCompletion(t *testing.T) {
	h := newDBHarness(t, "setup-token")
	ctx := context.Background()

	if s := h.status(t); s.AdminExists || s.SetupCompleted {
		t.Fatalf("empty DB status = %+v, want both false", s)
	}

	// Claim creates the first admin → admin_exists flips true.
	res := h.claim(t, "setup-token", `{"email":"o@e.com","username":"o","password":"correct-horse"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim status = %d, want 201", res.StatusCode)
	}
	if s := h.status(t); !s.AdminExists {
		t.Fatalf("status after claim = %+v, want admin_exists true", s)
	}
	if s := h.status(t); s.SetupCompleted {
		t.Fatalf("setup_completed = true before the column is set, want false")
	}

	// Marking the column flips setup_completed.
	if _, err := h.pool.Exec(ctx,
		`UPDATE instance_settings SET setup_completed_at = now() WHERE id = true`); err != nil {
		t.Fatalf("set setup_completed_at: %v", err)
	}
	if s := h.status(t); !s.SetupCompleted {
		t.Fatalf("status after marking complete = %+v, want setup_completed true", s)
	}
}

// TestClaimTokenClearsRealAdminGate is the end-to-end proof: a claimed token
// authenticates a subsequent RequireAuth→RequireAdmin request.
func TestClaimTokenClearsRealAdminGate(t *testing.T) {
	h := newDBHarness(t, "setup-token")

	res := h.claim(t, "setup-token", `{"email":"o@e.com","username":"o","password":"correct-horse"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim status = %d, want 201", res.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresAt   string `json:"expires_at"`
		User        struct {
			ID, Email, Username, Role string
		} `json:"user"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if body.AccessToken == "" || body.TokenType != "Bearer" || body.ExpiresAt == "" {
		t.Fatalf("claim response not login-shaped: %+v", body)
	}
	if body.User.Role != "admin" {
		t.Fatalf("claimed user role = %q, want admin", body.User.Role)
	}

	// The claim token must pass the real admin gate.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/v1/admin/probe", nil)
	req.Header.Set("Authorization", "Bearer "+body.AccessToken)
	probe, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Body.Close()
	if probe.StatusCode != http.StatusNoContent {
		t.Fatalf("admin probe with claim token = %d, want 204", probe.StatusCode)
	}
}

// TestClaimWrongTokenIs401DB confirms the wrong-token 401 over the real stack
// and that NO admin was created.
func TestClaimWrongTokenIs401DB(t *testing.T) {
	h := newDBHarness(t, "setup-token")

	res := h.claim(t, "nope", `{"email":"o@e.com","username":"o","password":"correct-horse"}`)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token claim = %d, want 401", res.StatusCode)
	}
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("admin count = %d after a rejected claim, want 0", n)
	}
	if s := h.status(t); s.AdminExists {
		t.Fatal("admin_exists true after a rejected claim")
	}
}

// completeAs issues POST /v1/setup/complete with the given bearer token ("" =
// unauthenticated).
func (h *dbHarness) completeAs(t *testing.T, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/v1/setup/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

// claimAdmin performs the first-run claim and returns the new admin's token.
func (h *dbHarness) claimAdmin(t *testing.T) string {
	t.Helper()
	res := h.claim(t, h.token, `{"email":"o@e.com","username":"o","password":"correct-horse"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("claim = %d, want 201", res.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	return body.AccessToken
}

// setupCompletedAt reads the raw column so a test can prove the timestamp did
// not move on a repeat call.
func (h *dbHarness) setupCompletedAt(t *testing.T) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := h.pool.QueryRow(context.Background(),
		`SELECT setup_completed_at FROM instance_settings WHERE id = true`).Scan(&ts); err != nil {
		t.Fatalf("read setup_completed_at: %v", err)
	}
	return ts
}

// TestCompleteRequiresAuth pins the contract's 401: unauthenticated is refused
// by the REAL RequireAuth, and nothing is written.
func TestCompleteRequiresAuth(t *testing.T) {
	h := newDBHarness(t, "setup-token")
	h.claimAdmin(t) // an admin exists; the gate is what refuses, not the state

	code, _ := h.completeAs(t, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated complete = %d, want 401", code)
	}
	if ts := h.setupCompletedAt(t); ts != nil {
		t.Fatalf("setup_completed_at = %v after a 401, want NULL", ts)
	}
	if s := h.status(t); s.SetupCompleted {
		t.Fatal("setup_completed true after an unauthenticated complete")
	}
}

// TestCompleteRequiresAdmin pins the contract's 403: a valid NON-admin bearer
// token is refused by the REAL RequireAdmin, and nothing is written. This is the
// server-enforced gate — hiding the wizard in the UI is never the access control.
func TestCompleteRequiresAdmin(t *testing.T) {
	h := newDBHarness(t, "setup-token")
	h.claimAdmin(t)

	ctx := context.Background()
	if _, err := h.authSvc.Register(ctx, "plain@e.com", "plain", "correct-horse"); err != nil {
		t.Fatalf("register plain user: %v", err)
	}
	tok, err := h.authSvc.Login(ctx, "plain@e.com", "correct-horse", "test")
	if err != nil {
		t.Fatalf("login plain user: %v", err)
	}
	if tok.User.Role != "user" {
		t.Fatalf("fixture user role = %q, want user", tok.User.Role)
	}

	code, _ := h.completeAs(t, tok.Plaintext)
	if code != http.StatusForbidden {
		t.Fatalf("non-admin complete = %d, want 403", code)
	}
	if ts := h.setupCompletedAt(t); ts != nil {
		t.Fatalf("setup_completed_at = %v after a 403, want NULL", ts)
	}
}

// TestCompleteIsIdempotentAndDoesNotMoveTheTimestamp is the headline property of
// the new route: two calls return the same body, and the SECOND must not rewrite
// when setup finished.
func TestCompleteIsIdempotentAndDoesNotMoveTheTimestamp(t *testing.T) {
	h := newDBHarness(t, "setup-token")
	admin := h.claimAdmin(t)

	code1, body1 := h.completeAs(t, admin)
	if code1 != http.StatusOK {
		t.Fatalf("first complete = %d, want 200 (body %s)", code1, body1)
	}
	first := h.setupCompletedAt(t)
	if first == nil {
		t.Fatal("setup_completed_at still NULL after complete")
	}

	// A gap so a rewrite would be unmistakable in the timestamp.
	time.Sleep(50 * time.Millisecond)

	code2, body2 := h.completeAs(t, admin)
	if code2 != http.StatusOK {
		t.Fatalf("second complete = %d, want 200", code2)
	}
	if body1 != body2 {
		t.Fatalf("not idempotent: first body %q != second body %q", body1, body2)
	}
	second := h.setupCompletedAt(t)
	if second == nil {
		t.Fatal("setup_completed_at became NULL")
	}
	if !first.Equal(*second) {
		t.Fatalf("setup_completed_at MOVED on the second call: %v → %v — completion time must be stable",
			first, second)
	}

	// And status agrees.
	if s := h.status(t); !s.SetupCompleted || !s.AdminExists {
		t.Fatalf("status after complete = %+v, want both true", s)
	}
}

// TestClaimAlreadyCompleteIs409DB confirms the runtime self-disable: a second
// claim (token still valid this boot) is refused 409 setup_already_complete.
func TestClaimAlreadyCompleteIs409DB(t *testing.T) {
	h := newDBHarness(t, "setup-token")

	first := h.claim(t, "setup-token", `{"email":"a@e.com","username":"a","password":"correct-horse"}`)
	first.Body.Close()
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first claim = %d, want 201", first.StatusCode)
	}

	second := h.claim(t, "setup-token", `{"email":"b@e.com","username":"b","password":"correct-horse"}`)
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second claim = %d, want 409", second.StatusCode)
	}
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	if err := json.NewDecoder(second.Body).Decode(&env); err != nil {
		t.Fatalf("decode 409: %v", err)
	}
	if env.Error.Code != "setup_already_complete" {
		t.Fatalf("409 code = %q, want setup_already_complete", env.Error.Code)
	}
	var n int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("admin count = %d after two claims, want 1", n)
	}
}
