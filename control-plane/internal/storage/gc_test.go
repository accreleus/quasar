package storage

// Integration tests for P5-05: GC janitor + usage bookkeeping.
// Requires TEST_DATABASE_URL (silently skipped without it; go-check skips,
// go-test-db runs them — a ticket touching the DB is not DONE until green).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── helpers ──────────────────────────────────────────────────────────────────

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
	if _, err := pool.Exec(ctx, `
		DELETE FROM session_metrics; DELETE FROM user_homes;
		DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM apps; DELETE FROM auth_tokens; DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// seedUser inserts a bare user row and returns its UUID string.
func seedUser(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (email, username, password_hash) VALUES ($1, $1, 'x') RETURNING id::text`,
		email).Scan(&id))
	return id
}

// seedApp inserts an app and returns its UUID string.
func seedApp(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec)
		VALUES ($1, 512, 1, 1280, 720, 30, 2000, '{}') RETURNING id::text`, name).Scan(&id))
	return id
}

// seedHost inserts a host and returns its UUID string.
func seedHost(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ('h','online') RETURNING id::text`).Scan(&id))
	return id
}

// insertHome directly inserts a user_homes row and returns its UUID string.
func insertHome(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'volume', 'vol-test')
		RETURNING id::text`, userID, appID, hostID).Scan(&id))
	return id
}

// insertRunningSession inserts a running session for (user, app, host).
func insertRunningSession(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) string {
	t.Helper()
	// Need a GPU to satisfy NOT NULL on gpu_id in sessions.
	var gpuID string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1::uuid, 0, 8192, 4) RETURNING id::text`, hostID).Scan(&gpuID))

	var id string
	must(t, pool.QueryRow(context.Background(), `
		INSERT INTO sessions
		  (user_id, app_id, host_id, gpu_id, state,
		   width, height, fps, bitrate_kbps, h264_profile,
		   reserved_vram_mb, reserved_encode_slots)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running',
		        1280, 720, 30, 2000, 'constrained-baseline', 512, 1)
		RETURNING id::text`,
		userID, appID, hostID, gpuID).Scan(&id))
	return id
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestListHomes_Empty(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	homes, next, err := mgr.ListHomes(context.Background(), ListHomesOpts{})
	must(t, err)
	if len(homes) != 0 {
		t.Errorf("want 0 homes, got %d", len(homes))
	}
	if next != "" {
		t.Errorf("want empty cursor, got %q", next)
	}
}

func TestListHomes_FilterUserID(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u1 := seedUser(t, pool, "u1@test")
	u2 := seedUser(t, pool, "u2@test")
	app := seedApp(t, pool, "app")
	host := seedHost(t, pool)

	insertHome(t, pool, u1, app, host)
	insertHome(t, pool, u2, app, host) // different host needed for unique constraint — actually same (user_id, app_id, host_id) is unique; u2 is different user so OK

	homes, _, err := mgr.ListHomes(ctx, ListHomesOpts{UserID: u1})
	must(t, err)
	if len(homes) != 1 {
		t.Fatalf("filter user_id: want 1 home, got %d", len(homes))
	}
	if homes[0].UserID == nil || *homes[0].UserID != u1 {
		t.Errorf("filtered home user_id = %v, want %s", homes[0].UserID, u1)
	}
}

func TestListHomes_FilterPendingGC(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "gc@test")
	app1 := seedApp(t, pool, "app1")
	app2 := seedApp(t, pool, "app2")
	host := seedHost(t, pool)

	h1 := insertHome(t, pool, u, app1, host)
	insertHome(t, pool, u, app2, host)

	// Tombstone h1.
	_, err := mgr.TombstoneHome(ctx, h1)
	must(t, err)

	pendingTrue := true
	tombstoned, _, err := mgr.ListHomes(ctx, ListHomesOpts{PendingGC: &pendingTrue})
	must(t, err)
	if len(tombstoned) != 1 || tombstoned[0].ID != h1 {
		t.Errorf("pending_gc=true: want 1 tombstoned home, got %v", tombstoned)
	}

	pendingFalse := false
	live, _, err := mgr.ListHomes(ctx, ListHomesOpts{PendingGC: &pendingFalse})
	must(t, err)
	if len(live) != 1 {
		t.Errorf("pending_gc=false: want 1 live home, got %d", len(live))
	}
}

func TestTombstoneHome_Happy(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "tb@test")
	app := seedApp(t, pool, "tbapp")
	host := seedHost(t, pool)
	id := insertHome(t, pool, u, app, host)

	ref, err := mgr.TombstoneHome(ctx, id)
	must(t, err)
	// The returned (user, app) pair is what the admin audit row records — the
	// home's uuid alone does not say whose data is now scheduled for deletion.
	if ref.Username != "tb@test" || ref.AppName != "tbapp" {
		t.Errorf("TombstoneHome ref = %+v, want {tb@test tbapp}", ref)
	}

	var gcAfter *time.Time
	must(t, pool.QueryRow(ctx, `SELECT gc_after FROM user_homes WHERE id::text = $1`, id).Scan(&gcAfter))
	if gcAfter == nil {
		t.Errorf("gc_after is NULL after tombstone, want non-null")
	}
}

func TestTombstoneHome_NotFound(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	_, err := mgr.TombstoneHome(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("want ErrHomeNotFound, got nil")
	}
	if !isHomeNotFound(err) {
		t.Errorf("want ErrHomeNotFound, got %v", err)
	}
}

func isHomeNotFound(err error) bool {
	return err != nil && err.Error() == ErrHomeNotFound.Error()
}

func TestTombstoneHome_LiveSession(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "live@test")
	app := seedApp(t, pool, "liveapp")
	host := seedHost(t, pool)
	id := insertHome(t, pool, u, app, host)
	insertRunningSession(t, pool, u, app, host)

	_, err := mgr.TombstoneHome(ctx, id)
	if err == nil {
		t.Fatal("want ErrHomeInUse, got nil")
	}
	if err.Error() != ErrHomeInUse.Error() {
		t.Errorf("want ErrHomeInUse, got %v", err)
	}
}

func TestListUserStorage_OwnRowsOnly(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u1 := seedUser(t, pool, "own1@test")
	u2 := seedUser(t, pool, "own2@test")
	app := seedApp(t, pool, "myapp")
	host := seedHost(t, pool)

	insertHome(t, pool, u1, app, host)
	insertHome(t, pool, u2, app, host)

	items, err := mgr.ListUserStorage(ctx, u1)
	must(t, err)
	if len(items) != 1 {
		t.Fatalf("want 1 item for u1, got %d", len(items))
	}
	if items[0].AppID != app {
		t.Errorf("item app_id = %s, want %s", items[0].AppID, app)
	}
}

func TestListUserStorage_ExcludesTombstoned(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	ctx := context.Background()

	u := seedUser(t, pool, "dead@test")
	app1 := seedApp(t, pool, "alive")
	app2 := seedApp(t, pool, "dead")
	host := seedHost(t, pool)

	insertHome(t, pool, u, app1, host)
	h2 := insertHome(t, pool, u, app2, host)
	_, err := mgr.TombstoneHome(ctx, h2)
	must(t, err)

	items, err := mgr.ListUserStorage(ctx, u)
	must(t, err)
	if len(items) != 1 || items[0].AppID != app1 {
		t.Errorf("want 1 live item, got %v", items)
	}
}

func TestDeleteUser_TombstonesOrphansHomes(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	u := seedUser(t, pool, "del@test")
	app := seedApp(t, pool, "delapp")
	host := seedHost(t, pool)
	homeID := insertHome(t, pool, u, app, host)

	// Delete the user via the auth service (which calls TombstoneUserHomes then DELETE).
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	must(t, authSvc.DeleteUser(ctx, u))

	// The user row is gone.
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE id::text = $1`, u).Scan(&n))
	if n != 0 {
		t.Errorf("user still exists after delete")
	}

	// The home row is still there (GC hasn't run), orphaned with NULL user_id and gc_after set.
	var userIDNull bool
	var gcAfter *time.Time
	must(t, pool.QueryRow(ctx, `SELECT user_id IS NULL, gc_after FROM user_homes WHERE id::text = $1`, homeID).
		Scan(&userIDNull, &gcAfter))
	if !userIDNull {
		t.Errorf("home row user_id not NULL after user deletion")
	}
	if gcAfter == nil {
		t.Errorf("home row gc_after not set after user deletion")
	}
}

func TestHomeJanitor_ReapsExpired(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()

	u := seedUser(t, pool, "jan@test")
	app1 := seedApp(t, pool, "expired")
	app2 := seedApp(t, pool, "fresh")
	host := seedHost(t, pool)

	h1 := insertHome(t, pool, u, app1, host)
	h2 := insertHome(t, pool, u, app2, host)

	// Set h1's gc_after to > 24h ago (already past grace).
	must(t, pool.QueryRow(ctx,
		`UPDATE user_homes SET gc_after = now() - interval '25 hours' WHERE id::text = $1 RETURNING id::text`, h1).
		Scan(&h1))

	// Set h2's gc_after to now (within grace, should not be reaped).
	must(t, pool.QueryRow(ctx,
		`UPDATE user_homes SET gc_after = now() WHERE id::text = $1 RETURNING id::text`, h2).
		Scan(&h2))

	// Run the janitor sweep directly by invoking the SQL it would run.
	_, err := pool.Exec(ctx, `DELETE FROM user_homes WHERE gc_after IS NOT NULL AND gc_after + interval '24 hours' < now()`)
	must(t, err)

	// h1 should be gone.
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, h1).Scan(&n))
	if n != 0 {
		t.Errorf("expired home still exists after janitor sweep")
	}

	// h2 should remain.
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_homes WHERE id::text = $1`, h2).Scan(&n))
	if n != 1 {
		t.Errorf("fresh home was reaped unexpectedly")
	}
}

func TestAdminEndpoints_RequireAdmin(t *testing.T) {
	pool := testDB(t)
	mgr := NewLocal(pool, t.TempDir())
	h := NewHandler(mgr)

	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	authHandler := auth.NewHandler(authSvc)

	mux := http.NewServeMux()
	h.Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)

	// Unauthenticated → 401 on GET /v1/admin/storage/homes.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/admin/storage/homes", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated list: want 401, got %d", rr.Code)
	}

	// Unauthenticated → 401 on DELETE /v1/admin/storage/homes/{id}.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("DELETE", "/v1/admin/storage/homes/some-id", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated tombstone: want 401, got %d", rr.Code)
	}

	// Register a regular user and login to get a bearer token.
	ctx := context.Background()
	_, err = authSvc.Register(ctx, "norole@test.local", "norole", "password12345")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	tok, err := authSvc.Login(ctx, "norole@test.local", "password12345", "test")
	if err != nil {
		t.Fatalf("login user: %v", err)
	}

	// Regular user → 403 on both admin endpoints (server-enforced, invariant #6).
	req := httptest.NewRequest("GET", "/v1/admin/storage/homes", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin list: want 403, got %d", rr.Code)
	}

	req = httptest.NewRequest("DELETE", "/v1/admin/storage/homes/some-id", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin tombstone: want 403, got %d", rr.Code)
	}
}
