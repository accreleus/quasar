package session

// migration_0113_db_test.go — compatibility tools (amendment 56). Up is additive and every
// existing row keeps its meaning; the schema's CHECKs hold; down drops everything. Runs in a
// scratch database: it migrates down.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0113UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 111)

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
	must(t, exec(t, pool, `INSERT INTO instance_settings (id) VALUES (true)`))

	migrateTo(t, m, 113)

	var names []string
	var def *string
	var maxEnabled, reserve int
	var free *int64
	must(t, pool.QueryRow(ctx, `SELECT compat_tool_names FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&names))
	must(t, pool.QueryRow(ctx, `SELECT compat_tool_default_id::text, compat_tools_max_enabled, compat_tools_disk_reserve_gib FROM instance_settings`).
		Scan(&def, &maxEnabled, &reserve))
	must(t, pool.QueryRow(ctx, `SELECT compat_tools_free_bytes FROM hosts WHERE id = $1::uuid`, hostID).Scan(&free))
	if names != nil || def != nil || maxEnabled != 8 || reserve != 10 || free != nil {
		t.Errorf("defaults: names=%v default=%v max=%d reserve=%d free=%v", names, def, maxEnabled, reserve, free)
	}

	const sha = "2e37447902529c9fc39138dc0b3f24aed3513b59a190b825e6b4a668e49e2033bdf6b8a7998e8686e067bbb3f61f3a6633e2408f0cc96f6dad983bd18e2db31e"
	for name, sql := range map[string]string{
		"valve with a GE name":      `INSERT INTO compat_tools (family, source, steam_name, display_name) VALUES ('valve','steam','GE-Proton1-1','x')`,
		"valve with a tag":          `INSERT INTO compat_tools (family, source, steam_name, tag, display_name) VALUES ('valve','steam','proton_9','GE-Proton9-1','x')`,
		"ge without a checksum":     `INSERT INTO compat_tools (family, source, tag, display_name, asset_name, asset_size_bytes) VALUES ('ge','tag','GE-Proton9-1','x','GE-Proton9-1.tar.gz',1)`,
		"ge named like a Valve one": `INSERT INTO compat_tools (family, source, tag, steam_name, display_name, asset_name, asset_size_bytes, asset_sha512) VALUES ('ge','tag','GE-Proton9-1','proton_9','x','GE-Proton9-1.tar.gz',1,'` + sha + `')`,
		"ge with another asset":     `INSERT INTO compat_tools (family, source, tag, display_name, asset_name, asset_size_bytes, asset_sha512) VALUES ('ge','tag','GE-Proton9-1','x','other.tar.gz',1,'` + sha + `')`,
		"a name with a slash":       `INSERT INTO compat_tools (family, source, steam_name, display_name) VALUES ('valve','steam','proton_9/../x','x')`,
		"max_enabled zero":          `UPDATE instance_settings SET compat_tools_max_enabled = 0`,
		"reserve over the cap":      `UPDATE instance_settings SET compat_tools_disk_reserve_gib = 1025`,
	} {
		if exec(t, pool, sql) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	must(t, exec(t, pool, `INSERT INTO compat_tools (family, source, steam_name, display_name, enabled) VALUES ('valve','steam','proton_9','Proton 9',true)`))
	for name, sql := range map[string]string{
		"appid 0":               `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'steam','0',id FROM compat_tools`,
		"a leading zero":        `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'steam','007',id FROM compat_tools`,
		"an argument-shaped id": `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'steam','1 -foo',id FROM compat_tools`,
		"another provider":      `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'heroic','1',id FROM compat_tools`,
	} {
		if exec(t, pool, sql) == nil {
			t.Errorf("mapping with %s was accepted", name)
		}
	}
	must(t, exec(t, pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'steam','1145360',id FROM compat_tools`))
	if exec(t, pool, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id) SELECT 'steam','1145360',id FROM compat_tools`) == nil {
		t.Error("a second mapping for one appid was accepted: the key must make it a replace")
	}
	if exec(t, pool, `DELETE FROM compat_tools`) == nil {
		t.Error("deleting a mapped tool was accepted: tool_id is ON DELETE RESTRICT")
	}

	migrateTo(t, m, 111)
	var left int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('compat_tools','compat_tool_mappings','host_compat_tools')`).Scan(&left))
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'sessions' AND column_name = 'compat_tool_names')
		   OR (table_name = 'hosts' AND column_name LIKE 'compat_tools_%')
		   OR (table_name = 'instance_settings' AND column_name LIKE 'compat_tool%')`).Scan(&cols))
	if left != 0 || cols != 0 {
		t.Errorf("after the down: %d tables and %d columns survived", left, cols)
	}
	migrateTo(t, m, 113)
}
