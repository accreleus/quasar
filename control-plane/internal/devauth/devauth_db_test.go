package devauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// testDB mirrors internal/auth's helper: skipped without TEST_DATABASE_URL so
// `go test ./...` stays green with no database, exercised for real by
// `make test-db`.
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, auth_tokens, user_devices, sessions, apps, hosts RESTART IDENTITY CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func dbService(t *testing.T, pool *pgxpool.Pool) (*Service, *auth.Service) {
	t.Helper()
	// Real argon2 params are slow; the auth package's test params are unexported,
	// so use the production defaults — one mint per test is affordable.
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), 24*time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	return NewService(authSvc, testSecret, testLogger()), authSvc
}

type mintResult struct {
	AccessToken string            `json:"access_token"`
	ExpiresAt   string            `json:"expires_at"`
	User        auth.User         `json:"user"`
	StorageKeys map[string]string `json:"storage_keys"`
}

func mint(t *testing.T, svc *Service, body string) mintResult {
	t.Helper()
	w := post(t, svc, testSecret, body)
	if w.Code != http.StatusOK {
		t.Fatalf("mint: want 200, got %d (%s)", w.Code, w.Body)
	}
	var out mintResult
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode mint response: %v (%s)", err, w.Body)
	}
	return out
}

// TestMintCreatesAnEphemeralUserRow: the identity is REAL (a users row) and
// MARKED (ephemeral_expires_at set to the returned expiry).
func TestMintCreatesAnEphemeralUserRow(t *testing.T) {
	pool := testDB(t)
	svc, _ := dbService(t, pool)
	ctx := context.Background()

	got := mint(t, svc, `{"ttl_seconds":600}`)

	var email, username, role string
	var expires *time.Time
	if err := pool.QueryRow(ctx, `
		SELECT email, username, role, ephemeral_expires_at FROM users WHERE id::text = $1
	`, got.User.ID).Scan(&email, &username, &role, &expires); err != nil {
		t.Fatalf("minted user row not found: %v", err)
	}
	if expires == nil {
		t.Fatal("ephemeral_expires_at is NULL — the reaper would never take this row")
	}
	wantExpiry, err := time.Parse(time.RFC3339, got.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", got.ExpiresAt, err)
	}
	if d := expires.Sub(wantExpiry); d > time.Second || d < -time.Second {
		t.Fatalf("row expiry %v != response expires_at %v", expires, wantExpiry)
	}
	if role != auth.RoleUser {
		t.Fatalf("default role = %q, want user", role)
	}
	if want := "agent-"; len(email) < len(want) || email[:len(want)] != want {
		t.Fatalf("email = %q, want agent-<uuid>@dev.invalid", email)
	}
	if suffix := "@dev.invalid"; email[len(email)-len(suffix):] != suffix {
		t.Fatalf("email = %q, want the @dev.invalid domain", email)
	}
	if username == "" {
		t.Fatal("username is empty")
	}

	// The password is never returned anywhere in the response.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "password") {
		t.Fatalf("response mentions a password: %s", raw)
	}
}

// TestMintedTokenAuthenticates: the token is a REAL bearer token on the normal
// auth path — nothing about this endpoint bypasses authentication.
func TestMintedTokenAuthenticates(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)

	got := mint(t, svc, "")
	user, _, err := authSvc.Authenticate(context.Background(), got.AccessToken)
	if err != nil {
		t.Fatalf("minted token does not authenticate: %v", err)
	}
	if user.ID != got.User.ID {
		t.Fatalf("token belongs to %s, want %s", user.ID, got.User.ID)
	}
}

// TestReaperDeletesExpiredIdentityWithSessionsAndDevices is the acceptance line:
// once the TTL lapses the user is gone AND its sessions, tokens and device
// bindings went with it (migration 0007's cascade).
func TestReaperDeletesExpiredIdentityWithSessionsAndDevices(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	got := mint(t, svc, `{"ttl_seconds":600}`)

	// Give the identity a device binding and a session, exactly as a real run would.
	var deviceID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO user_devices (user_id, device_key) VALUES ($1::uuid, 'dev-key-1') RETURNING id::text
	`, got.User.ID).Scan(&deviceID); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	var appID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO apps (name) VALUES ('reaper-test-app') RETURNING id::text
	`).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var sessionID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, 'stopped', 1920, 1080, 60, 15000)
		RETURNING id::text
	`, got.User.ID, appID).Scan(&sessionID); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Not yet expired: the reaper must leave it alone.
	if rep, err := authSvc.ReapEphemeral(ctx); err != nil || rep.Deleted != 0 {
		t.Fatalf("premature reap: %+v err=%v", rep, err)
	}

	// Age the identity past its TTL.
	if _, err := pool.Exec(ctx,
		`UPDATE users SET ephemeral_expires_at = now() - interval '1 second' WHERE id::text = $1`,
		got.User.ID); err != nil {
		t.Fatalf("age identity: %v", err)
	}

	rep, err := authSvc.ReapEphemeral(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if rep.Deleted != 1 {
		t.Fatalf("reaped %+v, want Deleted=1", rep)
	}

	for _, q := range []struct{ name, sql string }{
		{"user", `SELECT count(*) FROM users WHERE id::text = $1`},
		{"auth_tokens", `SELECT count(*) FROM auth_tokens WHERE user_id::text = $1`},
		{"user_devices", `SELECT count(*) FROM user_devices WHERE user_id::text = $1`},
		{"sessions", `SELECT count(*) FROM sessions WHERE user_id::text = $1`},
	} {
		var c int
		if err := pool.QueryRow(ctx, q.sql, got.User.ID).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", q.name, err)
		}
		if c != 0 {
			t.Fatalf("%d %s row(s) survived the reap", c, q.name)
		}
	}

	// And the token dies with the identity — immediately, no grace.
	if _, _, err := authSvc.Authenticate(ctx, got.AccessToken); !errors.Is(err, auth.ErrUserNotFound) {
		t.Fatalf("reaped identity's token still authenticates (err=%v)", err)
	}
}

// TestReaperLeavesRealAccountsAlone: an expired-looking real account cannot exist
// (ephemeral_expires_at is NULL), and the reaper must never touch one.
func TestReaperLeavesRealAccountsAlone(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	human, err := authSvc.Register(ctx, "human@example.com", "human", "correct-horse-battery")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	got := mint(t, svc, `{"ttl_seconds":60}`)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET ephemeral_expires_at = now() - interval '1 hour' WHERE id::text = $1`,
		got.User.ID); err != nil {
		t.Fatalf("age identity: %v", err)
	}

	if rep, err := authSvc.ReapEphemeral(ctx); err != nil || rep.Deleted != 1 {
		t.Fatalf("reap: %+v err=%v, want Deleted=1", rep, err)
	}
	var c int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id::text = $1`, human.ID).Scan(&c); err != nil {
		t.Fatalf("count real user: %v", err)
	}
	if c != 1 {
		t.Fatal("the reaper deleted a real account")
	}
}

// TestAdminMintReachesAdminSurfaceAndUserMintDoesNot exercises the REAL gate:
// auth.RequireAuth → RequireAdmin, unchanged, with a minted token in the header.
func TestAdminMintReachesAdminSurfaceAndUserMintDoesNot(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)

	authHandler := auth.NewHandler(authSvc)
	adminOnly := authHandler.RequireAuth(authHandler.RequireAdmin(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})))

	call := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/probe", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		adminOnly.ServeHTTP(w, req)
		return w.Code
	}

	adminTok := mint(t, svc, `{"role":"admin"}`)
	if adminTok.User.Role != auth.RoleAdmin {
		t.Fatalf("role=admin mint returned role %q", adminTok.User.Role)
	}
	if code := call(adminTok.AccessToken); code != http.StatusNoContent {
		t.Fatalf("admin mint on an admin-gated handler: got %d, want 204", code)
	}

	userTok := mint(t, svc, `{"role":"user"}`)
	if code := call(userTok.AccessToken); code != http.StatusForbidden {
		t.Fatalf("user mint on an admin-gated handler: got %d, want 403", code)
	}
}

// TestConcurrentMintsAreUnique guards the random email/username generation: a
// harness minting several identities in a row must not collide.
func TestConcurrentMintsAreUnique(t *testing.T) {
	pool := testDB(t)
	svc, _ := dbService(t, pool)

	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		got := mint(t, svc, "")
		if seen[got.User.Email] {
			t.Fatalf("duplicate email minted: %s", got.User.Email)
		}
		seen[got.User.Email] = true
	}
	var c int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE ephemeral_expires_at IS NOT NULL`).Scan(&c); err != nil {
		t.Fatal(err)
	}
	if c != 5 {
		t.Fatalf("expected 5 ephemeral users, got %d", c)
	}
}

// insertSession gives a user a session in the requested state, creating an app on
// demand. Returns the session id.
func insertSession(t *testing.T, pool *pgxpool.Pool, userID, state string) string {
	t.Helper()
	ctx := context.Background()
	var appID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (name) VALUES ('reap-guard-app') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, app_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3, 1920, 1080, 60, 15000)
		RETURNING id::text
	`, userID, appID, state).Scan(&id); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

func expire(t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET ephemeral_expires_at = now() - interval '1 second' WHERE id::text = $1`,
		userID); err != nil {
		t.Fatalf("expire identity: %v", err)
	}
}

func userExists(t *testing.T, pool *pgxpool.Pool, userID string) bool {
	t.Helper()
	var c int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM users WHERE id::text = $1`, userID).Scan(&c); err != nil {
		t.Fatalf("count user: %v", err)
	}
	return c > 0
}

// TestExpiredIdentityWithRunningSessionSurvivesUntilTerminal is the guard the bulk
// DELETE skipped: deleting a user whose session is RUNNING would orphan that
// session (and its GPU reservation) on the node agent. The identity must outlive
// its TTL until the session goes terminal, then reap on the next sweep.
func TestExpiredIdentityWithRunningSessionSurvivesUntilTerminal(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	got := mint(t, svc, `{"ttl_seconds":60}`)
	sessionID := insertSession(t, pool, got.User.ID, "running")
	expire(t, pool, got.User.ID)

	rep, err := authSvc.ReapEphemeral(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if rep.Deleted != 0 || rep.InSession != 1 || rep.Failed != 0 {
		t.Fatalf("expired-but-streaming identity: %+v, want Deleted=0 InSession=1 Failed=0", rep)
	}
	if !userExists(t, pool, got.User.ID) {
		t.Fatal("identity with a RUNNING session was deleted — its session is now orphaned on the agent")
	}

	// Session goes terminal: the next sweep takes the identity.
	if _, err := pool.Exec(ctx, `UPDATE sessions SET state = 'stopped' WHERE id::text = $1`, sessionID); err != nil {
		t.Fatalf("stop session: %v", err)
	}
	rep, err = authSvc.ReapEphemeral(ctx)
	if err != nil {
		t.Fatalf("reap after terminal: %v", err)
	}
	if rep.Deleted != 1 {
		t.Fatalf("after the session went terminal: %+v, want Deleted=1", rep)
	}
	if userExists(t, pool, got.User.ID) {
		t.Fatal("identity survived its session going terminal")
	}
}

// TestReapUsesTheCanonicalDeleteGuards: user_homes must be TOMBSTONED (gc_after
// set) so the home janitor reclaims the backing store. A bulk DELETE leaks it.
func TestReapTombstonesUserHomes(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	got := mint(t, svc, `{"ttl_seconds":60}`)
	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name) VALUES ('reap-home-host') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	var appID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO apps (name) VALUES ('reap-home-app') RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatalf("insert app: %v", err)
	}
	var homeID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO user_homes (user_id, host_id, app_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'volume', 'quasar-home-agent')
		RETURNING id::text
	`, got.User.ID, hostID, appID).Scan(&homeID); err != nil {
		t.Fatalf("insert user_home: %v", err)
	}

	expire(t, pool, got.User.ID)
	if rep, err := authSvc.ReapEphemeral(ctx); err != nil || rep.Deleted != 1 {
		t.Fatalf("reap: %+v err=%v", rep, err)
	}

	var gcAfter *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT gc_after FROM user_homes WHERE id::text = $1`, homeID).Scan(&gcAfter); err != nil {
		t.Fatalf("read home: %v", err)
	}
	if gcAfter == nil {
		t.Fatal("user_home was not tombstoned — the backing store leaks forever")
	}
}

// TestOneUndeletableRowDoesNotBlockTheSweep: the reaper is per-row, so a single
// refused identity must not stop every other expired identity from going. (The
// refused one here is the instance's last admin, which store.deleteUser guards.)
func TestOneUndeletableRowDoesNotBlockTheSweep(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	// The truncated DB has no other admin, so this mint is the LAST admin and
	// store.deleteUser refuses it.
	lastAdmin := mint(t, svc, `{"role":"admin","ttl_seconds":60}`)
	blockedBySession := mint(t, svc, `{"ttl_seconds":60}`)
	insertSession(t, pool, blockedBySession.User.ID, "running")
	healthy := mint(t, svc, `{"ttl_seconds":60}`)

	for _, id := range []string{lastAdmin.User.ID, blockedBySession.User.ID, healthy.User.ID} {
		expire(t, pool, id)
	}

	rep, err := authSvc.ReapEphemeral(ctx)
	if rep.Deleted != 1 {
		t.Fatalf("one refused row blocked the sweep: %+v (err=%v)", rep, err)
	}
	if rep.InSession != 1 || rep.Failed != 1 || err == nil {
		t.Fatalf("expected 1 in-session + 1 failed (reported via err): %+v err=%v", rep, err)
	}
	if userExists(t, pool, healthy.User.ID) {
		t.Fatal("the healthy expired identity was not reaped")
	}
	if !userExists(t, pool, lastAdmin.User.ID) || !userExists(t, pool, blockedBySession.User.ID) {
		t.Fatal("a guarded identity was deleted anyway")
	}
}

// TestEphemeralAdminThatWroteHostConfigReapsCleanly covers migration 0052: before
// it, host_settings.updated_by / console_config.updated_by were bare FKs, so an
// ephemeral ADMIN (the endpoint's stated purpose) that saved either surface became
// permanently undeletable and poisoned every future sweep.
func TestEphemeralAdminThatWroteHostConfigReapsCleanly(t *testing.T) {
	pool := testDB(t)
	svc, authSvc := dbService(t, pool)
	ctx := context.Background()

	// A second, non-ephemeral admin so the last-admin guard is not what we measure.
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('operator@example.com', 'operator', 'x', 'admin')
	`); err != nil {
		t.Fatalf("insert operator admin: %v", err)
	}

	agent := mint(t, svc, `{"role":"admin","ttl_seconds":60}`)
	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name) VALUES ('fk-poison-host') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatalf("insert host: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO host_settings (host_id, overrides, updated_by) VALUES ($1::uuid, '{}', $2::uuid)
	`, hostID, agent.User.ID); err != nil {
		t.Fatalf("insert host_settings: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO console_config (host_id, config, updated_by) VALUES ($1::uuid, '{}', $2::uuid)
	`, hostID, agent.User.ID); err != nil {
		t.Fatalf("insert console_config: %v", err)
	}

	expire(t, pool, agent.User.ID)
	rep, err := authSvc.ReapEphemeral(ctx)
	if err != nil || rep.Deleted != 1 {
		t.Fatalf("an admin that wrote host/console config did not reap: %+v err=%v", rep, err)
	}

	for _, tbl := range []string{"host_settings", "console_config"} {
		var updatedBy *string
		if err := pool.QueryRow(ctx,
			`SELECT updated_by::text FROM `+tbl+` WHERE host_id::text = $1`, hostID).Scan(&updatedBy); err != nil {
			t.Fatalf("read %s: %v", tbl, err)
		}
		if updatedBy != nil {
			t.Fatalf("%s.updated_by = %q, want NULL after the referenced admin was deleted", tbl, *updatedBy)
		}
	}
}
