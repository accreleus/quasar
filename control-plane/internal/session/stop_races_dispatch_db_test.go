package session

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
)

// A stop that takes the row between the dispatch's state check and its home
// bind ends the session as stopped: no failure audit, Error log or failure
// detail, and nothing reaches the agent. The stop is made two ways: an
// operator stop that awaits the ack, and the parked supersede that does not.
func TestStopRacingDispatchIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(t *testing.T, coord *Coordinator, id string)
	}{
		{"acked stop", func(t *testing.T, coord *Coordinator, id string) {
			if _, err := coord.Stop(context.Background(), id, StopReasonParkedSuperseded); err != nil {
				t.Fatalf("stop: %v", err)
			}
		}},
		{"parked supersede without ack", func(t *testing.T, coord *Coordinator, id string) {
			must(t, exec(t, coord.store.pool, `UPDATE sessions SET kind = 'parked', parked_until = now() + interval '1 hour' WHERE id = $1::uuid`, id))
			if n, err := coord.stopParked(context.Background(), StopReasonParkedSuperseded, parkedFilter{ID: id}); err != nil || n != 1 {
				t.Fatalf("stop parked: n=%d err=%v", n, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			s := seed(t, pool, 2)
			appID := seedManagedApp(t, pool, `{}`)
			seedHome(t, pool, s.userID, appID, s.hostID)
			store := NewStore(pool)
			ctx := context.Background()
			app, err := store.GetLaunchApp(ctx, appID)
			must(t, err)
			expected := expectedHomeDispatch(app)
			p := managedLaunchParams(s, appID)
			p.PinHostID = s.hostID
			sess, err := store.ScheduleAndCreate(ctx, p)
			must(t, err)
			var ref string
			must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`, s.userID, appID).Scan(&ref))

			var logs bytes.Buffer
			d := &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{{capable: true, queued: true, result: agentws.AckResult{OK: true}}}}
			coord := newTestCoordinator(t, store, d, slog.New(slog.NewTextHandler(&logs, nil)), WithAuditor(audit.NewStore(pool)))
			tc.stop(t, coord, sess.ID)
			logs.Reset()

			assign := agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: sess.ID}
			if coord.sendHomeBoundAssign(s.hostID, sess.ID, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), assign, &expected) {
				t.Fatal("a stopped row was dispatched")
			}
			failures := func() (n int) {
				for _, row := range sessionAuditRows(t, pool) {
					if row.Action == "session.failed" && row.Target == sess.ID {
						n++
					}
				}
				return n
			}

			got := readRow(t, pool, sess.ID)
			if got.State != string(StateStopping) || got.StopReason != StopReasonParkedSuperseded {
				t.Errorf("row %+v, want stopping / %s", got, StopReasonParkedSuperseded)
			}
			if strings.Contains(got.Detail, "failed") {
				t.Errorf("state_detail = %q carries a failure", got.Detail)
			}
			if n := d.epochs[0].sends; n != 0 {
				t.Errorf("%d commands reached the agent", n)
			}
			if failures() != 0 {
				t.Errorf("session.failed audited for a stop")
			}
			if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "session failed") {
				t.Errorf("a stop logged as a failure:\n%s", logs.String())
			}

			// The agent's unknown-id path answers the stop with `stopped`.
			coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "stopped"})
			got = readRow(t, pool, sess.ID)
			if got.State != string(StateStopped) || got.StopReason != StopReasonParkedSuperseded {
				t.Errorf("after the agent's report: row %+v, want stopped / %s", got, StopReasonParkedSuperseded)
			}
			if failures() != 0 {
				t.Errorf("session.failed audited after the agent's stopped report")
			}
		})
	}
}

// Only a row a stop or reap owns is dropped quietly; a row in any other
// non-assigned state is a bug and still fails the session.
func TestDispatchOfAnUnexpectedStateStillFailsLoudly(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	app, err := store.GetLaunchApp(ctx, appID)
	must(t, err)
	expected := expectedHomeDispatch(app)
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	var ref string
	must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`, s.userID, appID).Scan(&ref))
	must(t, exec(t, pool, `UPDATE sessions SET state = 'starting' WHERE id = $1::uuid`, sess.ID))

	d := &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{{capable: true, queued: true, result: agentws.AckResult{OK: true}}}}
	coord := newTestCoordinator(t, store, d, testLogger(), WithAuditor(audit.NewStore(pool)))
	assign := agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: sess.ID}
	if coord.sendHomeBoundAssign(s.hostID, sess.ID, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), assign, &expected) {
		t.Fatal("a starting row was dispatched")
	}
	if got := readRow(t, pool, sess.ID); got.State != string(StateFailed) {
		t.Errorf("row %+v, want failed", got)
	}
	var failures int
	for _, row := range sessionAuditRows(t, pool) {
		if row.Action == "session.failed" && row.Target == sess.ID {
			failures++
		}
	}
	if failures != 1 {
		t.Errorf("session.failed audit rows = %d, want 1", failures)
	}
}
