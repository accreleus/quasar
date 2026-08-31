package session

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// countTraceEvents is the shared row-count helper for the package's trace tests.
func countTraceEvents(t *testing.T, pool *pgxpool.Pool, sessionID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_trace_events WHERE session_id = $1::uuid`, sessionID).Scan(&n); err != nil {
		t.Fatalf("count trace events: %v", err)
	}
	return n
}

// --- trust boundary -----------------------------------------------------------

// TestAgentTraceEventAllowed: the host-ownership boundary mirrors AgentMetrics —
// allowed for the owning host on a running session; dropped cross-host, dropped for a
// non-running session, dropped for an unknown id.
func TestAgentTraceEventAllowed(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()
	sess := runningSession(t, store, s)
	sid := sess.ID

	// Owning host + running ⇒ allowed.
	ok, err := store.AgentTraceEventAllowed(ctx, s.hostID, sid)
	if err != nil || !ok {
		t.Fatalf("owning-host running: ok=%v err=%v want true", ok, err)
	}

	// Cross-host ⇒ dropped.
	otherHost := "00000000-0000-0000-0000-0000000000aa"
	if sess.HostID != nil && *sess.HostID == otherHost {
		otherHost = "00000000-0000-0000-0000-0000000000bb"
	}
	ok, err = store.AgentTraceEventAllowed(ctx, otherHost, sid)
	if err != nil || ok {
		t.Fatalf("cross-host: ok=%v err=%v want false", ok, err)
	}

	// Unknown session ⇒ dropped (no error).
	ok, err = store.AgentTraceEventAllowed(ctx, s.hostID, "00000000-0000-0000-0000-0000000000cc")
	if err != nil || ok {
		t.Fatalf("unknown session: ok=%v err=%v want false,nil", ok, err)
	}

	// Assigned-but-not-running ⇒ dropped.
	assigned, _ := store.ScheduleAndCreate(ctx, launchParams(s))
	ok, err = store.AgentTraceEventAllowed(ctx, s.hostID, assigned.ID)
	if err != nil || ok {
		t.Fatalf("non-running: ok=%v err=%v want false", ok, err)
	}
}
