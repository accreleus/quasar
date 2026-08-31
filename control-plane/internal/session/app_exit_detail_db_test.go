package session

import (
	"context"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// First-run-experience S5 (#463): an app container that exits before producing
// a frame used to surface as "media path interrupted", and the decisive lines
// died with the `--rm` container. The agent's classification AND its captured
// log tail must both reach the session row on the ordinary failed callback.
func TestAgentStateStoresTheAppExitFailureDetail(t *testing.T) {
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
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "starting"})

	reason := "the app exited with code 0 before producing any video. Check the app container log."
	code := "app_exited_early"
	logTail := "Steam needs to be online to update\nPlease confirm your network connection"
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID:  res.Session.ID,
		State:      "failed",
		Error:      &reason,
		ReasonCode: &code,
		AppLogTail: &logTail,
	})

	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("state: got %s want failed", got.State)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != reason {
		t.Errorf("error_message: got %v, want the operator prose", got.ErrorMessage)
	}
	if got.FailureCode == nil || *got.FailureCode != code {
		t.Fatalf("failure_code: got %v, want %q — the UI branches on this, not on prose",
			got.FailureCode, code)
	}
	if got.AppLogTail == nil || !strings.Contains(*got.AppLogTail, "Steam needs to be online") {
		t.Fatalf("app_log_tail: got %v, want the captured container log", got.AppLogTail)
	}
}

// The classification must be strictly additive: an ordinary failure callback —
// every pre-amendment agent, and every failure that is not an early app exit —
// leaves both columns NULL rather than acquiring an empty-string code that the
// UI would render as an unknown failure kind.
func TestAgentStateLeavesFailureDetailNullWhenNotReported(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	res, _ := coord.Launch(ctx, s.userID, s.appID, StreamOverride{})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "starting"})
	boom := "encode pipeline error"
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: res.Session.ID, State: "failed", Error: &boom,
	})

	got, _ := store.Get(ctx, res.Session.ID)
	if got.FailureCode != nil {
		t.Errorf("failure_code: got %q, want NULL for an unclassified failure", *got.FailureCode)
	}
	if got.AppLogTail != nil {
		t.Errorf("app_log_tail: got %q, want NULL", *got.AppLogTail)
	}
}

// SetFailureDetail must NOT be state-guarded. It runs after the terminal
// transition has already committed, so a `state NOT IN ('stopped','failed')`
// predicate — the shape used by the live-session updaters — would match nothing
// and silently discard every log tail.
func TestSetFailureDetailWritesToAnAlreadyFailedRow(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	boom := "app exited"
	if _, err := store.Transition(ctx, sess.ID, StateFailed, nil, &boom); err != nil {
		t.Fatalf("→ failed: %v", err)
	}

	code, tail := "app_exited_early", "line one\nline two"
	if err := store.SetFailureDetail(ctx, sess.ID, &code, &tail); err != nil {
		t.Fatalf("set failure detail: %v", err)
	}
	got, _ := store.Get(ctx, sess.ID)
	if got.FailureCode == nil || *got.FailureCode != code {
		t.Fatalf("failure_code on a terminal row: got %v want %q", got.FailureCode, code)
	}
	if got.AppLogTail == nil || *got.AppLogTail != tail {
		t.Fatalf("app_log_tail on a terminal row: got %v want %q", got.AppLogTail, tail)
	}

	// A follow-up report carrying only one of the two must not blank the other
	// (COALESCE, not overwrite-with-null).
	only := "app_exited_early"
	if err := store.SetFailureDetail(ctx, sess.ID, &only, nil); err != nil {
		t.Fatalf("partial update: %v", err)
	}
	got, _ = store.Get(ctx, sess.ID)
	if got.AppLogTail == nil || *got.AppLogTail != tail {
		t.Fatalf("a partial report blanked app_log_tail: got %v", got.AppLogTail)
	}
}

// Review round 2, finding #1 — the operator-stop race.
//
// An app can exit at the same moment an operator stops the session. Transition
// COERCES a `failed` report on an already-`stopping` row to `stopped`: that is a
// clean teardown, and the coercion deliberately clears error_message so the row
// is not user-visibly a failure. The S5 detail write must respect that verdict.
// Keying off the agent's message instead re-attaches failure_code and a hundred
// lines of app log to a cleanly stopped session, which renders in the admin UI
// as an app crash the operator did not cause.
func TestAppExitDetailIsNotAttachedWhenAStopRaceCoercesToStopped(t *testing.T) {
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
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "starting"})
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: res.Session.ID, State: "running"})

	// The operator stops; the row goes to `stopping`.
	if _, err := coord.Stop(ctx, res.Session.ID, "user_requested"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// …and the agent's app-exit report lands in that window.
	reason := "the app exited with code 0 before producing any video."
	code := "app_exited_early"
	logTail := "Steam needs to be online to update"
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID:  res.Session.ID,
		State:      "failed",
		Error:      &reason,
		ReasonCode: &code,
		AppLogTail: &logTail,
	})

	got, err := store.Get(ctx, res.Session.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateStopped {
		t.Fatalf("state: got %s, want stopped (the stop-race coercion)", got.State)
	}
	if got.FailureCode != nil {
		t.Errorf("failure_code %q attached to a cleanly stopped session — it contradicts the "+
			"coercion and renders an operator stop as an app crash", *got.FailureCode)
	}
	if got.AppLogTail != nil {
		t.Errorf("app_log_tail attached to a cleanly stopped session: %q", *got.AppLogTail)
	}
}
