package session

// migration_0105_db_test.go — warm Steam. The down migration refuses while a
// parked session is live, then deletes the finished parked rows so none comes
// back as a play. Runs in a scratch database: it migrates down.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration0105DownRefusesALiveParkedSession(t *testing.T) {
	url := scratchDB(t)
	m := newMigrator(t, url)
	migrateTo(t, m, 105)

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer pool.Close()
	s := seed(t, pool, 4)
	// A raw insert: today's ScheduleAndCreate writes columns later than 0105.
	var playID string
	must(t, pool.QueryRow(ctx, `INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state, width, height,
		fps, bitrate_kbps, h264_profile, reserved_encode_slots, assigned_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'assigned', 1280, 720, 60, 6000,
		        'constrained-baseline', 1, now()) RETURNING id::text`,
		s.userID, s.appID, s.hostID, s.gpuID).Scan(&playID))
	parked := insertParked(t, pool, warmUser(t, pool, "parker"), s.appID, s.hostID, s.gpuID, 1, "10 minutes")

	err = m.Migrate(104)
	if err == nil || !strings.Contains(err.Error(), "parked session is still live") {
		t.Fatalf("down with a live parked session: err = %v, want the refusal", err)
	}
	// The refusal rolled back, but its connection still holds the migrate lock
	// and the dirty flag: reopen, clear the flag, retry.
	_, _ = m.Close()
	m = newMigrator(t, url)
	if err := m.Force(105); err != nil {
		t.Fatalf("force: %v", err)
	}
	must(t, exec(t, pool, `UPDATE sessions SET state = 'stopped', ended_at = now() WHERE id = $1::uuid`, parked))
	migrateTo(t, m, 104)

	var n int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1::uuid`, parked).Scan(&n))
	if n != 0 {
		t.Error("the finished parked row survived the down migration and now reads as a play")
	}
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1::uuid`, playID).Scan(&n))
	if n != 1 {
		t.Error("the down migration deleted a play session")
	}
	var cols int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'sessions' AND column_name IN ('kind','parked_until','launch_progress'))
		   OR (table_name = 'instance_settings' AND column_name LIKE 'steam_warm_%')
		   OR (table_name = 'users' AND column_name = 'steam_warm_suppressed')
		   OR (table_name = 'user_profile_preferences' AND column_name = 'steam_dialogs')`).Scan(&cols))
	if cols != 0 {
		t.Errorf("%d warm-Steam columns left after down", cols)
	}

	migrateTo(t, m, 105)
	var kind string
	must(t, pool.QueryRow(ctx, `SELECT kind FROM sessions WHERE id = $1::uuid`, playID).Scan(&kind))
	if kind != KindPlay {
		t.Errorf("kind = %q after up, want the play default", kind)
	}
	// The partial unique index: one live parked session per user.
	user := warmUser(t, pool, "twice")
	insertParked(t, pool, user, s.appID, s.hostID, s.gpuID, 1, "10 minutes")
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (user_id, app_id, state, width, height, fps,
		bitrate_kbps, h264_profile, kind) VALUES ($1::uuid, $2::uuid, 'pending', 1, 1, 1, 1,
		'constrained-baseline', 'parked')`, user, s.appID); err == nil {
		t.Error("a second live parked session for one user was accepted")
	}
}
