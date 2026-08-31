package session

import (
	"context"
	"sort"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// recordingForgetter is a stand-in for agentws.RelayBus (which session cannot
// import — see SessionForgetter).
type recordingForgetter struct {
	mu  sync.Mutex
	ids []string
}

func (r *recordingForgetter) Forget(sessionID string) {
	r.mu.Lock()
	r.ids = append(r.ids, sessionID)
	r.mu.Unlock()
}

func (r *recordingForgetter) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.ids...)
	sort.Strings(out)
	return out
}

func (r *recordingForgetter) sawSession(id string) bool {
	for _, s := range r.seen() {
		if s == id {
			return true
		}
	}
	return false
}

// TestForgetterFiredOnAgentTerminalState: an agent-reported terminal state
// (the normal end of a session) must evict the relay's per-session buffer
// (#402). Before the wiring landed nothing on the terminal path touched the
// relay at all.
func TestForgetterFiredOnAgentTerminalState(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	fg := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithSessionForgetter(fg))
	ctx := context.Background()

	sess := runningSession(t, store, s)
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		Type: "session_state", SessionID: sess.ID, State: string(StateStopped),
	})

	if !fg.sawSession(sess.ID) {
		t.Fatalf("terminal agent state did not forget the session: saw %v", fg.seen())
	}
}

// TestForgetterFiredOnCoordinatorFail: a control-plane-side fail (dispatch
// failure, unsustainable health) is the other terminal path off the agent
// callback, and it must evict too (#402).
func TestForgetterFiredOnCoordinatorFail(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	fg := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithSessionForgetter(fg))

	sess := runningSession(t, store, s)
	coord.failSession(sess.ID, "test-induced fault")

	if !fg.sawSession(sess.ID) {
		t.Fatalf("failSession did not forget the session: saw %v", fg.seen())
	}
}

// TestForgetterFiredOnHostDisconnect: the reap path drives sessions terminal
// with a bulk UPDATE and no per-session callback, so it evicts from the
// pre-read id list — the same shape healthEvaluator.forget already uses (#402).
func TestForgetterFiredOnHostDisconnect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	fg := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithSessionForgetter(fg))
	ctx := context.Background()

	sess := runningSession(t, store, s)
	coord.HostDisconnected(ctx, s.hostID)

	if !fg.sawSession(sess.ID) {
		t.Fatalf("host disconnect reap did not forget the session: saw %v", fg.seen())
	}
}

// TestForgetterFiredOnAgentReconnect: the reconnect reconcile is the fourth
// terminal path (#402).
func TestForgetterFiredOnAgentReconnect(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	fg := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithSessionForgetter(fg))
	ctx := context.Background()

	sess := runningSession(t, store, s)
	coord.AgentReconnected(ctx, s.hostID)

	if !fg.sawSession(sess.ID) {
		t.Fatalf("agent reconnect reconcile did not forget the session: saw %v", fg.seen())
	}
}

// TestNoForgetterIsSafe: a coordinator built without one (every existing test,
// and any deployment with nothing to evict) must not panic on a terminal
// transition.
func TestNoForgetterIsSafe(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	sess := runningSession(t, store, s)
	coord.AgentState(context.Background(), s.hostID, agentws.SessionStateMsg{
		Type: "session_state", SessionID: sess.ID, State: string(StateStopped),
	})
}
