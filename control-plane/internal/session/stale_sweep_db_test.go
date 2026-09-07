package session

import (
	"context"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestStaleSweepMeasuresFromBoot (#128): the sweep must measure from
// max(last_heartbeat_at, boot). Without the boot term, a control plane that
// restarts after a quiet period reaps every session in its first tick — before
// any agent's reconnect can land, which is the failure this issue is about.
func TestStaleSweepMeasuresFromBoot(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithAgentConnectivity(fakeAgents(false)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	must(t, err)
	waitFor(t, func() bool { return len(disp.types()) >= 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	// The host last spoke ten minutes ago, but we booted one second ago.
	mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Second), 2*time.Minute)

	if got, _ := store.Get(ctx, res.Session.ID); got.State != StateRunning {
		t.Fatalf("swept at boot = %s, want running: the grace runs from boot", got.State)
	}

	// Booted long ago: the host really has been silent past the window.
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Hour), 2*time.Minute)

	if got, _ := store.Get(ctx, res.Session.ID); got.State != StateFailed {
		t.Fatalf("swept after grace = %s, want failed", got.State)
	}
	if slots := reservedSlots(t, pool, s.gpuID); slots != 0 {
		t.Fatalf("sweep left %d reserved slots; the backstop must release them", slots)
	}
}

// TestStaleSweepSkipsConnectedHost (#128): a connected agent is alive whatever
// its heartbeat column says.
func TestStaleSweepSkipsConnectedHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger(), WithAgentConnectivity(fakeAgents(true)))
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	must(t, err)
	waitFor(t, func() bool { return len(disp.types()) >= 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Hour), time.Minute)

	if got, _ := store.Get(ctx, res.Session.ID); got.State != StateRunning {
		t.Fatalf("connected host swept: state=%s, want running", got.State)
	}
}

// TestStaleSweepWithoutConnectivityIsInert (#128): an unwired backstop cannot
// tell a live host from a dead one, so it must reap nothing rather than
// everything.
func TestStaleSweepWithoutConnectivityIsInert(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	must(t, err)
	waitFor(t, func() bool { return len(disp.types()) >= 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))
	coord.sweepStaleHosts(ctx, time.Now().Add(-time.Hour), time.Minute)

	if got, _ := store.Get(ctx, res.Session.ID); got.State != StateRunning {
		t.Fatalf("unwired sweep reaped: state=%s, want running", got.State)
	}
}

// mustSetHeartbeat backdates a host's last_heartbeat_at so the sweep's window
// can be exercised without sleeping.
func mustSetHeartbeat(t *testing.T, pool *pgxpool.Pool, hostID string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET last_heartbeat_at = $2 WHERE id = $1::uuid`, hostID, at); err != nil {
		t.Fatalf("set heartbeat: %v", err)
	}
}
