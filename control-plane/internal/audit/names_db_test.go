package audit

// names_db_test.go — the read-time name lookups, against real tables. Requires
// Postgres: make test-db.
//
// Every query in nameQueries is SQL that no unit test can reach, and each names
// a different table and column. A typo in one is invisible until an operator
// opens the audit log, so each one is exercised here at least once.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedNamedEntities inserts one row per named kind and returns their ids.
//
// EVERY row is registered for deletion in a t.Cleanup. The DB tests share one
// database and run -p 1, so a leaked row is not a leak but a failure in some
// other package: a stray stream_profiles row lands in session's ladder
// assertions, and a second platform_releases row with the same (channel,
// source_commit) violates its unique constraint on the next test in this file.
// Text ids and the release commit therefore also carry a per-call suffix.
func seedNamedEntities(t *testing.T, s *Store) map[kind]string {
	t.Helper()
	ctx := context.Background()
	ids := map[kind]string{}

	// Deleted in reverse insertion order, so a child never outlives its parent.
	type row struct{ table, id string }
	var created []row
	t.Cleanup(func() {
		for i := len(created) - 1; i >= 0; i-- {
			r := created[i]
			// #nosec G201 — r.table is a literal from this file, never input.
			if _, err := s.pool.Exec(context.Background(),
				`DELETE FROM `+r.table+` WHERE id::text = $1`, r.id); err != nil {
				t.Errorf("cleanup %s %s: %v", r.table, r.id, err)
			}
		}
	})

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	exec := func(what, table, id, sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
		created = append(created, row{table, id})
	}
	scan := func(what, table, sql string, args ...any) string {
		t.Helper()
		var id string
		if err := s.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", what, err)
		}
		created = append(created, row{table, id})
		return id
	}

	ids[kindUser] = scan("user", "users", `
		INSERT INTO users (email, username, password_hash)
		VALUES ($1, $2, 'x') RETURNING id::text`,
		"namestest-"+suffix+"@audit.test", "namestest-"+suffix)

	ids[kindApp] = scan("app", "apps", `
		INSERT INTO apps (name, runtime_spec) VALUES ('Steam', '{}'::jsonb) RETURNING id::text`)

	ids[kindHost] = scan("host", "hosts", `
		INSERT INTO hosts (node_name, node_secret_hash) VALUES ($1, 'x') RETURNING id::text`,
		"gpu-test-"+suffix)

	ids[kindSession] = scan("session", "sessions", `
		INSERT INTO sessions (user_id, app_id, host_id, state, width, height, fps, bitrate_kbps)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'running', 1920, 1080, 60, 8000) RETURNING id::text`,
		ids[kindUser], ids[kindApp], ids[kindHost])

	ids[kindRuntimePreset] = scan("runtime preset", "runtime_presets", `
		INSERT INTO runtime_presets (name, image) VALUES ($1, 'x') RETURNING id::text`,
		"names-preset-"+suffix)

	ids[kindStreamProfile] = "names-rung-" + suffix
	exec("stream profile", "stream_profiles", ids[kindStreamProfile], `
		INSERT INTO stream_profiles (id, display_name, width, height, fps, nominal_bitrate_kbps, h264_profile)
		VALUES ($1, 'Names Rung', 1920, 1080, 60, 8000, 'constrained-baseline')`, ids[kindStreamProfile])

	ids[kindLaunchProfile] = "names-chain-" + suffix
	exec("launch profile", "launch_profiles", ids[kindLaunchProfile], `
		INSERT INTO launch_profiles (id, display_name) VALUES ($1, 'Names Chain')`, ids[kindLaunchProfile])

	ids[kindImage] = "names-image-" + suffix
	exec("image", "image_catalog", ids[kindImage], `
		INSERT INTO image_catalog (id, manifest_version, display_name, kind, version, registry_ref, raw)
		VALUES ($1, 1, 'Names Image', 'prebuilt', '1.0.0', 'example/names:1.0.0', '{}'::jsonb)`, ids[kindImage])

	ids[kindJob] = "names.job." + suffix
	exec("job", "jobs", ids[kindJob], `
		INSERT INTO jobs (id, name, plane, scope, schedule_kind, interval_secs)
		VALUES ($1, 'Names Job', 'control', 'instance', 'interval', 3600)`, ids[kindJob])

	ids[kindRelease] = scan("release", "platform_releases", `
		INSERT INTO platform_releases (channel, version, source_commit, built_at, schema_version, manifest)
		VALUES ('stable', '0.9.9', $1, now(), 1, '{}'::jsonb) RETURNING id::text`,
		"c0ffee"+suffix)

	ids[kindApplyRun] = scan("apply run", "platform_apply_runs", `
		INSERT INTO platform_apply_runs (release_id, state) VALUES ($1::uuid, 'pending')
		RETURNING id::text`, ids[kindRelease])

	ids[kindHostEnrollment] = scan("host enrollment", "host_enrollments", `
		INSERT INTO host_enrollments (token_hash, node_name, max_uses, created_by)
		VALUES ($1, 'enrolling-host', 1, $2::uuid) RETURNING id::text`,
		"tok-"+suffix, ids[kindUser])

	return ids
}

func TestResolveNamesCoversEveryLookup(t *testing.T) {
	s := testStore(t)
	ids := seedNamedEntities(t, s)
	username := usernameOf(t, s, ids[kindUser])
	want := map[kind]string{
		kindApp:            "Steam",
		kindHost:           hostNameOf(t, s, ids[kindHost]),
		kindUser:           username,
		kindSession:        "Steam · " + username,
		kindRuntimePreset:  presetNameOf(t, s, ids[kindRuntimePreset]),
		kindStreamProfile:  "Names Rung",
		kindLaunchProfile:  "Names Chain",
		kindImage:          "Names Image",
		kindJob:            "Names Job",
		kindRelease:        "0.9.9",
		kindApplyRun:       "0.9.9",
		kindHostEnrollment: "enrolling-host",
	}
	if len(want) != len(nameQueries) {
		t.Fatalf("this test covers %d kinds but nameQueries has %d — a new lookup needs a case here",
			len(want), len(nameQueries))
	}
	refs := make([]ref, 0, len(ids))
	for k, id := range ids {
		refs = append(refs, ref{kind: k, id: id})
	}
	got, err := s.resolveNames(context.Background(), refs)
	if err != nil {
		t.Fatalf("resolveNames: %v", err)
	}
	for k, name := range want {
		if got[ref{kind: k, id: ids[k]}] != name {
			t.Errorf("%s resolved to %q, want %q", k, got[ref{kind: k, id: ids[k]}], name)
		}
	}
}

func TestListNamesTheTargetAndTheDetailIDs(t *testing.T) {
	s := testStore(t)
	ids := seedNamedEntities(t, s)
	ctx := context.Background()

	// The row from the operator's screenshot.
	if err := s.Record(ctx, ids[kindUser], "session.launched", "session", ids[kindSession],
		map[string]any{"app_id": ids[kindApp], "host_id": ids[kindHost]}); err != nil {
		t.Fatalf("record: %v", err)
	}
	items, _, err := s.List(ctx, 0, 10, ListFilter{Action: "session.launched"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	names := items[0].Names
	for id, want := range map[string]string{
		ids[kindSession]: "Steam · " + usernameOf(t, s, ids[kindUser]),
		ids[kindApp]:     "Steam",
		ids[kindHost]:    hostNameOf(t, s, ids[kindHost]),
	} {
		if names[id] != want {
			t.Errorf("names[%s] = %q, want %q", id, names[id], want)
		}
	}
}

func TestListFallsBackToTheStampedNameForADeletedApp(t *testing.T) {
	s := testStore(t)
	ids := seedNamedEntities(t, s)
	ctx := context.Background()

	if err := s.Record(ctx, ids[kindUser], "app.delete", "app", ids[kindApp],
		map[string]any{"name": "Steam", "delete_derived": false}); err != nil {
		t.Fatalf("record: %v", err)
	}
	// The seeded session references the app, so it goes first.
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1::uuid`, ids[kindSession]); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1::uuid`, ids[kindApp]); err != nil {
		t.Fatalf("delete app: %v", err)
	}
	items, _, err := s.List(ctx, 0, 10, ListFilter{Action: "app.delete"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if got := items[0].Names[ids[kindApp]]; got != "Steam" {
		t.Fatalf("names[%s] = %q, want the stamped name to survive the delete", ids[kindApp], got)
	}
}

func TestListNamesEveryRowEvenWhenNothingResolves(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.Record(ctx, "", "instance.settings.updated", "instance", "",
		map[string]any{"keys": []string{"mic_capture_enabled"}}); err != nil {
		t.Fatalf("record: %v", err)
	}
	items, _, err := s.List(ctx, 0, 10, ListFilter{Action: "instance.settings"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Names == nil {
		t.Fatal("Names must be non-nil on every item, so a client can index it without a guard")
	}
}

func scalar(t *testing.T, s *Store, sql, id string) string {
	t.Helper()
	var v string
	if err := s.pool.QueryRow(context.Background(), sql, id).Scan(&v); err != nil {
		t.Fatalf("read name: %v", err)
	}
	return v
}

func usernameOf(t *testing.T, s *Store, id string) string {
	return scalar(t, s, `SELECT username FROM users WHERE id::text = $1`, id)
}

func hostNameOf(t *testing.T, s *Store, id string) string {
	return scalar(t, s, `SELECT node_name FROM hosts WHERE id::text = $1`, id)
}

func presetNameOf(t *testing.T, s *Store, id string) string {
	return scalar(t, s, `SELECT name FROM runtime_presets WHERE id::text = $1`, id)
}
