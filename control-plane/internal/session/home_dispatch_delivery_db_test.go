package session

import (
	"context"
	"errors"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

type scriptedHomeEpoch struct {
	capable bool
	result  agentws.AckResult
	queued  bool
	err     error
	sends   int
}

func (e *scriptedHomeEpoch) SupportsHomeCleanup() bool { return e.capable }
func (*scriptedHomeEpoch) Send(any) (bool, error)      { return true, nil }
func (e *scriptedHomeEpoch) SendWithAck(context.Context, string, any) (agentws.AckResult, bool, error) {
	e.sends++
	return e.result, e.queued, e.err
}

type homeEpochDispatcher struct {
	*fakeDispatcher
	epochs []*scriptedHomeEpoch
	reads  int
}

func (d *homeEpochDispatcher) CurrentHomeCommandEpoch(string) (agentws.HomeCommandEpoch, bool) {
	i := d.reads
	d.reads++
	if i >= len(d.epochs) {
		i = len(d.epochs) - 1
	}
	return d.epochs[i], true
}

func TestManagedHomeAssignDeliveryKeepsOnlyUncertainHold(t *testing.T) {
	for _, tc := range []struct {
		name      string
		epochs    []*scriptedHomeEpoch
		wantHeld  bool
		wantSends []int
	}{
		{"queued ack lost", []*scriptedHomeEpoch{{capable: true, queued: true, err: context.DeadlineExceeded}}, true, []int{1}},
		{"explicit rejection", []*scriptedHomeEpoch{{capable: true, queued: true, result: agentws.AckResult{OK: false}}}, false, []int{1}},
		{"epoch replaced before queue", []*scriptedHomeEpoch{
			{capable: true, err: agentws.ErrAgentNotConnected},
			{capable: true, queued: true, result: agentws.AckResult{OK: true}},
		}, true, []int{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			s := seed(t, pool, 2)
			appID := seedManagedApp(t, pool, `{}`)
			seedHome(t, pool, s.userID, appID, s.hostID)
			t.Cleanup(func() {
				_, cleanupErr := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
					pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
					WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID)
				if cleanupErr != nil {
					t.Errorf("clear test hold: %v", cleanupErr)
				}
			})
			store := NewStore(pool)
			ctx := context.Background()
			p := managedLaunchParams(s, appID)
			p.PinHostID = s.hostID
			sess, err := store.ScheduleAndCreate(ctx, p)
			must(t, err)
			var ref string
			must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`, s.userID, appID).Scan(&ref))
			d := &homeEpochDispatcher{fakeDispatcher: newFakeDispatcher(true), epochs: tc.epochs}
			coord := newTestCoordinator(t, store, d, testLogger())
			ok := coord.sendHomeBoundAssign(s.hostID, sess.ID, []byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), agentws.SessionAssignCmd{Type: "session_assign", ID: "test-command", SessionID: sess.ID})
			if ok != (tc.name == "epoch replaced before queue") {
				t.Fatalf("assign accepted = %t", ok)
			}
			var held bool
			must(t, pool.QueryRow(ctx, `SELECT pending_home_token IS NOT NULL FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&held))
			if held != tc.wantHeld {
				t.Fatalf("hold after delivery outcome = %t, want %t", held, tc.wantHeld)
			}
			for i, want := range tc.wantSends {
				if tc.epochs[i].sends != want {
					t.Fatalf("epoch %d sends = %d, want %d", i, tc.epochs[i].sends, want)
				}
			}
			if tc.name == "queued ack lost" {
				_, err = store.ScheduleAndCreate(ctx, p)
				if !errors.Is(err, ErrHomeConflict) {
					t.Fatalf("lost ack allowed second launch: %v", err)
				}
			}
		})
	}
}
