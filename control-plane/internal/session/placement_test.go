package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// P3-02 multi-host placement: these prove the scheduler spreads launches across
// online hosts per the policy, excludes draining/offline hosts, never overcommits
// a GPU under concurrent launches (now under per-GPU locking), runs different-GPU
// launches concurrently, and still holds the per-user quota under concurrency
// (the per-user advisory lock). Integration tests — need Postgres. The policy
// parser unit test below runs unconditionally.

// --- helpers ----------------------------------------------------------------

// seedSecondHost adds a second online host ('host-2') with one GPU, sharing the
// user/app seeded by seed/seedCap. node_name is UNIQUE, so this is a distinct host.
func seedSecondHost(t *testing.T, pool *pgxpool.Pool, vramMBTotal, encodeSlots int) (hostID, gpuID string) {
	t.Helper()
	ctx := context.Background()
	must(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('host-2','online','ok') RETURNING id::text`).Scan(&hostID))
	must(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 0, $2, $3) RETURNING id::text`, hostID, vramMBTotal, encodeSlots).Scan(&gpuID))
	return
}

// seedExtraUser inserts an additional user with the given quota, so a concurrency
// test can fire launches that are NOT serialized by the per-user advisory lock.
func seedExtraUser(t *testing.T, pool *pgxpool.Pool, i, quota int) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(),
		`INSERT INTO users (email, username, password_hash, max_concurrent_sessions)
		 VALUES ($1, $2, 'x', $3) RETURNING id::text`,
		fmt.Sprintf("u%d@test.local", i), fmt.Sprintf("u%d", i), quota).Scan(&id))
	return id
}

// sessionsOnHost counts the active sessions placed on a host.
func sessionsOnHost(t *testing.T, pool *pgxpool.Pool, hostID string) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM sessions
		WHERE host_id::text = $1 AND state IN ('assigned','starting','running')
	`, hostID).Scan(&n))
	return n
}

func launchAs(s seedIDs, userID string) CreateParams {
	p := launchParams(s)
	p.UserID = userID
	return p
}

// --- policy parser (no DB) --------------------------------------------------

func TestParsePlacementPolicy(t *testing.T) {
	ok := []string{"", "spread", "least-loaded", "SPREAD", "  Least-Loaded  "}
	for _, v := range ok {
		p, err := ParsePlacementPolicy(v)
		if err != nil || p != PolicySpread {
			t.Fatalf("ParsePlacementPolicy(%q): got (%v, %v) want (spread, nil)", v, p, err)
		}
	}
	for _, v := range []string{"locality", "LOCALITY", "  Locality  "} {
		p, err := ParsePlacementPolicy(v)
		if err != nil || p != PolicyLocality {
			t.Fatalf("ParsePlacementPolicy(%q): got (%v, %v) want (locality, nil)", v, p, err)
		}
	}
	for _, v := range []string{"binpack", "garbage", "random"} {
		if _, err := ParsePlacementPolicy(v); err == nil {
			t.Fatalf("ParsePlacementPolicy(%q): want error, got nil", v)
		}
	}
}

// --- placement (DB) ---------------------------------------------------------

// TestSpreadDistributesAcrossHosts: with two online hosts of equal capacity, the
// least-loaded policy balances sequential launches across them (not all on one).
func TestSpreadDistributesAcrossHosts(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool) // default PolicySpread
	s := seed(t, pool, 4)   // host-1, gpu with 4 encode slots, generous vram
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if h1n, h2n := sessionsOnHost(t, pool, s.hostID), sessionsOnHost(t, pool, h2); h1n != 2 || h2n != 2 {
		t.Fatalf("spread: host-1=%d host-2=%d, want 2/2 (not concentrated)", h1n, h2n)
	}
}

// TestDrainingHostExcluded: a draining host is never a placement candidate even
// with free capacity; launches land only on the online host, and once it is full
// the result is capacity_exhausted (not a placement on the draining host).
func TestDrainingHostExcluded(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2) // host-1, 2 slots — will be drained
	h2, _ := seedSecondHost(t, pool, 16384, 2)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id::text=$1`, s.hostID); err != nil {
		t.Fatalf("drain host-1: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if n := sessionsOnHost(t, pool, s.hostID); n != 0 {
		t.Fatalf("placed %d session(s) on the DRAINING host-1; want 0", n)
	}
	if n := sessionsOnHost(t, pool, h2); n != 2 {
		t.Fatalf("host-2 (online): got %d sessions want 2", n)
	}
	// host-2 is now full; host-1 is draining ⇒ no candidate ⇒ capacity_exhausted.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("launch onto full+draining fleet: got %v want ErrCapacityExhausted", err)
	}
	if n := sessionsOnHost(t, pool, s.hostID); n != 0 {
		t.Fatalf("a rejected launch leaked onto the draining host-1: %d", n)
	}
}

// TestConcurrentSameGPUDifferentUsersNoOvercommit fires N simultaneous launches
// from DISTINCT users at a single GPU with room for C < N. Distinct users are not
// serialized by the per-user lock, so this genuinely exercises the per-GPU lock:
// exactly C admitted, the rest capacity_exhausted, reserved sum never exceeds C.
func TestConcurrentSameGPUDifferentUsersNoOvercommit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	const capacity = 3
	const launches = 8
	s := seed(t, pool, capacity) // one GPU, C slots, generous vram
	ctx := context.Background()

	users := make([]string, launches)
	for i := range users {
		users[i] = seedExtraUser(t, pool, i, 5)
	}

	var wg sync.WaitGroup
	errs := make([]error, launches)
	for i := 0; i < launches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.ScheduleAndCreate(ctx, launchAs(s, users[i]))
		}(i)
	}
	wg.Wait()

	admitted, exhausted, other := tally(errs)
	if other != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if admitted != capacity || exhausted != launches-capacity {
		t.Fatalf("admitted=%d exhausted=%d, want %d/%d", admitted, exhausted, capacity, launches-capacity)
	}
	if slots := reservedSlots(t, pool, s.gpuID); slots != capacity {
		t.Fatalf("reserved encode slots: got %d want %d (overcommit!)", slots, capacity)
	}
}

// TestConcurrentAcrossGPUsAllAdmitted fires launches at a two-GPU fleet whose
// total capacity exactly fits them. Different GPUs take different locks, so the
// launches proceed concurrently and all are admitted — neither GPU overcommitted.
func TestConcurrentAcrossGPUsAllAdmitted(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2) // host-1 gpu: 2 slots
	_, g2 := seedSecondHost(t, pool, 16384, 2)
	const launches = 4 // == total capacity (2 + 2)
	ctx := context.Background()

	users := make([]string, launches)
	for i := range users {
		users[i] = seedExtraUser(t, pool, i, 5)
	}

	var wg sync.WaitGroup
	errs := make([]error, launches)
	for i := 0; i < launches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.ScheduleAndCreate(ctx, launchAs(s, users[i]))
		}(i)
	}
	wg.Wait()

	admitted, _, other := tally(errs)
	if other != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if admitted != launches {
		t.Fatalf("admitted=%d, want %d (full two-GPU utilization)", admitted, launches)
	}
	if a, b := reservedSlots(t, pool, s.gpuID), reservedSlots(t, pool, g2); a != 2 || b != 2 {
		t.Fatalf("per-GPU reserved: g1=%d g2=%d, want 2/2 (no overcommit)", a, b)
	}
}

// TestConcurrentQuotaNoOverrun: a single user with quota Q fires N>Q simultaneous
// launches at a fleet with abundant capacity. The per-user advisory lock must
// serialize them so exactly Q are admitted — without it, several would pass the
// COUNT-based quota check concurrently and overrun the limit.
func TestConcurrentQuotaNoOverrun(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	const quota = 2
	const launches = 6
	s := seed(t, pool, 50) // capacity is NOT the binding constraint
	seedSecondHost(t, pool, 16384, 50)
	setQuota(t, pool, s.userID, quota)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, launches)
	for i := 0; i < launches; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.ScheduleAndCreate(ctx, launchParams(s))
		}(i)
	}
	wg.Wait()

	admitted, quotaExceeded := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrSessionQuotaExceeded):
			quotaExceeded++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if admitted != quota || quotaExceeded != launches-quota {
		t.Fatalf("admitted=%d quota_exceeded=%d, want %d/%d (quota overrun!)", admitted, quotaExceeded, quota, launches-quota)
	}
	var active int
	must(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM sessions
		WHERE user_id::text=$1 AND state IN ('pending','assigned','starting','running')`, s.userID).Scan(&active))
	if active != quota {
		t.Fatalf("user holds %d active sessions, want %d (quota overrun)", active, quota)
	}
}

// tally classifies launch results into admitted / capacity_exhausted / other.
func tally(errs []error) (admitted, exhausted, other int) {
	for _, err := range errs {
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrCapacityExhausted):
			exhausted++
		default:
			other++
		}
	}
	return
}

// --- locality placement (DB) ------------------------------------------------
// P5-07: locality policy prefers the host that holds a user's home for the app.

// seedHome inserts a live (non-tombstoned) user_homes row pinning (user, app) to host.
func seedHome(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'volume', 'quasar-'||$1||'-'||$2)
		ON CONFLICT (user_id, app_id, host_id) DO NOTHING
	`, userID, appID, hostID)
	must(t, err)
}

// seedTombstonedHome inserts a user_homes row that has been tombstoned (gc_after IS NOT NULL).
func seedTombstonedHome(t *testing.T, pool *pgxpool.Pool, userID, appID, hostID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO user_homes (user_id, app_id, host_id, provider, ref, gc_after)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'volume', 'quasar-'||$1||'-'||$2||'-tomb', now())
		ON CONFLICT (user_id, app_id, host_id) DO UPDATE SET gc_after = now()
	`, userID, appID, hostID)
	must(t, err)
}

// stopSession drives a session to the stopped state so its slot is freed.
func stopSession(t *testing.T, store *Store, id string) {
	t.Helper()
	_, err := store.Transition(context.Background(), id, StateStopped, nil, nil)
	must(t, err)
}

// TestLocalityPrefersHomeHost: user has a home on host A; locality policy places
// the launch there even when host B has equal (or more) headroom.
func TestLocalityPrefersHomeHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// Pin (user, managedApp) to host 1.
	seedHome(t, pool, s.userID, managedApp, s.hostID)

	// First launch: should prefer host 1.
	p := managedLaunchParams(s, managedApp)
	sess, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		h2str := h2
		t.Fatalf("locality: placed on %v, want host-1 (%s); host-2=%s", sess.HostID, s.hostID, h2str)
	}

	// Stop it, then relaunch: locality should return to host 1 again.
	stopSession(t, store, sess.ID)
	sess2, err := store.ScheduleAndCreate(ctx, p)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if sess2.HostID == nil || *sess2.HostID != s.hostID {
		t.Fatalf("locality re-launch: placed on %v, want host-1 (%s)", sess2.HostID, s.hostID)
	}
}

// TestLocalityFallsBackWhenCordoned: home is on host A (draining); the launch
// falls back to spread and lands on host B.
func TestLocalityFallsBackWhenCordoned(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	seedHome(t, pool, s.userID, managedApp, s.hostID)

	// Drain host 1 (the home host).
	must(t, pool.QueryRow(ctx, `UPDATE hosts SET status='draining' WHERE id::text=$1 RETURNING id::text`, s.hostID).Scan(new(string)))

	sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("launch onto cordoned home: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != h2 {
		t.Fatalf("cordoned fallback: placed on %v, want host-2 (%s)", sess.HostID, h2)
	}
}

// TestLocalityFallsBackWhenFull: home is on host A but A has no encode capacity;
// the launch falls back to host B.
func TestLocalityFallsBackWhenFull(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 1) // host-1: 1 slot only
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	seedHome(t, pool, s.userID, managedApp, s.hostID)

	// Directly occupy host-1's single slot. A scheduled launch of the filler would
	// go to host-2 first (spread picks most-free-slots: 4 > 1), leaving host-1 free
	// and defeating the test. Direct insertion gives controlled capacity.
	filler := seedExtraUser(t, pool, 99, 5)
	_, err := pool.Exec(ctx, `
		INSERT INTO sessions (
			user_id, app_id, host_id, gpu_id, state,
			width, height, fps, bitrate_kbps, h264_profile, playout0_ms,
			reserved_vram_mb, reserved_encode_slots,
			signaling_token_expires_at, assigned_at
		) VALUES (
			$1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running',
			1280, 720, 60, 6000, 'constrained-baseline', 0,
			1024, 1, now() + interval '1 hour', now()
		)
	`, filler, s.appID, s.hostID, s.gpuID)
	must(t, err)

	// Now the managed-app launch can't fit on the home host → falls back to h2.
	sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("launch onto full home: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != h2 {
		t.Fatalf("full-home fallback: placed on %v, want host-2 (%s)", sess.HostID, h2)
	}
}

// TestLocalityFallsBackWhenOffline: home host is offline; falls back to the
// online host.
func TestLocalityFallsBackWhenOffline(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	seedHome(t, pool, s.userID, managedApp, s.hostID)

	must(t, pool.QueryRow(ctx, `UPDATE hosts SET status='offline' WHERE id::text=$1 RETURNING id::text`, s.hostID).Scan(new(string)))

	sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("launch onto offline home: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != h2 {
		t.Fatalf("offline fallback: placed on %v, want host-2 (%s)", sess.HostID, h2)
	}
}

// TestLocalityNoHome: user has no user_homes row; locality follows spread exactly
// (same 2/2 distribution as TestSpreadDistributesAcrossHosts).
func TestLocalityNoHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// No seedHome call — user has no home row.

	for i := 0; i < 4; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if h1n, h2n := sessionsOnHost(t, pool, s.hostID), sessionsOnHost(t, pool, h2); h1n != 2 || h2n != 2 {
		t.Fatalf("locality no-home: host-1=%d host-2=%d, want 2/2 (spread fallback)", h1n, h2n)
	}
}

// TestLocalityNonManagedApp: non-managed app with locality policy spreads across
// hosts (no user_homes rows ever exist for non-managed apps).
func TestLocalityNonManagedApp(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// s.appID is the non-managed app from seed(). Launch 4 times.
	for i := 0; i < 4; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if h1n, h2n := sessionsOnHost(t, pool, s.hostID), sessionsOnHost(t, pool, h2); h1n != 2 || h2n != 2 {
		t.Fatalf("locality non-managed: host-1=%d host-2=%d, want 2/2 spread", h1n, h2n)
	}
}

// TestLocalityTombstonedHome: a tombstoned home (gc_after IS NOT NULL) must NOT
// attract placement. The test seeds a tombstoned home on host-1 and a live home
// on host-2 for the same (user, app). The launch must go to host-2 (live home),
// proving the tombstoned row on host-1 is ignored.
func TestLocalityTombstonedHome(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool, WithPlacementPolicy(PolicyLocality))
	s := seed(t, pool, 4)
	h2, _ := seedSecondHost(t, pool, 16384, 4)
	managedApp := seedManagedApp(t, pool, `{}`)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// Tombstoned home on host-1 must be ignored by the locality subquery.
	seedTombstonedHome(t, pool, s.userID, managedApp, s.hostID)
	// Live home on host-2: the locality subquery (gc_after IS NULL) picks this one.
	seedHome(t, pool, s.userID, managedApp, h2)

	sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
	if err != nil {
		t.Fatalf("tombstone launch: %v", err)
	}
	// Must land on host-2 (live home), not host-1 (tombstoned).
	if sess.HostID == nil || *sess.HostID != h2 {
		t.Fatalf("tombstoned home: placed on %v, want host-2 (%s); tombstone on host-1 must be ignored",
			sess.HostID, h2)
	}
}
