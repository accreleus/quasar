package session

// #434: a relaunch while the previous session still holds the managed-home
// claim. The hold outlives the session row by design (only the agent's
// qualified terminal clears it), so the launch has to tell a live or settling
// holder from a hold that will never clear on its own.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// holdHomeFor records the claim hold a cleanup-capable dispatch would have set.
func holdHomeFor(t *testing.T, pool *pgxpool.Pool, userID, appID, sessionID string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE managed_home_claims SET
		pending_home_session_id=$3::uuid,pending_home_token=gen_random_uuid(),pending_home_started_at=now()
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, appID, sessionID)
	must(t, err)
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
			pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, appID)
		if err != nil {
			t.Errorf("clear test hold: %v", err)
		}
	})
}

// withHomeHoldSettleWait shortens or lengthens the launch's settle budget for
// one test.
func withHomeHoldSettleWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := homeHoldSettleWait
	homeHoldSettleWait = d
	t.Cleanup(func() { homeHoldSettleWait = prev })
}

func claimState(t *testing.T, pool *pgxpool.Pool, userID, appID string) string {
	t.Helper()
	var state string
	must(t, pool.QueryRow(context.Background(), `SELECT state FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, appID).Scan(&state))
	return state
}

// launchHeldHome launches a first managed-home session, records its hold and
// returns the pieces the tests drive.
func launchHeldHome(t *testing.T, pool *pgxpool.Pool) (*Store, seedIDs, string, CreateParams, Session) {
	t.Helper()
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	first, err := store.ScheduleAndCreate(context.Background(), p)
	must(t, err)
	holdHomeFor(t, pool, s.userID, appID, first.ID)
	return store, s, appID, p, first
}

// The issue's first half: the user pressed stop and relaunched at once. The
// holder is still tearing down, so the answer is the single-writer rule's
// retryable home_in_use naming it, never "needs operator review".
func TestLaunchWhileHolderTearsDownIsHomeInUse(t *testing.T) {
	pool := testDB(t)
	store, s, appID, p, first := launchHeldHome(t, pool)
	ctx := context.Background()
	_, err := store.Transition(ctx, first.ID, StateStopping, strptr("stop requested"), nil)
	must(t, err)

	_, err = store.ScheduleAndCreate(ctx, p)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("relaunch while the holder tears down = %v, want home_in_use", err)
	}
	if inUse.SessionID != first.ID || !inUse.Stopping {
		t.Fatalf("home_in_use = %+v, want the stopping session %s", inUse, first.ID)
	}
	if errors.Is(err, ErrHomeConflict) {
		t.Fatal("a teardown in progress must not read as home_conflict")
	}
	if got := claimState(t, pool, s.userID, appID); got == "conflict" {
		t.Fatal("a refused relaunch during teardown persisted a conflict")
	}
}

// A capable agent holds the claim for the whole life of a session, so a
// running holder must answer like the single-writer gate does.
func TestLaunchWhileHolderRunsIsHomeInUse(t *testing.T) {
	pool := testDB(t)
	store, _, _, p, first := launchHeldHome(t, pool)
	ctx := context.Background()
	_, err := store.Transition(ctx, first.ID, StateRunning, nil, nil)
	must(t, err)

	_, err = store.ScheduleAndCreate(ctx, p)
	var inUse *HomeInUseError
	if !errors.As(err, &inUse) || inUse.SessionID != first.ID || inUse.Stopping {
		t.Fatalf("relaunch over a running holder = %v, want home_in_use naming %s", err, first.ID)
	}
}

// The issue's second half: the row already reads stopped but the agent's
// qualified terminal has not landed yet. The launch waits briefly and then
// succeeds once the proof clears the hold.
func TestLaunchWaitsOutASettlingHold(t *testing.T) {
	pool := testDB(t)
	store, s, _, p, first := launchHeldHome(t, pool)
	ctx := context.Background()
	_, err := store.Transition(ctx, first.ID, StateStopping, nil, nil)
	must(t, err)
	_, err = store.Transition(ctx, first.ID, StateStopped, nil, nil)
	must(t, err)
	withHomeHoldSettleWait(t, 10*time.Second)

	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := store.ClearQualifiedHomeHolds(context.Background(), s.hostID, first.ID); err != nil {
			t.Errorf("clear hold: %v", err)
		}
	}()
	start := time.Now()
	second, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("relaunch after a settling teardown = %v, want success once the hold clears", err)
	}
	if second.ID == first.ID {
		t.Fatal("relaunch returned the old session")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("relaunch waited %v; it should resume as soon as the hold clears", waited)
	}
}

// A settling hold that outlasts the wait still refuses, with home_conflict as
// the contract fixes for a hold, and records nothing for an operator to repair.
func TestSettlingHoldPastTheWaitIsHomeConflictButNotPersisted(t *testing.T) {
	pool := testDB(t)
	store, s, appID, p, first := launchHeldHome(t, pool)
	ctx := context.Background()
	_, err := store.Transition(ctx, first.ID, StateFailed, nil, nil)
	must(t, err)
	withHomeHoldSettleWait(t, 300*time.Millisecond)

	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("relaunch while the hold has not cleared = %v, want home_conflict", err)
	}
	if got := claimState(t, pool, s.userID, appID); got == "conflict" {
		t.Fatal("a pending hold must never be persisted as a claim conflict")
	}
}

// A hold that will not clear on its own is refused at once: waiting cannot help.
func TestStuckHoldIsHomeConflictWithoutWaiting(t *testing.T) {
	cases := map[string]func(t *testing.T, pool *pgxpool.Pool, s seedIDs, first Session){
		"holder ended long ago": func(t *testing.T, pool *pgxpool.Pool, _ seedIDs, first Session) {
			_, err := pool.Exec(context.Background(), `UPDATE sessions SET state='stopped',
				ended_at=now()-interval '1 hour' WHERE id=$1::uuid`, first.ID)
			must(t, err)
		},
		"holder's host is offline": func(t *testing.T, pool *pgxpool.Pool, s seedIDs, first Session) {
			_, err := pool.Exec(context.Background(), `UPDATE sessions SET state='stopped',ended_at=now()
				WHERE id=$1::uuid`, first.ID)
			must(t, err)
			_, err = pool.Exec(context.Background(), `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, s.hostID)
			must(t, err)
		},
		"holder row is gone": func(t *testing.T, pool *pgxpool.Pool, s seedIDs, first Session) {
			_, err := pool.Exec(context.Background(), `UPDATE sessions SET state='failed',ended_at=now()
				WHERE id=$1::uuid`, first.ID)
			must(t, err)
			_, err = pool.Exec(context.Background(), `UPDATE managed_home_claims
				SET pending_home_session_id='00000000-0000-4000-8000-000000000434'
				WHERE user_id=$1::uuid`, s.userID)
			must(t, err)
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			pool := testDB(t)
			store, s, appID, p, first := launchHeldHome(t, pool)
			arrange(t, pool, s, first)
			withHomeHoldSettleWait(t, 30*time.Second)

			start := time.Now()
			_, err := store.ScheduleAndCreate(context.Background(), p)
			if !errors.Is(err, ErrHomeConflict) {
				t.Fatalf("launch over a stuck hold = %v, want home_conflict", err)
			}
			if waited := time.Since(start); waited > 5*time.Second {
				t.Fatalf("stuck hold waited %v; it can never clear by waiting", waited)
			}
			if got := claimState(t, pool, s.userID, appID); got == "conflict" {
				t.Fatal("a pending hold must never be persisted as a claim conflict")
			}
		})
	}
}
