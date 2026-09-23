package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/storage"
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

func TestLaunchRefusesHomeHeldByTerminalSession(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	store := NewStore(pool)
	ctx := context.Background()
	p := managedLaunchParams(s, appID)
	p.PinHostID = s.hostID
	first, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	_, err = store.Transition(ctx, first.ID, StateFailed, nil, nil)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE managed_home_claims SET
		pending_home_session_id=$3::uuid,pending_home_token=$4::uuid,pending_home_started_at=now()
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`,
		s.userID, appID, first.ID, "00000000-0000-4000-8000-000000000049")
	must(t, err)
	t.Cleanup(func() {
		_, cleanupErr := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
			pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, s.userID, appID)
		if cleanupErr != nil {
			t.Errorf("clear test hold: %v", cleanupErr)
		}
	})
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("launch against uncertain terminal-session home = %v, want home_conflict", err)
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

func TestLaterLocationMismatchPersistsAfterRefusedLaunch(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state,materialized_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'materialized',now())`, s.userID, appID, s.hostID)
	must(t, err)
	seedHome(t, pool, s.userID, appID, h2) // later recorded location
	_, err = NewStore(pool).ScheduleAndCreate(ctx, managedLaunchParams(s, appID))
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("divergent launch: %v, want home_conflict", err)
	}
	claims, _, err := storage.NewLocal(pool, t.TempDir()).ListHomeClaims(ctx,
		storage.ListHomeClaimsOpts{UserID: s.userID, AppID: appID})
	must(t, err)
	if len(claims) != 1 || claims[0].State != "conflict" ||
		claims[0].ConflictReason == nil || *claims[0].ConflictReason != "location_mismatch" ||
		claims[0].MaterializedAt == nil {
		t.Fatalf("admin diagnosis after refusal = %+v, want persisted location_mismatch with use history", claims)
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

// An accepted running callback locks claim then home. A concurrent reservation
// must wait on that same claim before taking a legacy home row lock, otherwise
// the two transactions can deadlock in opposite directions.
func TestHomeReservationLocksClaimBeforeLegacyHome(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO managed_home_claims (user_id,canonical_app_id,host_id,state)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'reserved')`, s.userID, appID, s.hostID)
	must(t, err)
	callbackTx, err := pool.Begin(ctx)
	must(t, err)
	defer callbackTx.Rollback(ctx) //nolint:errcheck
	var locked string
	must(t, callbackTx.QueryRow(ctx, `SELECT canonical_app_id::text FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, s.userID, appID).Scan(&locked))
	reservationTx, err := pool.Begin(ctx)
	must(t, err)
	defer reservationTx.Rollback(ctx) //nolint:errcheck
	var reservationPID int
	must(t, reservationTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&reservationPID))
	result := make(chan error, 1)
	go func() { result <- claimSelectedHome(ctx, reservationTx, managedLaunchParams(s, appID), s.hostID) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting bool
		must(t, pool.QueryRow(ctx, `SELECT wait_event_type='Lock' AND query LIKE '%managed_home_claims%'
			FROM pg_stat_activity WHERE pid=$1`, reservationPID).Scan(&waiting))
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reservation never reached the claim lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A callback holding claim may now lock the home row without waiting on the
	// reservation. The previous home→claim order fails NOWAIT here.
	must(t, callbackTx.QueryRow(ctx, `SELECT id::text FROM user_homes
		WHERE user_id=$1::uuid AND app_id=$2::uuid FOR UPDATE NOWAIT`, s.userID, appID).Scan(&locked))
	must(t, callbackTx.Rollback(ctx))
	select {
	case err := <-result:
		must(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("reservation did not resume after claim release")
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
