package session

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// P2-07 launcher↔game swap (control-plane side). Integration tests — need Postgres.

// insertApp adds an enabled app with the given resource defaults and returns its id.
func insertApp(t *testing.T, pool *pgxpool.Pool, name string, vramMB, slots int32) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ($1, $2, $3, 1280, 720, 60, 6000) RETURNING id::text`,
		name, vramMB, slots).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

// runningSession drives a freshly-scheduled session straight to running (bypassing
// the agent handshake) so swap tests start from a clean `running` row.
func runningSession(t *testing.T, store *Store, s seedIDs) Session {
	t.Helper()
	ctx := context.Background()
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	sess, err = store.Transition(ctx, sess.ID, StateRunning, nil, nil)
	if err != nil {
		t.Fatalf("→ running: %v", err)
	}
	return sess
}

// TestSwapRejectedNotRunning: a swap on a non-running session is rejected
// ErrSessionNotSwappable and the session is left untouched.
func TestSwapRejectedNotRunning(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	// Session is `assigned` (scheduled, not yet running).
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	appB := insertApp(t, pool, "appB", 512, 1)

	if _, err := coord.Swap(ctx, sess.ID, appB); !errors.Is(err, ErrSessionNotSwappable) {
		t.Fatalf("swap on assigned: got %v want ErrSessionNotSwappable", err)
	}
	got, _ := store.Get(ctx, sess.ID)
	if got.State != StateAssigned {
		t.Fatalf("rejected swap changed state: %s", got.State)
	}
}

// TestSwapExceedsReservation: a swap to an app that needs more than the session's
// held reservation is rejected ErrSwapExceedsReservation; no swap dispatched.
func TestSwapExceedsReservation(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)             // reserved 1024 MB + 1 slot (launchParams)
	bigApp := insertApp(t, pool, "bigApp", 1024, 2) // needs 2 slots > 1 reserved

	if _, err := coord.Swap(ctx, sess.ID, bigApp); !errors.Is(err, ErrSwapExceedsReservation) {
		t.Fatalf("over-reservation swap: got %v want ErrSwapExceedsReservation", err)
	}
	got, _ := store.Get(ctx, sess.ID)
	if got.AppID != sess.AppID {
		t.Fatalf("rejected swap changed app_id: %s → %s", sess.AppID, got.AppID)
	}
	for _, ty := range disp.types() {
		if ty == "swap" {
			t.Fatal("over-reservation swap should not dispatch session_swap_app")
		}
	}
}

// TestSwapUnknownApp: a swap to a missing/disabled app is rejected ErrNotFound.
func TestSwapUnknownApp(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	if _, err := coord.Swap(ctx, sess.ID, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("swap to unknown app: got %v want ErrNotFound", err)
	}
}

// TestSwapAcceptedAndCommitted: a valid swap is dispatched, marked swapping, and
// the agent's swap-complete callback commits the new app_id.
func TestSwapAcceptedAndCommitted(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	appB := insertApp(t, pool, "appB", 512, 1) // fits within reserved 1024 MB / 1 slot

	swapped, err := coord.Swap(ctx, sess.ID, appB)
	if err != nil {
		t.Fatalf("swap: %v", err)
	}
	if swapped.StateDetail == nil || *swapped.StateDetail != "swapping" {
		t.Fatalf("swap response detail: got %v want swapping", swapped.StateDetail)
	}
	// The swap is dispatched to the agent.
	waitFor(t, func() bool {
		for _, ty := range disp.types() {
			if ty == "swap" {
				return true
			}
		}
		return false
	})

	// Agent reports progress, then success.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "running", Detail: "swapping"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "running", Detail: "swap complete"})

	got, _ := store.Get(ctx, sess.ID)
	if got.AppID != appB {
		t.Fatalf("swap not committed: app_id=%s want %s", got.AppID, appB)
	}
	if got.State != StateRunning {
		t.Fatalf("swap changed top-level state: %s want running", got.State)
	}
}

// TestSwapRolledBack: when the agent reports a rolled-back swap, app_id is
// unchanged and the session keeps running.
func TestSwapRolledBack(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	appB := insertApp(t, pool, "appB", 512, 1)

	if _, err := coord.Swap(ctx, sess.ID, appB); err != nil {
		t.Fatalf("swap: %v", err)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: "swap failed; rolled back: image pull failed"})

	got, _ := store.Get(ctx, sess.ID)
	if got.AppID != sess.AppID {
		t.Fatalf("rolled-back swap changed app_id: %s → %s", sess.AppID, got.AppID)
	}
	if got.State != StateRunning {
		t.Fatalf("rolled-back swap changed state: %s want running", got.State)
	}
}

// TestSwapRejectedByAgent: an agent ack{ok:false} leaves the session running the
// previous app (a rejected swap never fails the session) and reverts the detail.
func TestSwapRejectedByAgent(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(false) // agent rejects the swap
	disp.ackErr = "does not hold this session"
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	appB := insertApp(t, pool, "appB", 512, 1)

	if _, err := coord.Swap(ctx, sess.ID, appB); err != nil {
		t.Fatalf("swap: %v", err)
	}
	// The async reject reverts the detail; the session stays running the old app.
	waitFor(t, func() bool {
		got, _ := store.Get(ctx, sess.ID)
		return got.StateDetail != nil && *got.StateDetail == "swap rejected"
	})
	got, _ := store.Get(ctx, sess.ID)
	if got.State != StateRunning {
		t.Fatalf("rejected swap changed state: %s want running", got.State)
	}
	if got.AppID != sess.AppID {
		t.Fatalf("rejected swap changed app_id: %s → %s", sess.AppID, got.AppID)
	}
}
