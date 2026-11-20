package session

// migration_0118_db_test.go — the swap compat-tool guard compares mapping commit order (#1555).
// Up: every mapping gets a revision from the sequence, the session's watermark stays NULL.
// Down drops both columns and the sequence.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0118UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 117)

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
	must(t, exec(t, pool, `INSERT INTO compat_tools (family, source, steam_name, display_name, enabled)
		VALUES ('valve','steam','proton_9','Proton 9',true)`))
	var toolID string
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM compat_tools WHERE steam_name = 'proton_9'`).Scan(&toolID))
	must(t, exec(t, pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) VALUES ('steam','1145360', $1::uuid)`, toolID))

	migrateTo(t, m, 118)

	// The row that predates the column still gets a revision, and every new mapping a later one.
	var pre, next int64
	must(t, pool.QueryRow(ctx, `SELECT revision FROM compat_tool_mappings WHERE external_id = '1145360'`).Scan(&pre))
	must(t, pool.QueryRow(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id)
		VALUES ('steam','1145361', $1::uuid) RETURNING revision`, toolID).Scan(&next))
	if pre < 1 || next <= pre {
		t.Errorf("revisions: existing %d, new %d, want existing >= 1 and new later", pre, next)
	}
	var watermark *int64
	must(t, pool.QueryRow(ctx, `SELECT compat_mapping_revision FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&watermark))
	if watermark != nil {
		t.Errorf("session watermark = %v at up, want NULL", *watermark)
	}

	// Down drops both columns and the sequence.
	migrateTo(t, m, 117)
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'compat_tool_mappings' AND column_name = 'revision')
		   OR (table_name = 'sessions' AND column_name = 'compat_mapping_revision')`).Scan(&cols))
	var seqs int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = 'compat_tool_mappings_revision_seq'`).Scan(&seqs))
	if cols != 0 || seqs != 0 {
		t.Errorf("after the down: %d columns and %d sequences survived", cols, seqs)
	}
	migrateTo(t, m, 118)
}
