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

func TestCapableSwapTargetHoldOutlivesSyntheticTerminal(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	ctx := context.Background()
	app, err := store.GetLaunchApp(ctx, target)
	must(t, err)
	guard, err := store.GuardHomeForSwapWithHold(ctx, sess.ID, s.userID, app, s.hostID, true, "")
	must(t, err)
	hold := guard.hold
	if hold == nil || !hold.Created {
		t.Fatalf("capable target hold = %+v", hold)
	}
	t.Cleanup(func() { _ = store.ClearNewHomeHold(context.Background(), hold) })
	var homeID string
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM user_homes WHERE user_id=$1::uuid AND app_id=$2::uuid`,
		s.userID, target).Scan(&homeID))
	_, err = store.Transition(ctx, sess.ID, StateFailed, nil, nil)
	must(t, err)
	mgr := storage.NewLocal(pool, testHomeRoot)
	if _, err := mgr.TombstoneHome(ctx, homeID); !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("synthetic terminal exposed target: %v", err)
	}
	_, err = store.ScheduleAndCreate(ctx, managedLaunchParams(s, target))
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("new launch reused held target: %v", err)
	}
}

func TestUnqueuedSwapEpochRefreshUsesNewCapability(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	ctx := context.Background()
	app, err := store.GetLaunchApp(ctx, target)
	must(t, err)
	guard, err := store.GuardHomeForSwapWithHold(ctx, sess.ID, s.userID, app, s.hostID, true, "")
	must(t, err)
	hold := guard.hold
	if hold == nil || !hold.Created {
		t.Fatalf("first epoch hold = %+v", hold)
	}
	decision, err := store.RefreshSwapHomeHold(ctx, sess.ID, s.userID, target, s.hostID, hold, false, nil)
	must(t, err)
	if decision != nil {
		t.Fatalf("unsupported replacement epoch kept new hold: %+v", decision)
	}
	var pending, warning bool
	must(t, pool.QueryRow(ctx, `SELECT pending_home_token IS NOT NULL,legacy_unprotected_dispatch
		FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, target).
		Scan(&pending, &warning))
	if pending || !warning {
		t.Fatalf("epoch refresh: pending=%t warning=%t", pending, warning)
	}
	decision, err = store.RefreshSwapHomeHold(ctx, sess.ID, s.userID, target, s.hostID, nil, true, nil)
	must(t, err)
	if decision == nil || !decision.Created {
		t.Fatalf("later capable epoch hold = %+v", decision)
	}
	must(t, store.ClearNewHomeHold(ctx, decision))
}

// A capability downgrade before handoff releases the Steam hold this swap took
// as well as its own, and records the unprotected dispatch on both claims: the
// frame has not left the control plane, so nothing may stay held for it.
func TestUnqueuedSwapEpochRefreshReleasesTheSharedHold(t *testing.T) {
	f := newWarmFixture(t, 8)
	desktop := seedDesktop(t, f, true)
	mine := seedDesktop(t, f, false)
	sess := f.startedLaunch(t, mine)
	ctx := context.Background()

	app, err := f.store.GetLaunchApp(ctx, desktop)
	must(t, err)
	guard, err := f.store.GuardHomeForSwapWithHold(ctx, sess, f.userID, app, f.hostID, true, f.steam)
	must(t, err)
	if guard.hold == nil || !guard.hold.Created || guard.sharedHold == nil || !guard.sharedHold.Created {
		t.Fatalf("guard = %+v, want a new own hold and a new shared hold", guard)
	}
	if h, _ := f.claimHolder(t, f.steam); h != sess {
		t.Fatalf("Steam hold = %q, want the swap's own %s", h, sess)
	}

	decision, err := f.store.RefreshSwapHomeHold(ctx, sess, f.userID, desktop, f.hostID,
		guard.hold, false, guard.sharedHold)
	must(t, err)
	if decision != nil {
		t.Fatalf("unsupported replacement epoch kept new hold: %+v", decision)
	}
	for _, appID := range []string{desktop, f.steam} {
		var pending, warning bool
		must(t, f.pool.QueryRow(ctx, `SELECT pending_home_token IS NOT NULL,legacy_unprotected_dispatch
			FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, f.userID, appID).
			Scan(&pending, &warning))
		if pending || !warning {
			t.Errorf("app %s after the epoch refresh: pending=%t warning=%t, want released and recorded", appID, pending, warning)
		}
	}
}

// A capability change that would dispatch a sharing swap WITHOUT its Steam hold
// is refused: an earlier downgrade released that hold, and this refresh re-takes
// only the target's own, so the retry must re-enter the swap guard afresh. An
// unsupported epoch from the start is the same case — the guard took no holder
// at all, and the Steam identity is what the refresh still has to reason about.
func TestSwapEpochUpgradeWithoutTheSharedHoldIsRejected(t *testing.T) {
	for _, tc := range []struct {
		name         string
		guardCapable bool
		downgrade    bool
	}{
		{"downgraded then upgraded", true, true},
		{"unsupported epoch then capable", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWarmFixture(t, 8)
			desktop := seedDesktop(t, f, true)
			mine := seedDesktop(t, f, false)
			sess := f.startedLaunch(t, mine)
			ctx := context.Background()

			app, err := f.store.GetLaunchApp(ctx, desktop)
			must(t, err)
			guard, err := f.store.GuardHomeForSwapWithHold(ctx, sess, f.userID, app, f.hostID,
				tc.guardCapable, f.steam)
			must(t, err)
			if guard.sharedHold == nil || guard.sharedHold.AppID != f.steam {
				t.Fatalf("guard = %+v, want the swap's Steam identity recorded", guard)
			}

			prior := guard.hold
			if tc.downgrade {
				prior, err = f.store.RefreshSwapHomeHold(ctx, sess, f.userID, desktop, f.hostID,
					guard.hold, false, guard.sharedHold)
				must(t, err)
				if prior != nil {
					t.Fatalf("downgrade kept a new hold: %+v", prior)
				}
			}

			if _, err := f.store.RefreshSwapHomeHold(ctx, sess, f.userID, desktop, f.hostID,
				prior, true, guard.sharedHold); !errors.Is(err, errSwapSharedHoldLost) {
				t.Fatalf("upgrade to a capable epoch = %v, want the swap rejected", err)
			}
			// Nothing from that swap is left held by the session.
			for _, appID := range []string{desktop, f.steam} {
				var pending *string
				must(t, f.pool.QueryRow(ctx, `SELECT pending_home_session_id::text FROM managed_home_claims
					WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, f.userID, appID).Scan(&pending))
				if pending != nil {
					t.Errorf("app %s still held by %s after the refused upgrade", appID, *pending)
				}
			}
		})
	}
}

// A stop request can race the pending swap's agent callback. The target may
// still be mounted until teardown reaches a terminal state, even though the
// session's app_id still names the old app.
func TestStopDuringPendingManagedSwapKeepsTargetHomeProtected(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := seedManagedApp(t, pool, `{"image":"target:1"}`)
	seedHome(t, pool, s.userID, target, s.hostID)
	var homeID string
	ctx := context.Background()
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM user_homes
		WHERE user_id=$1::uuid AND app_id=$2::uuid AND host_id=$3::uuid`, s.userID, target, s.hostID).Scan(&homeID))
	app, err := store.GetLaunchApp(ctx, target)
	must(t, err)
	must(t, store.GuardHomeForSwap(ctx, sess.ID, s.userID, app, s.hostID))
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	stopping, err := coord.Stop(ctx, sess.ID, "user_requested")
	must(t, err)
	if stopping.State != StateStopping || stopping.StateDetail == nil || *stopping.StateDetail != swapDetailInProgress {
		t.Fatalf("stop erased durable target guard: state=%s detail=%v", stopping.State, stopping.StateDetail)
	}
	mgr := storage.NewLocal(pool, testHomeRoot)
	if _, err := mgr.TombstoneHome(ctx, homeID); !errors.Is(err, storage.ErrHomeInUse) {
		t.Fatalf("stopping swap target tombstone = %v, want home in use", err)
	}
	_, err = pool.Exec(ctx, `UPDATE user_homes SET gc_after=now()-interval '25 hours' WHERE id=$1::uuid`, homeID)
	must(t, err)
	pending, err := mgr.GCPending(ctx, s.hostID)
	must(t, err)
	if len(pending) != 0 {
		t.Fatalf("stopping swap target offered to GC: %+v", pending)
	}
	deleted, err := mgr.GCConfirm(ctx, s.hostID, []string{homeID})
	must(t, err)
	if deleted != 0 {
		t.Fatalf("stopping swap target GC confirmed %d homes", deleted)
	}
	coord.AgentState(ctx, s.hostID, agentws.SessionStateMsg{SessionID: sess.ID, State: "stopped"})
	deleted, err = mgr.GCConfirm(ctx, s.hostID, []string{homeID})
	must(t, err)
	if deleted != 1 {
		t.Fatalf("terminal callback did not release GC hold: deleted=%d", deleted)
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
	swap := newSwapper(store, disp, testLogger(), nil, nil)
	swap.pendingSwaps[sess.ID] = target
	swap.dispatchSwap(s.hostID, sess.ID, []byte(`{}`), true, false, s.userID, target, nil, nil)
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
			WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%managed_home_claims%' AND query LIKE '%FOR UPDATE%'`).Scan(&waiting))
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
