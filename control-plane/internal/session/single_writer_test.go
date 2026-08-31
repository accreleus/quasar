package session

// P5-04 single-writer guard: per-(user, app) home is single-writer for managed-home apps.
//
// Tests require Postgres (TEST_DATABASE_URL). Run with scripts/dev/dev.sh go-test-db.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/storage"
)

// seedManagedApp2 adds a second managed-home app (named differently to avoid PK collision
// when the same pool is reused within a test).
func seedManagedApp2(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path)
		VALUES ('managed-app-2', 512, 1, 1280, 720, 30, 2000, '{}', true, '/home/quasar')
		RETURNING id::text`).Scan(&id))
	entitleAll(t, pool, id)
	return id
}

// managedLaunchParams returns CreateParams with ManagedHome=true for the given seedIDs + appID.
func managedLaunchParams(s seedIDs, appID string) CreateParams {
	p := launchParams(s)
	p.AppID = appID
	p.ManagedHome = true
	return p
}

// TestSingleWriterDenySecondLaunch: second launch of same (user, managed app) is refused
// with ErrHomeInUse; no extra row persisted.
func TestSingleWriterDenySecondLaunch(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	managedApp := seedManagedApp(t, pool, `{}`)
	ctx := context.Background()

	first, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if first.State != StateAssigned {
		t.Fatalf("first state = %s, want assigned", first.State)
	}

	before := countSessions(t, pool)
	_, err = store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("second launch: got %v, want ErrHomeInUse", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("ErrHomeInUse persisted a row: %d → %d", before, after)
	}
}

// TestSingleWriterAllowDifferentApps: same user with two different managed apps can hold
// both concurrently (separate homes, separate writer slots).
func TestSingleWriterAllowDifferentApps(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	appA := seedManagedApp(t, pool, `{}`)
	appB := seedManagedApp2(t, pool)
	ctx := context.Background()

	setQuota(t, pool, s.userID, 5)

	if _, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, appA)); err != nil {
		t.Fatalf("launch app A: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, appB)); err != nil {
		t.Fatalf("launch app B (different managed app, should be allowed): %v", err)
	}
}

// TestSingleWriterAllowDifferentUsers: two users launching the same managed app do not
// block each other — the guard is per-(user, app), not per-app.
func TestSingleWriterAllowDifferentUsers(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	a := seed(t, pool, 10)
	managedApp := seedManagedApp(t, pool, `{}`)

	var bUserID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('sw-b@test.local','sw-b','x') RETURNING id::text`).Scan(&bUserID))
	setQuota(t, pool, bUserID, 5)

	bParams := managedLaunchParams(a, managedApp)
	bParams.UserID = bUserID

	if _, err := store.ScheduleAndCreate(ctx, managedLaunchParams(a, managedApp)); err != nil {
		t.Fatalf("user A launch: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, bParams); err != nil {
		t.Fatalf("user B launch (different user, same app — should be allowed): %v", err)
	}
}

// TestSingleWriterAllowNonManaged: non-managed app can be launched twice by the same user
// (existing behavior preserved — guard only fires for managed_home=true apps).
func TestSingleWriterAllowNonManaged(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	ctx := context.Background()

	setQuota(t, pool, s.userID, 5)

	// launchParams uses s.appID which is non-managed.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("first non-managed launch: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("second non-managed launch (should be allowed): %v", err)
	}
}

// TestSingleWriterConcurrentLaunch: two goroutines simultaneously launching the same
// (user, managed app) — exactly one wins, one gets ErrHomeInUse.
// Follows the TestUpdateUserLastAdminRace pattern.
func TestSingleWriterConcurrentLaunch(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	managedApp := seedManagedApp(t, pool, `{}`)
	ctx := context.Background()

	setQuota(t, pool, s.userID, 5)

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	var oks, homeInUse int
	for err := range errs {
		switch {
		case err == nil:
			oks++
		case errors.Is(err, ErrHomeInUse):
			homeInUse++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if oks != 1 || homeInUse != 1 {
		t.Errorf("want exactly one success and one ErrHomeInUse; got ok=%d homeInUse=%d", oks, homeInUse)
	}

	// Exactly one session row.
	var n int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions
		WHERE user_id::text = $1 AND app_id::text = $2`, s.userID, managedApp).Scan(&n))
	if n != 1 {
		t.Errorf("sessions for (user, app) = %d, want 1", n)
	}
}

// TestSingleWriterSameAppSwapAllowed: swapping a session to the same managed app it is
// currently running must succeed (the single running session does not count against itself).
func TestSingleWriterSameAppSwapAllowed(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	// Launch and drive to running.
	sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("launch managed app: %v", err)
	}
	sess, err = store.Transition(ctx, sess.ID, StateRunning, nil, nil)
	if err != nil {
		t.Fatalf("→ running: %v", err)
	}

	// Swap to the same app — must not be blocked.
	if _, err := coord.Swap(ctx, sess.ID, managedApp); err != nil {
		t.Fatalf("same-app swap rejected: %v (want success)", err)
	}
}

// TestSingleWriterDenySwapToOccupied: if a user already has a live session of managed app B,
// swapping another session to app B is refused with ErrHomeInUse.
func TestSingleWriterDenySwapToOccupied(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	managedAppB := seedManagedApp(t, pool, `{}`)
	disp := newCapturingDispatcher()
	coord := newTestCoordinator(t, store, disp, testLogger(),
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	ctx := context.Background()

	setQuota(t, pool, s.userID, 5)

	// Session 1: already running managed app B.
	sess1Params := managedLaunchParams(s, managedAppB)
	sess1, err := store.ScheduleAndCreate(ctx, sess1Params)
	if err != nil {
		t.Fatalf("launch sess1 (app B): %v", err)
	}
	if _, err := store.Transition(ctx, sess1.ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("sess1 → running: %v", err)
	}

	// Session 2: running non-managed app (so it can be swapped).
	sess2, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch sess2 (non-managed): %v", err)
	}
	sess2, err = store.Transition(ctx, sess2.ID, StateRunning, nil, nil)
	if err != nil {
		t.Fatalf("sess2 → running: %v", err)
	}

	// Swap sess2 to app B — must be refused because sess1 already holds the home.
	_, err = coord.Swap(ctx, sess2.ID, managedAppB)
	if !errors.Is(err, ErrHomeInUse) {
		t.Fatalf("swap to occupied managed app: got %v, want ErrHomeInUse", err)
	}

	// sess2 must be unchanged (still running, no swap detail set).
	got, _ := store.Get(ctx, sess2.ID)
	if got.State != StateRunning {
		t.Errorf("sess2 state = %s after rejected swap, want running", got.State)
	}
	if got.StateDetail != nil && *got.StateDetail == swapDetailInProgress {
		t.Error("sess2 state_detail set to swapping despite rejected guard")
	}
}
