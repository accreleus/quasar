// job_reclaim_test.go — #492: the coordinator's half of the eager reclaim.
//
// The jobs side proves WHAT is closed (internal/jobs/reclaim_db_test.go); this
// proves the agent re-register path actually asks for it, with this host's id and
// a reason an operator can read — and that a coordinator with no jobs framework
// wired still reconciles its sessions.
package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/jobs"
)

// recordingReclaimer is a stand-in for *jobs.Dispatcher (which session does not
// import — see JobReclaimer).
type recordingReclaimer struct {
	mu    sync.Mutex
	calls []struct{ hostID, reason string }
	n     int
	err   error
}

func (r *recordingReclaimer) reclaim(_ context.Context, hostID, reason string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct{ hostID, reason string }{hostID, reason})
	return r.n, r.err
}

func (r *recordingReclaimer) seen() []struct{ hostID, reason string } {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]struct{ hostID, reason string }(nil), r.calls...)
}

// TestAgentReconnectReclaimsHostJobRuns is the wiring claim: the ONE liveness
// signal the jobs framework ever gets about a host's agent must reach it.
func TestAgentReconnectReclaimsHostJobRuns(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	rec := &recordingReclaimer{n: 1}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithJobReclaimer(rec.reclaim))

	coord.AgentReconnected(context.Background(), s.hostID)

	calls := rec.seen()
	if len(calls) != 1 {
		t.Fatalf("agent re-register made %d reclaim calls, want 1", len(calls))
	}
	if calls[0].hostID != s.hostID {
		t.Errorf("reclaimed host %q, want %q", calls[0].hostID, s.hostID)
	}
	if calls[0].reason != jobReclaimReason {
		t.Errorf("reclaim reason = %q, want %q", calls[0].reason, jobReclaimReason)
	}
}

// TestHostDisconnectDoesNotReclaimJobRuns pins the asymmetry between the two
// halves of a reconnect.
//
// THE CONTRACT THE RECLAIM RESTS ON: a node-agent's job poller is
// CONNECTION-SCOPED, and its guard raises the poller's AbortFlag on teardown
// (node-agent/src/jobs/mod.rs, JobPollerGuard::drop). So by the time a
// registration arrives, the previous connection's claims have been abandoned
// agent-side — the pass stops and, crucially, will not report, because a
// spawn_blocking pass already in flight cannot be cancelled and would otherwise
// report minutes later. That is what makes "this host is not executing what it
// claimed" true rather than merely likely, including for the same-live-process
// websocket reconnect a control-plane restart causes.
//
// A DISCONNECT alone is a different thing: teardown has raised the flag, but a
// blocking pass may still be finishing and the connection may come back. Nothing
// is proven about the host's state until it re-registers, so the disconnect path
// leaves the rows alone and the claim-timeout reaper stays the authority.
func TestHostDisconnectDoesNotReclaimJobRuns(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	rec := &recordingReclaimer{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithJobReclaimer(rec.reclaim))

	coord.HostDisconnected(context.Background(), s.hostID)

	if calls := rec.seen(); len(calls) != 0 {
		t.Fatalf("a disconnect reclaimed job runs: %+v", calls)
	}
}

// TestAgentReconnectReconcilesSessionsWhenReclaimFails: the jobs seam is
// best-effort. A failing (or absent) reclaim must not cost the host its session
// reconciliation — that is what releases GPU reservations.
func TestAgentReconnectReconcilesSessionsWhenReclaimFails(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	rec := &recordingReclaimer{err: errors.New("jobs store is unreachable")}
	fg := &recordingForgetter{}
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
		WithJobReclaimer(rec.reclaim), WithSessionForgetter(fg))
	ctx := context.Background()

	sess := runningSession(t, store, s)
	coord.AgentReconnected(ctx, s.hostID)

	if !fg.sawSession(sess.ID) {
		t.Fatalf("a failed job reclaim skipped the session reconcile: saw %v", fg.seen())
	}
}

// TestReclaimReasonMatchesTheJobsFallback pins the matched pair described on
// jobReclaimReason. The import is TEST-ONLY: it catches a drift between the two
// literals without giving the production package a dependency the JobReclaimer
// seam exists to avoid.
func TestReclaimReasonMatchesTheJobsFallback(t *testing.T) {
	if jobReclaimReason != jobs.DefaultReclaimReason {
		t.Fatalf("reason drift: session %q vs jobs fallback %q — job_runs.error must read "+
			"the same whichever path wrote it", jobReclaimReason, jobs.DefaultReclaimReason)
	}
}

// TestAgentReconnectWithoutAJobReclaimer: nil is a quiet default, not a panic.
func TestAgentReconnectWithoutAJobReclaimer(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())

	coord.AgentReconnected(context.Background(), s.hostID)
}
