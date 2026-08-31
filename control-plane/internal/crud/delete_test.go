package crud

// Integration tests for DELETE /v1/apps/{id} and DELETE /v1/hosts/{id}.
// Require Postgres: run via scripts/dev/dev.sh go-test-db (sets TEST_DATABASE_URL).
// All tests share a single DB and truncate in setup (-p 1 is mandatory).

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool opens a connection to the test database, runs migrations, truncates
// all rows, and registers a cleanup to close the pool.
func testPool(t *testing.T) *pgxpool.Pool {
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
		DELETE FROM user_app_favourites;
		DELETE FROM sessions; DELETE FROM gpus; DELETE FROM hosts;
		DELETE FROM apps; DELETE FROM runtime_presets;
		DELETE FROM auth_tokens; DELETE FROM users;
	`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedBasic inserts one user, one app, one host, and one GPU. Returns their IDs.
type basicIDs struct {
	userID, appID, hostID, gpuID string
}

func seedBasic(t *testing.T, pool *pgxpool.Pool) basicIDs {
	t.Helper()
	ctx := context.Background()
	var ids basicIDs
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('u@crud.test','u','x') RETURNING id::text`).Scan(&ids.userID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('testapp', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&ids.appID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status)
		VALUES ('host-1','offline') RETURNING id::text`).Scan(&ids.hostID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, 8192, 4) RETURNING id::text`, ids.hostID).Scan(&ids.gpuID))
	return ids
}

func mustExec(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// insertSession inserts a session in the given state, wired to the given app and host.
func insertSession(t *testing.T, pool *pgxpool.Pool, ids basicIDs, state string) string {
	t.Helper()
	var sessionID string
	mustExec(t, pool.QueryRow(context.Background(), `
		INSERT INTO sessions
			(user_id, app_id, host_id, gpu_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1, $2, $3, $4, $5, 1280, 720, 60, 6000)
		RETURNING id::text`,
		ids.userID, ids.appID, ids.hostID, ids.gpuID, state,
	).Scan(&sessionID))
	return sessionID
}

// insertHome inserts a user_homes row for the given (user, app, host).
func insertHome(t *testing.T, pool *pgxpool.Pool, ids basicIDs) string {
	t.Helper()
	var homeID string
	mustExec(t, pool.QueryRow(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1, $2, $3, 'local', '/data/home')
		RETURNING id::text`,
		ids.userID, ids.appID, ids.hostID,
	).Scan(&homeID))
	return homeID
}

// --- app-delete tests -------------------------------------------------------

// TestDeleteApp_RefusedWhileActive: app-delete must return ErrAppHasActiveSessions
// when a non-terminal session references the app.
func TestDeleteApp_RefusedWhileActive(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ids := seedBasic(t, pool)
	// Insert a running session.
	insertSession(t, pool, ids, "running")

	_, err := s.deleteApp(context.Background(), ids.appID, false)
	if !errors.Is(err, ErrAppHasActiveSessions) {
		t.Fatalf("expected ErrAppHasActiveSessions, got: %v", err)
	}
}

// TestDeleteApp_Succeeds: app-delete succeeds when no active sessions exist;
// asserts that managed homes are tombstoned and that terminal session history
// is cascade-deleted.
func TestDeleteApp_Succeeds(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ids := seedBasic(t, pool)

	// Terminal session — should cascade away on delete.
	sessionID := insertSession(t, pool, ids, "stopped")
	// Managed home — should be tombstoned.
	homeID := insertHome(t, pool, ids)

	name, err := s.deleteApp(context.Background(), ids.appID, false)
	if err != nil {
		t.Fatalf("deleteApp: %v", err)
	}
	// The returned name is what the audit record carries; a uuid alone names a
	// row that no longer exists.
	if name != "testapp" {
		t.Fatalf("deleteApp returned name %q, want \"testapp\"", name)
	}

	ctx := context.Background()

	// App row must be gone.
	var appExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM apps WHERE id::text = $1)`, ids.appID).Scan(&appExists)
	if appExists {
		t.Fatal("app row still exists after delete")
	}

	// Terminal session must have cascaded.
	var sesExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id::text = $1)`, sessionID).Scan(&sesExists)
	if sesExists {
		t.Fatal("terminal session row was not cascade-deleted")
	}

	// Home row must be tombstoned (gc_after set) — app_id nulled by FK SET NULL.
	var gcAfterIsSet bool
	_ = pool.QueryRow(ctx, `SELECT gc_after IS NOT NULL FROM user_homes WHERE id::text = $1`, homeID).Scan(&gcAfterIsSet)
	if !gcAfterIsSet {
		t.Fatal("home gc_after was not set (tombstone missing)")
	}
}

// TestDeleteApp_NotFound: deleting an unknown ID returns ErrNotFound.
func TestDeleteApp_NotFound(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	_, err := s.deleteApp(context.Background(), "00000000-0000-0000-0000-000000000000", false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}

// --- host-delete tests -------------------------------------------------------

// TestDeleteHost_RefusedWhileActiveSessions: host-delete must return
// ErrHostHasActiveSessions when a non-terminal session is on the host.
func TestDeleteHost_RefusedWhileActiveSessions(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ids := seedBasic(t, pool)
	insertSession(t, pool, ids, "starting")

	_, err := s.deleteHost(context.Background(), ids.hostID)
	if !errors.Is(err, ErrHostHasActiveSessions) {
		t.Fatalf("expected ErrHostHasActiveSessions, got: %v", err)
	}
}

// TestDeleteHost_Succeeds: host-delete succeeds when offline and no active
// sessions exist; asserts homes tombstoned and GPU + terminal session cascaded.
func TestDeleteHost_Succeeds(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ids := seedBasic(t, pool)

	// Terminal session and a managed home on this host.
	sessionID := insertSession(t, pool, ids, "failed")
	homeID := insertHome(t, pool, ids)

	nodeName, err := s.deleteHost(context.Background(), ids.hostID)
	if err != nil {
		t.Fatalf("deleteHost: %v", err)
	}
	if nodeName != "host-1" {
		t.Fatalf("deleteHost returned node_name %q, want \"host-1\"", nodeName)
	}

	ctx := context.Background()

	// Host row must be gone.
	var hostExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts WHERE id::text = $1)`, ids.hostID).Scan(&hostExists)
	if hostExists {
		t.Fatal("host row still exists after delete")
	}

	// GPU must have cascaded.
	var gpuExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM gpus WHERE id::text = $1)`, ids.gpuID).Scan(&gpuExists)
	if gpuExists {
		t.Fatal("gpu row was not cascade-deleted")
	}

	// Terminal session must have cascaded.
	var sesExists bool
	_ = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id::text = $1)`, sessionID).Scan(&sesExists)
	if sesExists {
		t.Fatal("terminal session row was not cascade-deleted")
	}

	// Home must be tombstoned (gc_after set) and host_id nulled.
	var gcAfterIsSet bool
	_ = pool.QueryRow(ctx, `SELECT gc_after IS NOT NULL FROM user_homes WHERE id::text = $1`, homeID).Scan(&gcAfterIsSet)
	if !gcAfterIsSet {
		t.Fatal("home gc_after was not set (tombstone missing)")
	}
}

// TestDeleteHost_NotFound: deleting an unknown ID returns ErrNotFound.
func TestDeleteHost_NotFound(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	_, err := s.deleteHost(context.Background(), "00000000-0000-0000-0000-000000000000")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
}
