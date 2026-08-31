package session

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

func pendingSwapCount(c *Coordinator) int {
	c.swapper.mu.Lock()
	defer c.swapper.mu.Unlock()
	return len(c.swapper.pendingSwaps)
}

// TestSwapperForgetsOnAgentStop: a session with a swap in flight that ends any
// way other than through the swap protocol orphaned its pendingSwaps entry
// forever (#405). Entries were only ever cleared on the agent-reported failed /
// rolled back / complete edges and on dispatch rejection; an operator Stop, a
// host-disconnect reap or a plain agent-reported `stopped` all hit
// `if to != StateRunning { return false }` and left the entry behind.
func TestSwapperForgetsOnAgentStop(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	coord.swapper.mu.Lock()
	coord.swapper.pendingSwaps[sess.ID] = "some-app"
	coord.swapper.mu.Unlock()

	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		Type: "session_state", SessionID: sess.ID, State: string(StateStopped),
	})

	if n := pendingSwapCount(coord); n != 0 {
		t.Fatalf("pendingSwaps = %d after a terminal stop, want 0", n)
	}
}

// TestSwapperForgetsOnCoordinatorFail: the CP-side fail path (#405).
func TestSwapperForgetsOnCoordinatorFail(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	sess := runningSession(t, store, s)
	coord.swapper.mu.Lock()
	coord.swapper.pendingSwaps[sess.ID] = "some-app"
	coord.swapper.mu.Unlock()

	coord.failSession(sess.ID, "test-induced fault")

	if n := pendingSwapCount(coord); n != 0 {
		t.Fatalf("pendingSwaps = %d after failSession, want 0", n)
	}
}

// TestSwapperForgetsOnHostDisconnect: the reap path (#405).
func TestSwapperForgetsOnHostDisconnect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	sess := runningSession(t, store, s)
	coord.swapper.mu.Lock()
	coord.swapper.pendingSwaps[sess.ID] = "some-app"
	coord.swapper.mu.Unlock()

	coord.HostDisconnected(context.Background(), s.hostID)

	if n := pendingSwapCount(coord); n != 0 {
		t.Fatalf("pendingSwaps = %d after a host-disconnect reap, want 0", n)
	}
}

// TestSwapperForgetsOnAgentReconnect: the reconnect reconcile path (#405).
func TestSwapperForgetsOnAgentReconnect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	sess := runningSession(t, store, s)
	coord.swapper.mu.Lock()
	coord.swapper.pendingSwaps[sess.ID] = "some-app"
	coord.swapper.mu.Unlock()

	coord.AgentReconnected(context.Background(), s.hostID)

	if n := pendingSwapCount(coord); n != 0 {
		t.Fatalf("pendingSwaps = %d after an agent-reconnect reconcile, want 0", n)
	}
}
