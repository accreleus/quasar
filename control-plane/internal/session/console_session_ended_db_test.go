package session

import (
	"context"
	"log/slog"
	"testing"
)

// #782: only a console session (a play session that reserves no encode slot) its
// user stopped holds console auto-start back; a user stopping a streamed session
// must not.
func TestConsoleSessionEndedHoldsOnlyAUserStoppedConsoleSession(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	coord := newTestCoordinator(t, NewStore(pool), nil, slog.Default())
	ctx := context.Background()
	insert := func(slots int, reason string) string {
		t.Helper()
		var id string
		must(t, pool.QueryRow(ctx, `INSERT INTO sessions
			(user_id, app_id, host_id, state, width, height, fps, bitrate_kbps,
			 reserved_encode_slots, stop_reason, ended_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'stopped', 1920, 1080, 60, 2000, $4, $5, now())
			RETURNING id::text`, s.userID, s.appID, s.hostID, slots, reason).Scan(&id))
		return id
	}
	cases := []struct {
		name   string
		slots  int
		reason string
		want   bool
	}{
		{"console stopped by its user", 0, stopReasonUserRequested, true},
		{"streamed session stopped by its user", 1, stopReasonUserRequested, false},
		{"console stopped by the control plane", 0, "console_display_disconnected", false},
	}
	for _, c := range cases {
		endedAt, held := coord.ConsoleSessionEnded(ctx, insert(c.slots, c.reason))
		if held != c.want {
			t.Errorf("%s: user stop = %v, want %v", c.name, held, c.want)
		}
		if endedAt.IsZero() {
			t.Errorf("%s: no end time", c.name)
		}
	}
	if endedAt, held := coord.ConsoleSessionEnded(ctx, "00000000-0000-0000-0000-000000000000"); held || !endedAt.IsZero() {
		t.Errorf("unknown session: endedAt=%v held=%v, want zero and false", endedAt, held)
	}
}
