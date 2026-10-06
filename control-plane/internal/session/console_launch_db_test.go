package session

import (
	"context"
	"errors"
	"testing"
)

// Amendment 19 (#455): a console app that does not declare
// runtime_spec.direct_display is refused before anything is scheduled — a
// readiness failure, not a launch. The agentws auto-start gate refuses first;
// this is the backstop behind it.
func TestConsoleLaunchRefusesAppThatCannotRunDirect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	coord := newTestCoordinator(t, store, newCapturingDispatcher(), testLogger())
	ctx := context.Background()

	_, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "local_only", 1920, 1080, 60)
	if !errors.Is(err, ErrConsoleAppNotDirect) {
		t.Fatalf("console launch of an app without direct_display: err = %v, want ErrConsoleAppNotDirect", err)
	}
	var sessions int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = $1::uuid`, s.userID).Scan(&sessions))
	if sessions != 0 {
		t.Fatalf("refused console launch left %d session row(s), want none", sessions)
	}

	// Declared, the same app launches local_only.
	must(t, exec(t, pool, `UPDATE apps SET runtime_spec = runtime_spec || '{"direct_display":true}'::jsonb
		WHERE id::text = $1`, s.appID))
	if _, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "local_only", 1920, 1080, 60); err != nil {
		t.Fatalf("console launch of a direct app: %v", err)
	}
	// The retired topology fails closed even for a direct app.
	if _, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "dual_output", 1920, 1080, 60); err == nil {
		t.Fatal("dual_output console launch accepted after amendment 19 retired it")
	}
}
