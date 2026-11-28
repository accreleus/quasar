package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
)

// reapPaths is every way the control plane ends a host's sessions itself.
var reapPaths = []struct {
	name string
	// A disconnect or reconnect spares `running`, so there the lost row is in flight.
	lost   func(*testing.T, *Store, seedIDs) Session
	reap   func(context.Context, *Coordinator, seedIDs)
	audits int // session.failed rows for the lost session
}{
	{"heartbeat", runningSession, func(ctx context.Context, c *Coordinator, s seedIDs) {
		c.AgentHeartbeat(ctx, s.hostID, []string{})
	}, 1},
	{"disconnect", inFlightSession, func(ctx context.Context, c *Coordinator, s seedIDs) {
		c.HostDisconnected(ctx, s.hostID)
	}, 0},
	{"reconnect", inFlightSession, func(ctx context.Context, c *Coordinator, s seedIDs) {
		c.AgentReconnected(ctx, s.hostID)
	}, 0},
	{"stale sweep", runningSession, func(ctx context.Context, c *Coordinator, s seedIDs) {
		c.sweepStaleHosts(ctx, time.Now().Add(-time.Hour), time.Minute)
	}, 1},
}

// On every reap path a `stopping` row ends as its stop would, never host_lost;
// a session nobody stopped is still host_lost.
func TestReapNeverReportsAStoppingSessionAsHostLost(t *testing.T) {
	for _, path := range reapPaths {
		t.Run(path.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool)
			s := seed(t, pool, 4)
			setQuota(t, pool, s.userID, 10)
			forgotten := &recordingForgetter{}
			coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
				WithAgentConnectivity(fakeAgents(false)), WithSessionForgetter(forgotten),
				WithAuditor(audit.NewStore(pool)))
			ctx := context.Background()

			lost := path.lost(t, store, s)
			left := runningSession(t, store, s) // the user pressed Leave
			_, err := coord.Stop(ctx, left.ID, stopReasonUserRequested)
			must(t, err)
			exited := runningSession(t, store, s) // the agent's own teardown: no stop_reason
			coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: exited.ID, State: "stopping", Detail: "tearing down"})
			mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))

			path.reap(ctx, coord, s)

			for id, detail := range map[string]string{left.ID: "stop requested", exited.ID: "tearing down"} {
				got, err := store.Get(ctx, id)
				must(t, err)
				if got.State != StateStopped || deref(got.StateDetail) != detail || got.ErrorMessage != nil || got.EndedAt == nil {
					t.Errorf("stopping row = %s, detail %q, error %q, ended %v; want stopped, %q, no error",
						got.State, deref(got.StateDetail), deref(got.ErrorMessage), got.EndedAt, detail)
				}
			}
			got, err := store.Get(ctx, lost.ID)
			must(t, err)
			if got.State != StateFailed || deref(got.StateDetail) != "host_lost" || got.ErrorMessage == nil || got.EndedAt == nil {
				t.Errorf("lost row = %s, detail %q, error %q; want failed, host_lost", got.State, deref(got.StateDetail), deref(got.ErrorMessage))
			}
			if slots := reservedSlots(t, pool, s.gpuID); slots != 0 {
				t.Errorf("%d slots still reserved", slots)
			}
			failures := map[string]int{}
			for _, row := range sessionAuditRows(t, pool) {
				if row.Action == "session.failed" {
					failures[row.Target]++
				}
			}
			if failures[left.ID] != 0 || failures[exited.ID] != 0 || failures[lost.ID] != path.audits {
				t.Errorf("session.failed rows: left %d, exited %d, lost %d; want 0, 0, %d",
					failures[left.ID], failures[exited.ID], failures[lost.ID], path.audits)
			}
			ended := map[string]int{}
			for _, id := range forgotten.seen() {
				ended[id]++
			}
			for _, id := range []string{lost.ID, left.ID, exited.ID} {
				if ended[id] != 1 {
					t.Errorf("session %s: %d terminal notifications, want 1", id, ended[id])
				}
			}
		})
	}
}

// One row Go cannot decode (created_at = infinity) must not keep its siblings,
// and their reservations, from being reaped.
func TestUnreadableRowDoesNotBlockTheReap(t *testing.T) {
	for _, path := range reapPaths {
		t.Run(path.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool)
			s := seed(t, pool, 4)
			setQuota(t, pool, s.userID, 10)
			coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger(),
				WithAgentConnectivity(fakeAgents(false)))
			ctx := context.Background()

			bad := path.lost(t, store, s)
			healthy := path.lost(t, store, s)
			must(t, exec(t, pool, `UPDATE sessions SET created_at = 'infinity'::timestamptz WHERE id::text = $1`, bad.ID))
			mustSetHeartbeat(t, pool, s.hostID, time.Now().Add(-10*time.Minute))

			path.reap(ctx, coord, s)

			for _, id := range []string{bad.ID, healthy.ID} {
				var state string
				must(t, pool.QueryRow(ctx, `SELECT state FROM sessions WHERE id::text = $1`, id).Scan(&state))
				if state != string(StateFailed) {
					t.Errorf("session %s = %s, want failed", id, state)
				}
			}
			if slots := reservedSlots(t, pool, s.gpuID); slots != 0 {
				t.Errorf("%d slots still reserved", slots)
			}
		})
	}
}

// The socket close wakes pending command waiters before the disconnect reap
// runs, and a DELETE may have committed `stopping` without its stop grace yet.
// Whichever of the two gets the row, the stop is not a failure.
func TestStopRacingADisconnectIsNeverAFailure(t *testing.T) {
	for _, order := range []string{"reap first", "waiter first"} {
		t.Run(order, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool)
			s := seed(t, pool, 4)
			var logs bytes.Buffer
			forgotten := &recordingForgetter{}
			coord := newTestCoordinator(t, store, newFakeDispatcher(true), slog.New(slog.NewTextHandler(&logs, nil)),
				WithSessionForgetter(forgotten), WithAuditor(audit.NewStore(pool)))
			ctx := context.Background()
			sess := inFlightSession(t, store, s)
			// coordinator.stop's committed write, before noteStopRequested.
			_, err := store.transition(ctx, sess.ID, "", nil, StateStopping, strptr("stop requested"), nil, strptr(stopReasonUserRequested))
			must(t, err)

			wake := func() { coord.failStart(sess, s.hostID, "session_start dispatch failed: agent disconnected") }
			reap := func() { coord.HostDisconnected(ctx, s.hostID) }
			if order == "waiter first" {
				wake()
				reap()
			} else {
				reap()
				wake()
			}

			got, err := store.Get(ctx, sess.ID)
			must(t, err)
			if got.State != StateStopped || got.ErrorMessage != nil || deref(got.StateDetail) == "host_lost" {
				t.Errorf("row = %s, detail %q, error %q; want stopped, no error", got.State, deref(got.StateDetail), deref(got.ErrorMessage))
			}
			for _, row := range sessionAuditRows(t, pool) {
				if row.Action == "session.failed" && row.Target == sess.ID {
					t.Errorf("stop audited as a failure: %+v", row.Details)
				}
			}
			if strings.Contains(logs.String(), `msg="session failed"`) {
				t.Errorf("stop logged as a failure:\n%s", logs.String())
			}
			if ids := forgotten.seen(); len(ids) != 1 || ids[0] != sess.ID {
				t.Errorf("terminal notifications = %v, want one", ids)
			}
		})
	}
}
