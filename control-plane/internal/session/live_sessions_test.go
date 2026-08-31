package session

import (
	"context"
	"testing"
)

// TestLiveSessions verifies that Store.LiveSessions counts only sessions in
// the active states (assigned, starting, running) and ignores terminal ones.
// It is a DB integration test — skipped without TEST_DATABASE_URL.
func TestLiveSessions(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	// Seed a user, app, and host (we need a valid host_id to attach sessions to).
	s := seed(t, pool, 4)

	// Confirm a host with no sessions returns 0.
	if got := store.LiveSessions(s.hostID); got != 0 {
		t.Fatalf("empty host: LiveSessions = %d, want 0", got)
	}

	// insertSession inserts a session row directly into the DB with the given
	// state and returns its id. We bypass ScheduleAndCreate so we can insert
	// arbitrary states (including terminal ones) without going through the
	// scheduler logic.
	insertSession := func(state string) string {
		var id string
		must(t, pool.QueryRow(ctx, `
			INSERT INTO sessions
				(user_id, app_id, host_id, state,
				 width, height, fps, bitrate_kbps)
			VALUES
				($1::uuid, $2::uuid, $3::uuid, $4,
				 1280, 720, 60, 6000)
			RETURNING id::text
		`, s.userID, s.appID, s.hostID, state).Scan(&id))
		return id
	}

	// Insert one session per active state.
	insertSession("assigned")
	insertSession("starting")
	insertSession("running")

	// Insert terminal states that must NOT be counted.
	insertSession("stopped")
	insertSession("failed")

	if got := store.LiveSessions(s.hostID); got != 3 {
		t.Fatalf("LiveSessions = %d, want 3 (assigned+starting+running)", got)
	}

	// A different host id (unknown) must return 0.
	if got := store.LiveSessions("00000000-0000-0000-0000-000000000099"); got != 0 {
		t.Fatalf("unknown host: LiveSessions = %d, want 0", got)
	}
}
