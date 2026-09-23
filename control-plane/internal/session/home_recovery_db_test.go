package session

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

type homeRecoveryEpoch struct{ commands []agentws.SessionStopCmd }

func (*homeRecoveryEpoch) SupportsHomeCleanup() bool { return true }
func (e *homeRecoveryEpoch) Send(v any) (bool, error) {
	if cmd, ok := v.(agentws.SessionStopCmd); ok {
		e.commands = append(e.commands, cmd)
	}
	return true, nil
}
func (*homeRecoveryEpoch) SendWithAck(context.Context, string, any) (agentws.AckResult, bool, error) {
	return agentws.AckResult{OK: true}, true, nil
}

func TestHeldHomeRecoveryRetriesAfterSyntheticReapUntilQualifiedTerminal(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	var ref string
	must(t, pool.QueryRow(ctx, `SELECT ref FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`, s.userID, appID).Scan(&ref))
	_, err = store.BindManagedHomeDispatchWithHold(ctx, sess.ID,
		[]byte(`{"mounts":["`+ref+`:/home/quasar:rw"]}`), true)
	must(t, err)
	t.Cleanup(func() {
		_, cleanupErr := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
			pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID)
		if cleanupErr != nil {
			t.Errorf("clear test hold: %v", cleanupErr)
		}
	})
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	epoch := &homeRecoveryEpoch{}
	coord.ReconcilePendingHomes(ctx, s.hostID, epoch)
	if len(epoch.commands) != 0 {
		t.Fatalf("scan stopped live assigned session: %+v", epoch.commands)
	}
	_, err = store.Transition(ctx, sess.ID, StateFailed, nil, nil)
	must(t, err)
	coord.ReconcilePendingHomes(ctx, s.hostID, epoch)
	coord.ReconcilePendingHomes(ctx, s.hostID, epoch)
	if len(epoch.commands) != 2 || epoch.commands[0].SessionID != sess.ID || epoch.commands[1].SessionID != sess.ID {
		t.Fatalf("terminal held session retries = %+v", epoch.commands)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "stopped", HomeCleanupQualified: true,
	})
	coord.ReconcilePendingHomes(ctx, s.hostID, epoch)
	if len(epoch.commands) != 2 {
		t.Fatalf("qualified terminal did not stop retry: %+v", epoch.commands)
	}
	got, err := store.Get(ctx, sess.ID)
	must(t, err)
	if got.State != StateFailed {
		t.Fatalf("late cleanup changed public session state: %s", got.State)
	}
}
