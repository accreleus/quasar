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

// #1451: a stop that arrives between the assign ack and the start must not be
// reported as a failed launch. The agent drops the pending-only assignment, so
// a start sent now is refused with "no assignment for session"; the row belongs
// to the stop and no session.failed may be audited. The stop lands on either
// side of the start: after the assign ack (nothing is started) and during the
// start's own ack (the refusal is the stop's).
//
// The agent here registered without the home-cleanup ledger, so it reports
// nothing for the dropped assignment: the dispatch must end the row itself, or
// it keeps its encode slot and its home for the life of the connection (#940).
func TestStopBetweenAssignAndStartIsNotAFailure(t *testing.T) {
	for _, after := range []string{"assign", "start"} {
		t.Run(after, func(t *testing.T) {
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
			epoch := &scriptedHomeEpoch{capable: false, queued: true, result: agentws.AckResult{OK: true}}
			d := &stopInFlightDispatcher{
				homeEpochDispatcher: &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: []*scriptedHomeEpoch{epoch}},
				after:               after,
			}
			coord := newTestCoordinator(t, store, d, slog.New(slog.NewTextHandler(&logs, nil)), WithAuditor(audit.NewStore(pool)))
			stopped := func() {
				if _, err := coord.Stop(ctx, sess.ID, stopReasonUserRequested); err != nil {
					t.Errorf("stop: %v", err)
				}
			}
			if after == "assign" {
				epoch.after = stopped
			} else {
				d.stop = func(string) { stopped() }
			}
			logs.Reset()

			coord.dispatchAssignStartWithTopology(sess, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), "stream_only", &expected)

			wantStarts := 0
			if after == "start" {
				wantStarts = 1
			}
			if d.starts != wantStarts {
				t.Errorf("start commands sent = %d, want %d", d.starts, wantStarts)
			}
			got := readRow(t, pool, sess.ID)
			if got.State != string(StateStopped) || got.StopReason != stopReasonUserRequested || strings.Contains(got.Detail, "failed") {
				t.Errorf("row %+v, want stopped / %s with no failure detail", got, stopReasonUserRequested)
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
			if strings.Contains(logs.String(), "level=ERROR") {
				t.Errorf("a stop logged an error:\n%s", logs.String())
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
