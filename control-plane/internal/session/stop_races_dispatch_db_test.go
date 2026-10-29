package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
)

// A stop that takes the row before the dispatch sends anything ends the
// session as stopped: no failure audit, Error log or failure detail, and
// nothing reaches the agent. The stop is made two ways: an operator stop that
// awaits the ack, and the parked supersede that does not. It lands either before
// prepareAndDispatch's state check or between that check and the home bind. An
// agent with the home-cleanup ledger reports `stopped` for the id it never got;
// one without it reports nothing, so the control plane ends the row (#940).
func TestStopRacingDispatchIsNotAFailure(t *testing.T) {
	stops := []struct {
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
	}
	for _, capable := range []bool{true, false} {
		for _, site := range []string{"bind", "state check"} {
			for _, tc := range stops {
				name := fmt.Sprintf("ledger=%v/%s/%s", capable, site, tc.name)
				t.Run(name, func(t *testing.T) {
					stopRacingDispatch(t, capable, site, tc.stop)
				})
			}
		}
	}
}

func stopRacingDispatch(t *testing.T, capable bool, site string, stop func(*testing.T, *Coordinator, string)) {
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
	d := &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{{capable: capable, queued: true, result: agentws.AckResult{OK: true}}}}
	coord := newTestCoordinator(t, store, d, slog.New(slog.NewTextHandler(&logs, nil)), WithAuditor(audit.NewStore(pool)))
	stop(t, coord, sess.ID)
	logs.Reset()

	spec := []byte(`{"mounts":["` + ref + `:/home/quasar:rw"]}`)
	switch site {
	case "bind":
		assign := agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: sess.ID}
		if coord.sendHomeBoundAssign(s.hostID, sess.ID, spec, assign, &expected) {
			t.Fatal("a stopped row was dispatched")
		}
	case "state check":
		coord.prepareAndDispatch(sess, spec, "", &expected)
	}
	failures := func() (n int) {
		for _, row := range sessionAuditRows(t, pool) {
			if row.Action == "session.failed" && row.Target == sess.ID {
				n++
			}
		}
		return n
	}

	want := StateStopping
	if !capable {
		want = StateStopped
	}
	got := readRow(t, pool, sess.ID)
	if got.State != string(want) || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("row %+v, want %s / %s", got, want, StopReasonParkedSuperseded)
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

	// A ledger agent's unknown-id path answers the stop with `stopped`; on a row
	// already ended the same report changes nothing.
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "stopped"})
	got = readRow(t, pool, sess.ID)
	if got.State != string(StateStopped) || got.StopReason != StopReasonParkedSuperseded {
		t.Errorf("after the agent's report: row %+v, want stopped / %s", got, StopReasonParkedSuperseded)
	}
	if failures() != 0 {
		t.Errorf("session.failed audited after the agent's stopped report")
	}
}

// On an agent without the ledger, only an abandoned dispatch ends a stopped row:
// once the assign is out a start may follow, so the row waits for the agent. A
// late start for a row the control plane already ended does not revive it, and
// the agent is told to stop the runtime it lists.
func TestUnsentStopOnALedgerlessAgentAndALateStart(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	app, err := store.GetLaunchApp(ctx, appID)
	must(t, err)
	expected := expectedHomeDispatch(app)
	var ref string
	must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`, s.userID, appID).Scan(&ref))
	spec := []byte(`{"mounts":["` + ref + `:/home/quasar:rw"]}`)
	d := &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{{capable: false, queued: true, result: agentws.AckResult{OK: true}}}}
	coord := newTestCoordinator(t, store, d, testLogger(), WithAuditor(audit.NewStore(pool)))
	launch := func() Session {
		p := managedLaunchParams(s, appID)
		p.PinHostID = s.hostID
		sess, err := store.ScheduleAndCreate(ctx, p)
		must(t, err)
		return sess
	}
	assign := func(id string) bool {
		cmd := agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: id}
		return coord.sendHomeBoundAssign(s.hostID, id, spec, cmd, &expected)
	}

	inFlight := launch()
	if !assign(inFlight.ID) {
		t.Fatal("assign not sent")
	}
	if _, err := coord.Stop(ctx, inFlight.ID, stopReasonUserRequested); err != nil {
		t.Fatalf("stop: %v", err)
	}
	coord.AgentHeartbeat(ctx, s.hostID, []string{})
	if got := readRow(t, pool, inFlight.ID); got.State != string(StateStopping) {
		t.Errorf("stop after the assign went out: row %+v, want stopping until the agent answers", got)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: inFlight.ID, State: "stopped"})

	unsent := launch()
	if _, err := coord.Stop(ctx, unsent.ID, stopReasonUserRequested); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if assign(unsent.ID) {
		t.Fatal("a stopped row was dispatched")
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: unsent.ID, State: "running"})
	coord.AgentHeartbeat(ctx, s.hostID, []string{unsent.ID})
	if got := readRow(t, pool, unsent.ID); got.State != string(StateStopped) || got.StopReason != stopReasonUserRequested {
		t.Errorf("after a late start: row %+v, want stopped / %s", got, stopReasonUserRequested)
	}
	if r := d.stopReason(unsent.ID); r != "error" {
		t.Errorf("heartbeat stop for the late runtime: reason %q, want error", r)
	}
	for _, row := range sessionAuditRows(t, pool) {
		if row.Action == "session.failed" && (row.Target == inFlight.ID || row.Target == unsent.ID) {
			t.Errorf("session.failed audited for %s", row.Target)
		}
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
