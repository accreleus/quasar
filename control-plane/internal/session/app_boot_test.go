// app_boot_test.go — #484 WP-B: the control plane must pass "app booting" /
// "app presented" through to state_detail exactly like the swap detail tokens,
// and — the load-bearing property — must never mistake "app presented" for the
// swap-commit token "swap complete" (which would wrongly move app_id).
package session

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// TestAppBootDetailPassesThroughGenZero: a generation-0 launch (no swap in
// flight) reports "app booting" then "app presented" within `running`. Both
// details must land verbatim in state_detail, the top-level state must stay
// running, and app_id must not move — because with no pending swap,
// handleSwapCallback's `if !pending { return false }` never engages, and the
// callback falls straight through to the ordinary Coordinator.store.Transition
// path (the same path every non-swap detail already takes).
func TestAppBootDetailPassesThroughGenZero(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, err := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	waitFor(t, func() bool { return len(disp.types()) == 2 })
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	origAppID := res.Session.AppID

	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID, State: "running", Detail: appDetailBooting,
	})
	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StateDetail == nil || *got.StateDetail != appDetailBooting {
		t.Fatalf("app booting detail: got %v want %q", got.StateDetail, appDetailBooting)
	}
	if got.State != StateRunning {
		t.Fatalf("app booting changed top-level state: %s want running", got.State)
	}
	if got.AppID != origAppID {
		t.Fatalf("app booting moved app_id: %s -> %s", origAppID, got.AppID)
	}

	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID, State: "running", Detail: appDetailPresented,
	})
	got, err = store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StateDetail == nil || *got.StateDetail != appDetailPresented {
		t.Fatalf("app presented detail: got %v want %q", got.StateDetail, appDetailPresented)
	}
	if got.State != StateRunning {
		t.Fatalf("app presented changed top-level state: %s want running", got.State)
	}
	if got.AppID != origAppID {
		t.Fatalf("app presented moved app_id: %s -> %s", origAppID, got.AppID)
	}
}

// TestAppPresentedNotMistakenForSwapCommit: the load-bearing assertion the
// design demands. While a swap IS pending (the only state in which
// handleSwapCallback's switch is even reached), an "app presented" detail
// (emitted by the swap-path convergence — perform_swap now shares the same
// vocabulary as gen-0) must NOT be confused with the exact-match swap-commit
// token "swap complete": it must not commit the pending swap's app_id, must
// not clear the pending-swap tracking, and must fall into the `default:` arm
// that just records the detail string. A subsequent genuine "swap complete"
// on the same session must still commit normally, proving the two tokens are
// distinguished by exact string equality, not by any shared prefix/substring.
func TestAppPresentedNotMistakenForSwapCommit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	appB := insertApp(t, pool, "appB", 512, 1)
	origAppID := sess.AppID

	if _, err := coord.Swap(ctx, sess.ID, appB); err != nil {
		t.Fatalf("swap: %v", err)
	}
	waitFor(t, func() bool {
		for _, ty := range disp.types() {
			if ty == "swap" {
				return true
			}
		}
		return false
	})

	// Swap-path convergence (§3.1): the agent now emits the same app-boot
	// vocabulary during a swap, before the eventual "swap complete".
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: appDetailBooting,
	})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: appDetailPresented,
	})

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AppID != origAppID {
		t.Fatalf("%q was mistaken for a swap commit: app_id moved %s -> %s (want unchanged %s)",
			appDetailPresented, origAppID, got.AppID, origAppID)
	}
	if got.StateDetail == nil || *got.StateDetail != appDetailPresented {
		t.Fatalf("app presented detail during swap: got %v want %q", got.StateDetail, appDetailPresented)
	}
	if got.State != StateRunning {
		t.Fatalf("app presented during swap changed top-level state: %s want running", got.State)
	}

	// The pending swap must still be tracked — "app presented" must not have
	// cleared it the way "swap complete" does.
	coord.swapper.mu.Lock()
	_, stillPending := coord.swapper.pendingSwaps[sess.ID]
	coord.swapper.mu.Unlock()
	if !stillPending {
		t.Fatalf("%q cleared the pending swap; only %q may do that", appDetailPresented, swapDetailComplete)
	}

	// The real swap-commit token still works and now actually commits appB.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: swapDetailComplete,
	})
	got, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AppID != appB {
		t.Fatalf("swap complete did not commit: app_id=%s want %s", got.AppID, appB)
	}
}
