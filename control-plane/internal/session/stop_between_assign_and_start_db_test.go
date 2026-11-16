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

// stopInFlightDispatcher stops the session from inside the command whose ack it
// is answering, which is the real order: the stop lands while the control plane
// is waiting on that ack, and the agent drops the pending assignment when it
// arrives. Every later command is then refused as that agent does (#1451).
// The epoch is scripted not to support home cleanup, which is the agent that
// cannot report a stopped row for itself (the one #940's endUnsentStop is for).
type stopInFlightDispatcher struct {
	*homeEpochDispatcher
	after  string // "assign" or "start": the ack the stop follows
	stop   func(sessionID string)
	starts int
}

func (d *stopInFlightDispatcher) SendWithAck(ctx context.Context, hostID, id string, v any) (agentws.AckResult, error) {
	if c, ok := v.(agentws.SessionStartCmd); ok {
		d.starts++
		if d.after == "start" {
			d.stop(c.SessionID)
			return agentws.AckResult{OK: false, Error: "no assignment for session " + c.SessionID}, nil
		}
	}
	return d.homeEpochDispatcher.fakeDispatcher.SendWithAck(ctx, hostID, id, v)
}

// #1451, amendment 88: any command the agent refuses while a stop of that
// session is in its grace ends the session as the stop, never as a failure.
// The refusal is reached three ways here: an assign the agent turns down because
// the stop already retired the id ("session id already recorded"), the same
// refusal for the launch's own reason, and a start refused after a stop that
// landed between the assign ack and the start.
//
// Two agents are covered. One registered without the home-cleanup ledger, so it
// reports nothing for the id: the dispatch must end the row itself, or it keeps
// its encode slot and its home for the life of the connection (#940). One with
// the ledger reports `stopped` itself, and the control plane must stay out of
// its way (endUnsentStop is a no-op there).
func TestStopBetweenAssignAndStartIsNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopAt  string // which ack the stop follows
		refused bool   // the agent refuses the assign instead of acking it
		capable bool   // the agent holds the home-cleanup ledger
	}{
		{name: "assign ack", stopAt: "assign"},
		{name: "assign refused", stopAt: "assign", refused: true},
		{name: "start ack", stopAt: "start"},
		{name: "assign ack, ledger agent", stopAt: "assign", capable: true},
		{name: "assign refused, ledger agent", stopAt: "assign", refused: true, capable: true},
		{name: "start ack, ledger agent", stopAt: "start", capable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			s := seed(t, pool, 2)
			appID := seedManagedApp(t, pool, `{}`)
			seedHome(t, pool, s.userID, appID, s.hostID)
			// A launch abandoned before the agent's cleanup proof leaves its
			// pending home hold behind by design; the next test's truncate refuses
			// a pending operation, so drop it here.
			t.Cleanup(func() {
				if _, err := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
					pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
					WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID); err != nil {
					t.Errorf("clear test hold: %v", err)
				}
			})
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
			epoch := &scriptedHomeEpoch{capable: tc.capable, queued: true, result: agentws.AckResult{OK: true}}
			if tc.refused {
				epoch.result = agentws.AckResult{Error: "session id already recorded"}
			}
			d := &stopInFlightDispatcher{
				homeEpochDispatcher: &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{epoch}},
				after:               tc.stopAt,
			}
			coord := newTestCoordinator(t, store, d, slog.New(slog.NewTextHandler(&logs, nil)), WithAuditor(audit.NewStore(pool)))
			stopped := func() {
				if _, err := coord.Stop(ctx, sess.ID, stopReasonUserRequested); err != nil {
					t.Errorf("stop: %v", err)
				}
			}
			if tc.stopAt == "start" {
				d.stop = func(string) { stopped() }
			} else {
				epoch.after = stopped
			}
			logs.Reset()

			coord.dispatchAssignStartWithTopology(sess, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), "stream_only", &expected)

			wantStarts := 0
			if tc.stopAt == "start" {
				wantStarts = 1
			}
			if d.starts != wantStarts {
				t.Errorf("start commands sent = %d, want %d", d.starts, wantStarts)
			}
			got := readRow(t, pool, sess.ID)
			if tc.capable {
				// The ledger agent reports the stopped row itself; the control
				// plane must not have ended it under the agent's feet.
				if got.State != string(StateStopping) {
					t.Errorf("ledger agent: row %+v before its report, want stopping", got)
				}
				coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "stopped"})
				got = readRow(t, pool, sess.ID)
			}
			if got.State != string(StateStopped) || got.StopReason != stopReasonUserRequested || strings.Contains(got.Detail, "failed") {
				t.Errorf("row %+v, want stopped / %s with no failure detail", got, stopReasonUserRequested)
			}
			var errorMessage *string
			must(t, pool.QueryRow(ctx, `SELECT error_message FROM sessions WHERE id=$1::uuid`, sess.ID).Scan(&errorMessage))
			if errorMessage != nil {
				t.Errorf("error_message = %q on a stop", *errorMessage)
			}
			var heldSlots int
			must(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_encode_slots),0) FROM sessions
				WHERE host_id=$1::uuid AND state IN ('assigned','starting','running','stopping')`, s.hostID).Scan(&heldSlots))
			if heldSlots != 0 {
				t.Errorf("%d encode slots still held by a stopped launch", heldSlots)
			}
			var liveHome bool
			must(t, pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE user_id=$1::uuid AND app_id=$2::uuid
				AND state NOT IN ('stopped','failed'))`, s.userID, appID).Scan(&liveHome))
			if liveHome {
				t.Errorf("a stopped launch still holds its home")
			}
			if strings.Contains(logs.String(), "level=ERROR") || strings.Contains(logs.String(), "session failed") {
				t.Errorf("a stop logged a failure:\n%s", logs.String())
			}
			for _, row := range sessionAuditRows(t, pool) {
				if row.Action == "session.failed" && row.Target == sess.ID {
					t.Errorf("session.failed audited for a stop")
				}
			}
		})
	}
}

// The withheld-start and refused-start guards rest on one ordering: the command
// that follows a successful ack is answered inside the grace the stop opens. If
// an ack timeout ever grows past stopAckTimeout, a slow host re-opens #1451
// silently — the start (or its refusal) would land after the grace expired.
func TestCommandAckTimeoutsFitInsideTheStopGrace(t *testing.T) {
	for name, timeout := range map[string]time.Duration{
		"assignAckTimeout": assignAckTimeout,
		"startAckTimeout":  startAckTimeout,
	} {
		if timeout > stopAckTimeout {
			t.Errorf("%s = %s exceeds stopAckTimeout = %s: a command answered after the grace would not be seen as the stop's", name, timeout, stopAckTimeout)
		}
	}
}
