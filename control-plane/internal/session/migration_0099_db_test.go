package session

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Amendment 19 (#455): 0099 drops the nine retired console settings from every
// stored console_config row and leaves the six kept settings, and any other
// key, exactly as they were.
func TestMigration0099DropsRetiredConsoleKeys(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 98)
	pool, err := pgxpool.New(context.Background(), url)
	must(t, err)
	t.Cleanup(pool.Close)
	ctx := context.Background()

	var full, kept, empty string
	for _, name := range []string{"console-full", "console-kept", "console-empty"} {
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`, name).Scan(&id))
		switch name {
		case "console-full":
			full = id
		case "console-kept":
			kept = id
		default:
			empty = id
		}
	}
	keptSettings := `{"enabled":true,"output_id":"card0:DP-4","input_devices":["/dev/input/event4"],
		"auto_start_on_display":true,"default_app":"6f1c0000-0000-0000-0000-000000000001",
		"default_user":"0b2e0000-0000-0000-0000-000000000002"}`
	must(t, exec(t, pool, `INSERT INTO console_config (host_id, config) VALUES
		($1::uuid, $2::jsonb || '{"connector":"auto","mode":{"width":3840,"height":2160,"refresh_millihz":119879},
			"compositor":"weston","audio_output":"hw:1,3","stream":true,"stream_audio":true,"grab":true,
			"auto_connect_controller":true,"fullscreen":true}'::jsonb),
		($3::uuid, $2::jsonb),
		($4::uuid, '{}'::jsonb)`, full, keptSettings, kept, empty))

	migrateTo(t, m, 99)

	var want map[string]any
	must(t, json.Unmarshal([]byte(keptSettings), &want))
	for _, host := range []string{full, kept} {
		var raw []byte
		must(t, pool.QueryRow(ctx, `SELECT config FROM console_config WHERE host_id = $1::uuid`, host).Scan(&raw))
		var got map[string]any
		must(t, json.Unmarshal(raw, &got))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("host %s config after 0099 = %v, want exactly the six kept settings %v", host, got, want)
		}
	}
	var raw string
	must(t, pool.QueryRow(ctx, `SELECT config::text FROM console_config WHERE host_id = $1::uuid`, empty).Scan(&raw))
	if raw != "{}" {
		t.Fatalf("an empty config became %s", raw)
	}
}
