package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// The target app is absent from sessions.app_id until a swap completes. The
// durable swapping detail must protect its home even after the coordinator's
// in-memory pending target is lost on restart.
func TestPendingManagedSwapProtectsTargetHomeAcrossRestart(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	var homeID string
	must(t, pool.QueryRow(context.Background(), `SELECT id::text FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, target, s.hostID).Scan(&homeID))
	app, err := store.GetLaunchApp(context.Background(), target)
	must(t, err)
	must(t, store.GuardHomeForSwap(context.Background(), sess.ID, s.userID, app, s.hostID))
	mgr := storage.NewLocal(pool, testHomeRoot)
	if _, err := mgr.TombstoneHome(context.Background(), homeID); !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("pending target tombstone = %v, want home in use", err)
	}
	_, err = pool.Exec(context.Background(), `UPDATE user_homes SET gc_after=now()-interval '25 hours' WHERE id=$1::uuid`, homeID)
	must(t, err)
	pending, err := mgr.GCPending(context.Background(), s.hostID)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("pending swap exposed GC work: %+v", pending)
	}
	deleted, err := mgr.GCConfirm(context.Background(), s.hostID, []string{homeID})
	must(t, err)
	if deleted != 0 {
		t.Fatalf("pending swap GC confirmed %d homes", deleted)
	}
	// A new coordinator has no pendingSwaps map. An ordinary running callback
	// still cannot clear the durable protection.
	restarted := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	restarted.AgentState(context.Background(), s.hostID, agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: "app presented"})
	got, err := store.Get(context.Background(), sess.ID)
	must(t, err)
	if got.StateDetail == nil || *got.StateDetail != swapDetailInProgress {
		t.Fatalf("restart callback erased swap guard: %+v", got.StateDetail)
	}
	deleted, err = mgr.GCConfirm(context.Background(), s.hostID, []string{homeID})
	must(t, err)
	if deleted != 0 {
		t.Fatalf("restart callback exposed GC: deleted=%d", deleted)
	}
	_, err = store.Transition(context.Background(), sess.ID, StateStopped, nil, nil)
	must(t, err)
	deleted, err = mgr.GCConfirm(context.Background(), s.hostID, []string{homeID})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("terminal session did not release GC hold: deleted=%d", deleted)
	}
}

func TestUncertainManagedSwapAckRetainsHomeHold(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	var homeID string
	must(t, pool.QueryRow(context.Background(), `SELECT id::text FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, target, s.hostID).Scan(&homeID))
	app, err := store.GetLaunchApp(context.Background(), target)
	must(t, err)
	must(t, store.GuardHomeForSwap(context.Background(), sess.ID, s.userID, app, s.hostID))
	disp := newFakeDispatcher(true)
	disp.ackSendErr = errors.New("ack lost")
	swap := newSwapper(store, disp, testLogger(), nil)
	swap.pendingSwaps[sess.ID] = target
	swap.pendingHome[sess.ID] = true
	swap.dispatchSwap(s.hostID, sess.ID, []byte(`{}`), true)
	if _, pending := swap.pendingSwaps[sess.ID]; !pending {
		t.Fatal("uncertain ack removed pending target")
	}
	mgr := storage.NewLocal(pool, testHomeRoot)
	if _, err := mgr.TombstoneHome(context.Background(), homeID); !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("uncertain ack exposed target home: %v", err)
	}
	// An authenticated rollback for this still-known target can release it.
	swap.handleSwapCallback(context.Background(), agentws.SessionStateMsg{
		SessionID: sess.ID, State: "running", Detail: "swap failed; rolled back: image pull failed"})
	if _, pending := swap.pendingSwaps[sess.ID]; pending {
		t.Fatal("authenticated rollback left pending target")
	}
	if _, err := mgr.TombstoneHome(context.Background(), homeID); err != nil {
		t.Fatalf("authenticated rollback did not release target home: %v", err)
	}
}

func TestManagedSwapAndTombstoneSerializeBeforeTargetDispatch(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	var homeID string
	must(t, pool.QueryRow(context.Background(), `SELECT id::text FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, target, s.hostID).Scan(&homeID))
	_, err := pool.Exec(context.Background(), `INSERT INTO managed_home_claims
		(user_id,canonical_app_id,host_id,state) VALUES ($1::uuid,$2::uuid,$3::uuid,'reserved')`, s.userID, target, s.hostID)
	must(t, err)
	app, err := store.GetLaunchApp(context.Background(), target)
	must(t, err)
	blocker, err := pool.Begin(context.Background())
	must(t, err)
	defer blocker.Rollback(context.Background()) //nolint:errcheck
	var locked string
	must(t, blocker.QueryRow(context.Background(), `SELECT canonical_app_id::text FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, s.userID, target).Scan(&locked))
	guardResult := make(chan error, 1)
	go func() { guardResult <- store.GuardHomeForSwap(context.Background(), sess.ID, s.userID, app, s.hostID) }()
	// Wait until Guard holds the user advisory and session locks and is blocked
	// on the claim. Start the tombstone concurrently, then release the claim;
	// it must see the durable swapping detail and refuse the deletion.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting int
		must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE '%managed_home_claims%' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting))
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("swap never locked its session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	tombstoneResult := make(chan error, 1)
	go func() {
		_, err := storage.NewLocal(pool, testHomeRoot).TombstoneHome(context.Background(), homeID)
		tombstoneResult <- err
	}()
	must(t, blocker.Rollback(context.Background()))
	select {
	case err := <-guardResult:
		must(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("swap claim guard did not finish")
	}
	select {
	case err := <-tombstoneResult:
		if !errors.Is(err, storage.ErrHomeInUse) {
			t.Fatalf("concurrent tombstone = %v, want home in use", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("tombstone did not finish after swap guard")
	}
}
