package session

// migration_0107_db_test.go — the preview release channel (amendment 48). Up widens
// both channel CHECKs and adds release_check; down refuses while anything is on
// preview. Runs in a scratch database: it migrates down.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0107UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 106)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	must(t, exec(t, pool, `INSERT INTO instance_settings (id, release_channel) VALUES (true, 'edge')
		ON CONFLICT (id) DO UPDATE SET release_channel = 'edge'`))
	const previewRow = `INSERT INTO platform_releases (channel, source_commit, built_at, schema_version)
		VALUES ('preview', 'cccccccccccccccccccccccccccccccccccccccc', now(), 107)`
	if exec(t, pool, `UPDATE instance_settings SET release_channel = 'preview'`) == nil {
		t.Fatal("0106 accepted release_channel 'preview'")
	}

	migrateTo(t, m, 107)
	must(t, exec(t, pool, `UPDATE instance_settings SET release_channel = 'preview'`))
	must(t, exec(t, pool, previewRow))
	var check *string
	must(t, pool.QueryRow(ctx, `SELECT release_check FROM instance_settings`).Scan(&check))
	if check != nil {
		t.Errorf("release_check = %q after up, want NULL (follow the channel)", *check)
	}
	must(t, exec(t, pool, `UPDATE instance_settings SET release_check = 'daily'`))
	if exec(t, pool, `UPDATE instance_settings SET release_check = 'hourly'`) == nil {
		t.Error("release_check accepted 'hourly'")
	}
	if exec(t, pool, `UPDATE instance_settings SET release_channel = 'nightly'`) == nil {
		t.Error("release_channel accepted 'nightly'")
	}

	err = m.Migrate(106)
	if err == nil || !strings.Contains(err.Error(), "preview channel is in use") {
		t.Fatalf("down while on preview: err = %v, want the refusal", err)
	}
	_, _ = m.Close()
	m = newMigrator(t, url)
	if err := m.Force(107); err != nil {
		t.Fatalf("force: %v", err)
	}
	must(t, exec(t, pool, `UPDATE instance_settings SET release_channel = 'stable'`))
	err = m.Migrate(106)
	if err == nil || !strings.Contains(err.Error(), "preview channel is in use") {
		t.Fatalf("down with a preview row: err = %v, want the refusal", err)
	}
	_, _ = m.Close()
	m = newMigrator(t, url)
	if err := m.Force(107); err != nil {
		t.Fatalf("force: %v", err)
	}
	must(t, exec(t, pool, `DELETE FROM platform_releases WHERE channel = 'preview'`))
	migrateTo(t, m, 106)

	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'instance_settings' AND column_name = 'release_check'`).Scan(&cols))
	if cols != 0 {
		t.Error("release_check survived the down migration")
	}
	if exec(t, pool, previewRow) == nil {
		t.Error("platform_releases accepted 'preview' after down")
	}
	migrateTo(t, m, 107)
}
