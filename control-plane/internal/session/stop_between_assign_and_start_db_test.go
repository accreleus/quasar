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

// stopInFlightDispatcher stops the session from inside the command whose ack it
// is answering, which is the real order: the stop lands while the control plane
// is waiting on that ack, and the agent drops the pending assignment when it
// arrives. Every later command is then refused as that agent does (#1451).
type stopInFlightDispatcher struct {
	*fakeDispatcher
	after  string // "assign" or "start": the ack the stop follows
	stop   func(sessionID string)
	starts int
}

func (d *stopInFlightDispatcher) SendWithAck(ctx context.Context, hostID, id string, v any) (agentws.AckResult, error) {
	switch c := v.(type) {
	case agentws.SessionStartCmd:
		d.starts++
		if d.after == "start" {
			d.stop(c.SessionID)
			return agentws.AckResult{OK: false, Error: "no assignment for session " + c.SessionID}, nil
		}
	case agentws.SessionAssignCmd:
		res, err := d.fakeDispatcher.SendWithAck(ctx, hostID, id, v)
		if d.after == "assign" {
			d.stop(c.SessionID)
			d.fakeDispatcher.mu.Lock()
			d.fakeDispatcher.ackOK = false
			d.fakeDispatcher.ackErr = "no assignment for session " + c.SessionID
			d.fakeDispatcher.mu.Unlock()
		}
		return res, err
	}
	return d.fakeDispatcher.SendWithAck(ctx, hostID, id, v)
}

// #1451: a stop that arrives between the assign ack and the start must not be
// reported as a failed launch. The agent drops the pending-only assignment, so
// a start sent now is refused with "no assignment for session"; the row belongs
// to the stop and no session.failed may be audited. The stop lands on either
// side of the start: after the assign ack (nothing is started) and during the
// start's own ack (the refusal is the stop's).
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
			d := &stopInFlightDispatcher{fakeDispatcher: newFakeDispatcher(true), after: after}
			coord := newTestCoordinator(t, store, d, slog.New(slog.NewTextHandler(&logs, nil)), WithAuditor(audit.NewStore(pool)))
			d.stop = func(id string) {
				if _, err := coord.Stop(ctx, id, stopReasonUserRequested); err != nil {
					t.Errorf("stop: %v", err)
				}
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
			if got := readRow(t, pool, sess.ID); got.State != string(StateStopping) || got.StopReason != stopReasonUserRequested {
				t.Errorf("row %+v, want stopping / %s", got, stopReasonUserRequested)
			}
			if strings.Contains(logs.String(), "level=ERROR") {
				t.Errorf("a stop logged an error:\n%s", logs.String())
			}

			coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "stopped"})
			if got := readRow(t, pool, sess.ID); got.State != string(StateStopped) || got.StopReason != stopReasonUserRequested {
				t.Errorf("after the agent's stopped report: row %+v, want stopped / %s", got, stopReasonUserRequested)
			}
			for _, row := range sessionAuditRows(t, pool) {
				if row.Action == "session.failed" && row.Target == sess.ID {
					t.Errorf("session.failed audited for a stop")
				}
			}
		})
	}
}
