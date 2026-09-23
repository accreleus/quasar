package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A selected app is authoritative at reservation, including after a new host
// enrolls. Cached image state cannot widen a fixed selection.
func TestAppPlacementControlsNewReservations(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	host2, _ := addHost(t, pool, "placement-new-host", 2)
	ctx := context.Background()
	store := NewStore(pool)
	p := launchParams(s)

	// Existing apps begin dynamic; an ordinary new host joins the candidate set.
	_, err := pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	first, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	if first.HostID == nil || *first.HostID != host2 {
		t.Fatalf("dynamic placement used %v, want newly eligible host %s", first.HostID, host2)
	}
	_, err = store.Transition(ctx, first.ID, StateFailed, nil, nil)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE app_placement SET mode='fixed',revision=revision+1 WHERE app_id=$1::uuid`, s.appID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES ($1::uuid,$2::uuid)`, s.appID, s.hostID)
	must(t, err)
	installCatalogImage(t, pool, false)
	setAppImage(t, pool, s.appID, testImageRef)
	setHostImage(t, pool, s.hostID, "ready", testImageVer)
	setHostImage(t, pool, host2, "ready", testImageVer)
	p.AppImage = testImageRef // both cached images are ready; policy still wins
	_, err = pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("fixed selection widened to other host: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid`, s.hostID)
	must(t, err)
	second, err := store.ScheduleAndCreate(ctx, p)
	must(t, err)
	if second.HostID == nil || *second.HostID != s.hostID {
		t.Fatalf("fixed placement used %v, want %s", second.HostID, s.hostID)
	}
	_, err = pool.Exec(ctx, `DELETE FROM app_placement_hosts WHERE app_id=$1::uuid`, s.appID)
	must(t, err)
	_, err = store.ScheduleAndCreate(ctx, p)
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("empty fixed selection admitted a launch: %v", err)
	}
	retained, err := store.Get(ctx, second.ID)
	must(t, err)
	if retained.State != StateAssigned {
		t.Fatalf("placement removal interrupted an already accepted session: %s", retained.State)
	}
}

func TestAppPlacementExplainsExistingHomeOutsideSelection(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	host2, _ := addHost(t, pool, "placement-other-host", 2)
	appID := seedManagedApp(t, pool, `{}`)
	seedHome(t, pool, s.userID, appID, s.hostID)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE app_placement SET mode='fixed',revision=revision+1 WHERE app_id=$1::uuid`, appID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES ($1::uuid,$2::uuid)`, appID, host2)
	must(t, err)
	_, err = NewStore(pool).ScheduleAndCreate(ctx, managedLaunchParams(s, appID))
	if !errors.Is(err, ErrHomeConflict) {
		t.Fatalf("home owner excluded by selection = %v, want repair-required home_conflict", err)
	}
	if got := sessionsOnHost(t, pool, host2); got != 0 {
		t.Fatalf("selected other host reserved %d sessions for owned home", got)
	}
}

func TestSwapRespectsTargetPlacementOnCurrentHost(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	other, _ := addHost(t, pool, "placement-swap-other", 2)
	store := NewStore(pool)
	sess := runningSession(t, store, s)
	target := insertApp(t, pool, "placement-swap-target", 512, 1)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE app_placement SET mode='fixed',revision=revision+1 WHERE app_id=$1::uuid`, target)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES ($1::uuid,$2::uuid)`, target, other)
	must(t, err)
	dispatcher := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, dispatcher, testLogger())
	if _, err := coord.Swap(ctx, sess.ID, target); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("swap into app excluded from current host = %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	must(t, err)
	if got.AppID != sess.AppID || got.StateDetail != nil {
		t.Fatalf("excluded swap modified existing session: %+v", got)
	}
}

func TestPlacementRemovalRacingLaunchExcludesNewReservation(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 2)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `UPDATE app_placement SET mode='fixed' WHERE app_id=$1::uuid`, s.appID)
	must(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO app_placement_hosts(app_id,host_id) VALUES ($1::uuid,$2::uuid)`, s.appID, s.hostID)
	must(t, err)

	// Hold the policy writer's row lock while launch makes its candidate read.
	// It may see the old set, but its final FOR SHARE must wait for removal.
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT 1 FROM app_placement WHERE app_id=$1::uuid FOR UPDATE`, s.appID)
	must(t, err)
	done := make(chan error, 1)
	go func() {
		_, launchErr := NewStore(pool).ScheduleAndCreate(ctx, launchParams(s))
		done <- launchErr
	}()

	blocked := false
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		must(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE wait_event_type='Lock' AND query LIKE '%FROM app_placement%FOR SHARE%'`).Scan(&waiting))
		if waiting > 0 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("launch did not reach the authoritative placement lock")
	}
	_, err = tx.Exec(ctx, `DELETE FROM app_placement_hosts WHERE app_id=$1::uuid`, s.appID)
	must(t, err)
	_, err = tx.Exec(ctx, `UPDATE app_placement SET revision=revision+1 WHERE app_id=$1::uuid`, s.appID)
	must(t, err)
	must(t, tx.Commit(ctx))
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoHostAvailable) {
			t.Fatalf("launch racing placement removal = %v, want no_host_available", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("launch stayed blocked after removal committed")
	}
	if got := countSessions(t, pool); got != 0 {
		t.Fatalf("removed placement left %d new sessions", got)
	}
}
