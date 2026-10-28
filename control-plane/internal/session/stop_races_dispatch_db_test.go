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
// detail, and nothing reaches the agent.
func TestStopRacingDispatchIsNotAFailure(t *testing.T) {
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
	if _, err := coord.Stop(ctx, sess.ID, StopReasonParkedSuperseded); err != nil {
		t.Fatalf("stop: %v", err)
	}
	logs.Reset()

	assign := agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: sess.ID}
	if coord.sendHomeBoundAssign(s.hostID, sess.ID, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), assign, &expected) {
		t.Fatal("a stopped row was dispatched")
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
	for _, row := range sessionAuditRows(t, pool) {
		if row.Action == "session.failed" && row.Target == sess.ID {
			t.Errorf("session.failed audited for a stop: %+v", row)
		}
	}
	if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "session failed") {
		t.Errorf("a stop logged as a failure:\n%s", logs.String())
	}
}
