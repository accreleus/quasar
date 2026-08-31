package session

import (
	"context"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// P2-06 session-lifecycle hardening. These exercise the timeout/reconciliation
// edges that multi-user adds: a start that hangs, an agent that restarts and
// forgets its sessions, and idempotent teardown under concurrency. Integration
// tests — need Postgres.

// TestStuckStartTimeout: the agent acks assign+start but never reports running
// (e.g. the image hangs on pull, or the compositor never announces its socket).
// The stuck-start watchdog must drive the session terminal and release its
// reservation, and tell the agent to tear down.
func TestStuckStartTimeout(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1) // single encode slot, so release is observable
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	coord.startToRunningTimeout = 100 * time.Millisecond // fire fast in the test
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	// assign+start are acked by the fake; we deliberately send NO running callback.
	waitFor(t, func() bool { return len(disp.types()) >= 2 })

	// The watchdog must fail the session once the start-to-running window elapses.
	waitFor(t, func() bool {
		got, _ := store.Get(ctx, res.Session.ID)
		return got.State == StateFailed
	})
	got, _ := store.Get(ctx, res.Session.ID)
	if got.ErrorMessage == nil {
		t.Fatal("stuck-start failure left no error_message")
	}

	// Reservation released: the single slot is free again (a new launch succeeds).
	if slots := reservedSlots(t, pool, s.gpuID); slots != 0 {
		t.Fatalf("reservation not released after stuck-start: %d slots still held", slots)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("slot not free after stuck-start reap: %v", err)
	}

	// The agent was told to tear the stuck session down.
	waitFor(t, func() bool {
		for _, ty := range disp.types() {
			if ty == "stop" {
				return true
			}
		}
		return false
	})
}

// TestStartReachesRunningBeforeTimeout: the happy path must NOT be reaped — a
// session that reports running within the window is left alone by the watchdog.
func TestStartReachesRunningBeforeTimeout(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	coord.startToRunningTimeout = 200 * time.Millisecond
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) >= 2 })
	// Report running before the window elapses.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	// Wait past the watchdog window; the session must remain running, not reaped.
	time.Sleep(400 * time.Millisecond)
	got, _ := store.Get(ctx, res.Session.ID)
	if got.State != StateRunning {
		t.Fatalf("watchdog reaped a healthy running session: state=%s", got.State)
	}
}

// TestAgentReconnectReconciliation: a fresh agent connection (the agent restarted
// and forgot its sessions) must reconcile — the control plane fails the stale
// non-terminal sessions it still believes are on that host, releasing each
// reservation.
func TestAgentReconnectReconciliation(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// Two sessions reach running on the host.
	r1, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch 1: %v", err)
	}
	r2, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch 2: %v", err)
	}
	for _, id := range []string{r1.Session.ID, r2.Session.ID} {
		coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "running"})
	}
	if slots := reservedSlots(t, pool, s.gpuID); slots != 2 {
		t.Fatalf("setup: expected 2 reserved slots, got %d", slots)
	}

	// The agent reconnects fresh (process restarted): reconcile.
	coord.AgentReconnected(ctx, s.hostID)

	for _, id := range []string{r1.Session.ID, r2.Session.ID} {
		got, _ := store.Get(ctx, id)
		if got.State != StateFailed {
			t.Fatalf("session %s not reconciled to failed: state=%s", id, got.State)
		}
	}
	if slots := reservedSlots(t, pool, s.gpuID); slots != 0 {
		t.Fatalf("reservations not released after reconcile: %d slots still held", slots)
	}
}

// TestDoubleStopIdempotent: stopping an already-terminal session is a no-op that
// returns the session unchanged (control-api.md idempotency).
func TestDoubleStopIdempotent(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	if _, err := coord.Stop(ctx, res.Session.ID, "user_requested"); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "stopped"})

	first, _ := store.Get(ctx, res.Session.ID)
	// Second stop on a terminal session: no error, state unchanged.
	again, err := coord.Stop(ctx, res.Session.ID, "user_requested")
	if err != nil {
		t.Fatalf("double stop returned error: %v", err)
	}
	if again.State != StateStopped {
		t.Fatalf("double stop changed state: got %s want stopped", again.State)
	}
	if first.EndedAt == nil || again.EndedAt == nil || !first.EndedAt.Equal(*again.EndedAt) {
		t.Fatalf("double stop churned ended_at: %v vs %v", first.EndedAt, again.EndedAt)
	}
}

// TestStopOneDoesNotAffectAnother: with two concurrent sessions, stopping one
// leaves the other running with its reservation intact (idempotent teardown is
// per-session — P2-05 substrate).
func TestStopOneDoesNotAffectAnother(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	a, _ := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	b, _ := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	for _, id := range []string{a.Session.ID, b.Session.ID} {
		coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "running"})
	}

	// Stop A; B must be untouched.
	if _, err := coord.Stop(ctx, a.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop A: %v", err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: a.Session.ID, State: "stopped"})

	gotB, _ := store.Get(ctx, b.Session.ID)
	if gotB.State != StateRunning {
		t.Fatalf("stopping A disturbed B: B state=%s want running", gotB.State)
	}
	// Exactly one slot is still reserved (B's).
	if slots := reservedSlots(t, pool, s.gpuID); slots != 1 {
		t.Fatalf("expected 1 reserved slot after stopping A, got %d", slots)
	}
}
