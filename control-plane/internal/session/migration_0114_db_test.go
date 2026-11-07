package session

// migration_0114_db_test.go — sessions.park_on_end: additive, every existing row stays parkable,
// down drops it. Runs in a scratch database: it migrates down.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0114UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 113)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	var userID, appID, hostID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('m@t.local','m','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('m') RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ('m','online') RETURNING id::text`).Scan(&hostID))
	sessionID := insertSessionRow(t, pool, userID, appID, &hostID, "running")

	migrateTo(t, m, 114)
	var parkable bool
	must(t, pool.QueryRow(ctx, `SELECT park_on_end FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&parkable))
	if !parkable {
		t.Error("an existing session is not parkable after the migration")
	}

	migrateTo(t, m, 113)
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'sessions' AND column_name = 'park_on_end'`).Scan(&cols))
	if cols != 0 {
		t.Error("park_on_end survived the down migration")
	}
}
