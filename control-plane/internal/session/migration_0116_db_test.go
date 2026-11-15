package session

// migration_0116_db_test.go — instance default transport (amendment 61). Up: stock and explicit
// webrtc profiles read default, webtransport ones are untouched, the CHECKs and the column
// default hold. Down: each default row takes the setting's value, so no effective transport
// changes, and 0111's CHECK and DEFAULT return. Runs in a scratch database.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0116UpAndDown(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 115)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()

	must(t, exec(t, pool, `INSERT INTO launch_profiles (id, display_name, transport) VALUES
		('x-rtc','x','webrtc'), ('x-opt','x','webtransport_opt_in'), ('x-wt','x','webtransport')`))
	transports := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT id, transport FROM launch_profiles WHERE id LIKE 'x-%'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, tr string
			must(t, rows.Scan(&id, &tr))
			out[id] = tr
		}
		return out
	}

	// Pins the constraint name the migration drops (it is 0111's unnamed column CHECK).
	var name string
	must(t, pool.QueryRow(ctx, `SELECT conname FROM pg_constraint
		WHERE conrelid = 'launch_profiles'::regclass AND contype = 'c' AND pg_get_constraintdef(oid) LIKE '%transport%'`).Scan(&name))
	if name != "launch_profiles_transport_check" {
		t.Fatalf("0111's transport CHECK is %q", name)
	}

	migrateTo(t, m, 116)

	got := transports()
	if got["x-rtc"] != "default" || got["x-opt"] != "webtransport_opt_in" || got["x-wt"] != "webtransport" {
		t.Errorf("after up: %v", got)
	}
	var stock int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM launch_profiles WHERE transport = 'webrtc'`).Scan(&stock))
	if stock != 0 {
		t.Errorf("%d launch profiles still read webrtc after up", stock)
	}
	must(t, exec(t, pool, `INSERT INTO launch_profiles (id, display_name) VALUES ('x-new','x')`))
	if transports()["x-new"] != "default" {
		t.Errorf("a new profile reads %q, want default", transports()["x-new"])
	}
	must(t, exec(t, pool, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`))
	var setting string
	must(t, pool.QueryRow(ctx, `SELECT default_transport FROM instance_settings`).Scan(&setting))
	if setting != "webrtc" {
		t.Errorf("default_transport starts at %q, want webrtc", setting)
	}
	for name, sql := range map[string]string{
		"launch_profiles.transport":           `UPDATE launch_profiles SET transport = 'quic'`,
		"instance_settings.default_transport": `UPDATE instance_settings SET default_transport = 'default'`,
	} {
		if exec(t, pool, sql) == nil {
			t.Errorf("%s accepted a value outside its CHECK", name)
		}
	}

	// Down never refuses: default rows take the setting's current value.
	must(t, exec(t, pool, `UPDATE instance_settings SET default_transport = 'webtransport'`))
	migrateTo(t, m, 115)
	got = transports()
	if got["x-rtc"] != "webtransport" || got["x-new"] != "webtransport" || got["x-opt"] != "webtransport_opt_in" || got["x-wt"] != "webtransport" {
		t.Errorf("after down: %v", got)
	}
	if exec(t, pool, `UPDATE launch_profiles SET transport = 'default'`) == nil {
		t.Error("0111's CHECK did not come back")
	}
	must(t, exec(t, pool, `INSERT INTO launch_profiles (id, display_name) VALUES ('x-old','x')`))
	if transports()["x-old"] != "webrtc" {
		t.Errorf("0111's DEFAULT did not come back: %q", transports()["x-old"])
	}
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'instance_settings' AND column_name = 'default_transport'`).Scan(&cols))
	if cols != 0 {
		t.Error("default_transport survived the down")
	}
	migrateTo(t, m, 116)
}
