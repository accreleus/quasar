package session

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// A known home is a placement constraint even when its host cannot run a
// session. An idle GPU elsewhere must never create a blank replacement.
func TestLaunchRefusesReplacementForOfflineHomeOwner(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	_, err := pool.Exec(context.Background(), `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, s.hostID)
	must(t, err)

	p := managedLaunchParams(s, appID)
	_, err = NewStore(pool).ScheduleAndCreate(context.Background(), p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("launch with home on offline host: %v, want no_host_available; other host %s must remain unused", err, h2)
	}
	if got := sessionsOnHost(t, pool, h2); got != 0 {
		t.Fatalf("new host has %d sessions, want none", got)
	}
}

func TestLaunchRefusesDivergentLegacyHomes(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	seedHome(t, pool, s.userID, appID, h2)

	_, err := NewStore(pool).ScheduleAndCreate(context.Background(), managedLaunchParams(s, appID))
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("launch with divergent homes: %v, want home_conflict", err)
	}
	if got := sessionsOnHost(t, pool, s.hostID) + sessionsOnHost(t, pool, h2); got != 0 {
		t.Fatalf("conflicted launch reserved %d sessions", got)
	}
}

// An INSERT failure rolls the first claim back with its session transaction;
// a failure after the reservation commits cannot assert that the host created
// no files, so the owner must remain pinned.
func TestFirstHomeClaimRollbackAndPostCommitUncertainty(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	store := NewStore(pool)
	ctx := context.Background()
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	p.Codec = "vp9" // the session CHECK fails after claimSelectedHome
	if _, err := store.ScheduleAndCreate(ctx, p); err == nil {
		t.Fatal("invalid session insert unexpectedly succeeded")
	}
	var claims int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&claims))
	if claims != 0 {
		t.Fatalf("precommit failure retained %d claims, want rollback", claims)
	}

	p.Codec = ""
	sess, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	_, err = store.Transition(ctx, sess.ID, StateFailed, nil, nil)
	must(t, err)
	var owner, state string
	must(t, pool.QueryRow(ctx, `SELECT host_id::text, state FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&owner, &state))
	if owner != s.hostID || state != "reserved" {
		t.Fatalf("postcommit failure claim = (%s,%s), want (%s,reserved)", owner, state, s.hostID)
	}
	_, err = pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	p.PinHostID = ""
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("uncertain home owner moved to %s: %v", h2, err)
	}
}

func TestConcurrentFirstHomesClaimOneHost(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	store := NewStore(pool)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, hostID := range []string{s.hostID, h2} {
		wg.Add(1)
		go func(hostID string) {
			defer wg.Done()
			<-start
			p := managedLaunchParams(s, appID)
			p.PinHostID = hostID
			_, err := store.ScheduleAndCreate(context.Background(), p)
			results <- err
		}(hostID)
	}
	close(start)
	wg.Wait()
	close(results)
	var successful int
	for err := range results {
		switch {
		case err == nil:
			successful++
		case errors.Is(err, ErrHomeInUse), errors.Is(err, ErrHomeConflict):
		default:
			t.Fatalf("concurrent first-home launch: %v", err)
		}
	}
	if successful != 1 {
		t.Fatalf("successful launches = %d, want exactly one", successful)
	}
	var claimCount int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID).Scan(&claimCount))
	if claimCount != 1 {
		t.Fatalf("canonical claim count = %d, want one", claimCount)
	}
	if n := sessionsOnHost(t, pool, s.hostID) + sessionsOnHost(t, pool, h2); n != 1 {
		t.Fatalf("session reservations = %d, want one", n)
	}
}

func TestRunningEvidenceRequiresAssignedAgentHost(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	store := NewStore(pool)
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	sess, err := store.ScheduleAndCreate(context.Background(), p)
	must(t, err)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	coord.AgentState(context.Background(), h2, agentws.SessionStateMsg{SessionID: sess.ID, State: "running"})
	got, err := store.Get(context.Background(), sess.ID)
	must(t, err)
	if got.State != StateAssigned {
		t.Fatalf("other host changed session to %s, want assigned", got.State)
	}
}
