package session

// migration_0114_db_test.go — Ultra-low latency (PyroWave), amendments 55 and 58. Up is additive
// and every existing row keeps its meaning; the CHECKs hold; the down refuses while a session
// granted PyroWave is live, then drops the five columns. Runs in a scratch database.

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

	var userID, appID, hostID, lpID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash) VALUES ('m@t.local','m','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('m') RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ('m','online') RETURNING id::text`).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO launch_profiles (id, display_name) VALUES ('m','m') RETURNING id`).Scan(&lpID))
	sessionID := insertSessionRow(t, pool, userID, appID, &hostID, "running")
	must(t, exec(t, pool, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`))

	migrateTo(t, m, 114)

	var mode string
	var kbps *int
	var record *string
	var slot bool
	var perGPU int
	must(t, pool.QueryRow(ctx, `SELECT pyrowave_mode, pyrowave_kbps FROM launch_profiles WHERE id = $1`, lpID).Scan(&mode, &kbps))
	must(t, pool.QueryRow(ctx, `SELECT pyrowave::text, pyrowave_slot FROM sessions WHERE id = $1::uuid`, sessionID).Scan(&record, &slot))
	must(t, pool.QueryRow(ctx, `SELECT pyrowave_max_per_gpu FROM instance_settings`).Scan(&perGPU))
	if mode != "off" || kbps != nil || record != nil || slot || perGPU != 1 {
		t.Errorf("defaults: mode=%q kbps=%v record=%v slot=%v per_gpu=%d", mode, kbps, record, slot, perGPU)
	}

	for name, sql := range map[string]string{
		"an unknown mode":      `UPDATE launch_profiles SET pyrowave_mode = 'always'`,
		"kbps below the cap":   `UPDATE launch_profiles SET pyrowave_kbps = 49999`,
		"kbps above the cap":   `UPDATE launch_profiles SET pyrowave_kbps = 1000001`,
		"per-GPU zero":         `UPDATE instance_settings SET pyrowave_max_per_gpu = 0`,
		"per-GPU over the cap": `UPDATE instance_settings SET pyrowave_max_per_gpu = 5`,
	} {
		if exec(t, pool, sql) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	must(t, exec(t, pool, `UPDATE launch_profiles SET pyrowave_mode = 'prefer', pyrowave_kbps = 300000`))

	must(t, exec(t, pool, `UPDATE sessions SET pyrowave = '{"allowed": true}' WHERE id = $1::uuid`, sessionID))
	if err := m.Migrate(113); err == nil {
		t.Fatal("the down ran under a live session granted PyroWave")
	}
	must(t, m.Force(114))
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, sessionID))
	migrateTo(t, m, 113)
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE column_name IN ('pyrowave_mode','pyrowave_kbps','pyrowave','pyrowave_slot','pyrowave_max_per_gpu')`).Scan(&cols))
	if cols != 0 {
		t.Errorf("after the down: %d columns survived", cols)
	}
	migrateTo(t, m, 114)
}
