package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// testDB returns a pool against TEST_DATABASE_URL, applying migrations and
// truncating the auth tables for a clean slate. The test is skipped when the
// env var is unset, so `go test` stays green without a database.
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
	if _, err := pool.Exec(ctx, `TRUNCATE users, auth_tokens RESTART IDENTITY CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// testService builds an auth Service with fast argon2 params for tests.
func testService(t *testing.T, pool *pgxpool.Pool) *Service {
	t.Helper()
	svc, err := NewService(pool, testParams(), time.Hour)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}
