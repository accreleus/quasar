package session

// DB tests for a failed agent-to-browser relay (AgentSignalingFailure). Require Postgres
// (make test-db).

import (
	"context"
	"slices"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// relayFailureCase is one `running` session on the fake agent.
func relayFailureCase(t *testing.T) (*Store, *fakeDispatcher, *Coordinator, seedIDs, string) {
	t.Helper()
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	return store, disp, newTestCoordinator(t, store, disp, testLogger()), s, runningSession(t, store, s).ID
}

// #753: the agent's `bye` after a game exit rides on a `stopping` row. A browser socket
// too stuck to take it must not stop the session again or fail it: the agent ends it.
func TestSignalingRelayFailureLeavesAStoppingSessionToFinish(t *testing.T) {
	store, disp, coord, s, id := relayFailureCase(t)
	ctx := context.Background()
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "stopping", Detail: "game exited"})

	coord.AgentSignalingFailure(ctx, s.hostID, id, "relay browser queue blocked")

	got, _ := store.Get(ctx, id)
	if got.State != StateStopping || got.StateDetail == nil || *got.StateDetail != "game exited" || got.ErrorMessage != nil {
		t.Fatalf("after the relay failure: %s %v %v, want stopping / game exited, no error", got.State, got.StateDetail, got.ErrorMessage)
	}
	if slices.Contains(disp.noAckTypes(), "stop:"+id) {
		t.Fatalf("a stop was sent to a session already stopping: %v", disp.noAckTypes())
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: id, State: "stopped", Detail: "game exited"})
	got, _ = store.Get(ctx, id)
	if got.State != StateStopped || got.StateDetail == nil || *got.StateDetail != "game exited" {
		t.Fatalf("the agent's end: %s %v, want stopped / game exited", got.State, got.StateDetail)
	}
}

// The guard is that narrow: a running session whose relay fails is still stopped and failed.
func TestSignalingRelayFailureStillFailsARunningSession(t *testing.T) {
	store, disp, coord, s, id := relayFailureCase(t)
	ctx := context.Background()

	coord.AgentSignalingFailure(ctx, s.hostID, id, "relay browser queue blocked")

	if r := disp.stopReason(id); r != "signaling_relay_failed" {
		t.Fatalf("stop reason %q, want signaling_relay_failed", r)
	}
	got, _ := store.Get(ctx, id)
	if got.State != StateFailed || got.ErrorMessage == nil || *got.ErrorMessage != "signaling relay failed: relay browser queue blocked" {
		t.Fatalf("after the relay failure: %s %v, want failed with the relay error", got.State, got.ErrorMessage)
	}
}
