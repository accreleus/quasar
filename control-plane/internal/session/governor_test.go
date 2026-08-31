package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// P2-03 governor: these prove the scheduler admits only within budget, rejects
// overcommit with the contract's two codes, never overcommits under concurrent
// launches, and frees budget on release. Integration tests — need Postgres.

// TestDeclaredVramNoLongerBinds is the inverse of the old TestVRAMExhaustion
// (#383). Declared per-app VRAM was removed from admission: it was never a cap
// (there is no cgroup VRAM controller, no MPS here, no AMD equivalent), the
// number was a guess typed into an admin form, and under-declaring silently
// oversubscribed. Encode slots are the reservation.
//
// A 2048 MB GPU with 10 slots used to fit exactly two 1024-MB launches. It now
// fits ten, because slots are what bind — and reserved_vram_mb is written 0.
func TestDeclaredVramNoLongerBinds(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seedCap(t, pool, 2048, 10) // 2048 MB vram, 10 slots
	setQuota(t, pool, s.userID, 20)
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d (declared vram must not bind): %v", i+1, err)
		}
	}
	// The 11th exhausts SLOTS, which is the real constraint.
	before := countSessions(t, pool)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("slot-exhausted launch: got %v want ErrCapacityExhausted", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("rejected launch persisted a row: %d → %d", before, after)
	}
	var declared int
	must(t, pool.QueryRow(ctx, `SELECT COALESCE(SUM(reserved_vram_mb),0) FROM sessions
		WHERE gpu_id::text=$1 AND state IN ('assigned','starting','running')`, s.gpuID).Scan(&declared))
	if declared != 0 {
		t.Fatalf("new sessions still reserve declared VRAM: %d MB, want 0", declared)
	}
}

// TestNoHostAvailable_Offline: with no online host, a launch is no_host_available
// (nothing could serve it), distinct from capacity_exhausted.
func TestNoHostAvailable_Offline(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE id::text=$1`, s.hostID); err != nil {
		t.Fatalf("offline host: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("offline-host launch: got %v want ErrNoHostAvailable", err)
	}
}

func TestNoHostAvailable_UntrustedCapacity(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `UPDATE hosts SET capacity_detection='failed' WHERE id::text=$1`, s.hostID); err != nil {
		t.Fatalf("fail capacity: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("failed-capacity launch: got %v want ErrNoHostAvailable", err)
	}
	if got := mustAvail(t, store, s.hostID); len(got) != 0 {
		t.Fatalf("failed-capacity GPU visible: %+v", got)
	}

	if _, err := pool.Exec(ctx, `UPDATE hosts SET capacity_detection='ok' WHERE id::text=$1`, s.hostID); err != nil {
		t.Fatalf("restore capacity: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE gpus SET reported=false WHERE id::text=$1`, s.gpuID); err != nil {
		t.Fatalf("stale inventory: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("stale-GPU launch: got %v want ErrNoHostAvailable", err)
	}
	if got := mustAvail(t, store, s.hostID); len(got) != 0 {
		t.Fatalf("stale GPU visible: %+v", got)
	}
}

func TestSchedulerUsesOnlyConfiguredGPUNode(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()
	const node0 = "/dev/dri/by-path/pci-0000:04:00.0-render"
	const node1 = "/dev/dri/by-path/pci-0000:05:00.0-render"
	if _, err := pool.Exec(ctx, `UPDATE gpus SET vendor='amd', render_node=$2, device_path='/dev/dri/renderD128' WHERE id::text=$1`, s.gpuID, node0); err != nil {
		t.Fatalf("configure gpu0: %v", err)
	}
	var gpu1 string
	if err := pool.QueryRow(ctx, `INSERT INTO gpus(host_id,index,vendor,vram_mb_total,encode_slots_total,render_node,device_path)
		VALUES($1,1,'amd',16384,4,$2,'/dev/dri/renderD129') RETURNING id::text`, s.hostID, node1).Scan(&gpu1); err != nil {
		t.Fatalf("seed gpu1: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET effective_settings=$2::jsonb WHERE id::text=$1`, s.hostID,
		`{"encoder":"va","render_node":"/dev/dri/renderD129"}`); err != nil {
		t.Fatalf("configure host binding: %v", err)
	}
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != gpu1 {
		t.Fatalf("scheduled gpu=%v want configured gpu1=%s", sess.GPUID, gpu1)
	}
}

// TestSchedulerUnsetRenderNodeSchedulesAnyGPU: an empty or absent render_node
// in effective_settings is "unpinned" — any vendor-compatible GPU is eligible
// (a fresh deploy leaves QUASAR_RENDER_NODE unset and compose passes empty);
// a mismatched non-empty value still excludes the host.
func TestSchedulerUnsetRenderNodeSchedulesAnyGPU(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE gpus SET vendor='amd',
		render_node='/dev/dri/by-path/pci-0000:04:00.0-render',
		device_path='/dev/dri/renderD128' WHERE id::text=$1`, s.gpuID); err != nil {
		t.Fatalf("configure gpu: %v", err)
	}
	for _, settings := range []string{
		`{"encoder":"va","render_node":""}`, // fresh .env: compose passes empty
		`{"encoder":"va"}`,                  // key absent: ->> extracts NULL
	} {
		if _, err := pool.Exec(ctx, `UPDATE hosts SET effective_settings=$2::jsonb WHERE id::text=$1`,
			s.hostID, settings); err != nil {
			t.Fatalf("configure host %s: %v", settings, err)
		}
		sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
		if err != nil {
			t.Fatalf("schedule with %s: %v", settings, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE sessions SET state='stopped' WHERE id::text=$1`, sess.ID); err != nil {
			t.Fatalf("release session: %v", err)
		}
	}
	// Explicitly set but matching no GPU: still excluded (exact-match semantics).
	if _, err := pool.Exec(ctx, `UPDATE hosts SET effective_settings=$2::jsonb WHERE id::text=$1`,
		s.hostID, `{"encoder":"va","render_node":"/dev/dri/renderD999"}`); err != nil {
		t.Fatalf("configure mismatched host: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("mismatched render_node launch: got %v want ErrNoHostAvailable", err)
	}
}

func TestCapacityFailureSerializesWithAdmission(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin capacity mutation: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `UPDATE hosts SET capacity_detection='failed' WHERE id::text=$1`, s.hostID); err != nil {
		t.Fatalf("stage capacity failure: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.ScheduleAndCreate(ctx, launchParams(s))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("scheduler escaped host capacity lock early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit capacity failure: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoHostAvailable) {
			t.Fatalf("raced launch: got %v want ErrNoHostAvailable", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler remained blocked after capacity commit")
	}
	if got := countSessions(t, pool); got != 0 {
		t.Fatalf("raced capacity failure admitted %d sessions", got)
	}
}

func TestInventoryRemovalDuringAdmissionRetriesCleanly(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin inventory mutation: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM gpus WHERE id::text=$1`, s.gpuID); err != nil {
		t.Fatalf("stage inventory removal: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.ScheduleAndCreate(ctx, launchParams(s))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("scheduler escaped GPU row lock early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit inventory removal: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoHostAvailable) {
			t.Fatalf("inventory race: got %v want ErrNoHostAvailable", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler remained blocked after inventory commit")
	}
	if got := countSessions(t, pool); got != 0 {
		t.Fatalf("removed inventory admitted %d sessions", got)
	}
}

// TestNoHostAvailable_ExceedsTotals: the request needs more encode slots than any
// online GPU's *total*, so no GPU could ever serve it ⇒ no_host_available, not
// exhausted. (The VRAM half of this test moved to
// TestVetoImpossibleFloorIsNoHostAvailable in vram_admission_test.go — declared
// VRAM no longer participates in admission at all.)
func TestNoHostAvailable_ExceedsTotals(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seedCap(t, pool, 512, 1) // GPU has 1 encode slot
	ctx := context.Background()

	p := launchParams(s)
	p.NeedEncodeSlots = 4 // more than the GPU's TOTAL ⇒ never satisfiable
	if _, err := store.ScheduleAndCreate(ctx, p); !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("over-totals launch: got %v want ErrNoHostAvailable", err)
	}
}

// TestConcurrentLaunchesNoOvercommit is the test that catches a broken locking
// strategy: fire N simultaneous launches at a GPU with room for C < N. Exactly C
// must be admitted, the rest rejected with capacity_exhausted, and the reserved
// sum must never exceed the total.
func TestConcurrentLaunchesNoOvercommit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	const capacity = 3
	const launches = 8
	s := seed(t, pool, capacity) // C encode slots; vram generous
	ctx := context.Background()

	// Raise the per-user quota above the launch count so GPU capacity (encode
	// slots) is the sole binding constraint, not the per-user session limit.
	setQuota(t, pool, s.userID, launches*2)

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

	admitted, exhausted, other := 0, 0, 0
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
	if other != 0 {
		t.Fatalf("unexpected errors: %d (errs=%v)", other, errs)
	}
	if admitted != capacity {
		t.Fatalf("admitted: got %d want %d (exhausted=%d)", admitted, capacity, exhausted)
	}
	if exhausted != launches-capacity {
		t.Fatalf("exhausted: got %d want %d", exhausted, launches-capacity)
	}
	// The reserved sum must equal capacity and never exceed the total.
	if slots := reservedSlots(t, pool, s.gpuID); slots != capacity {
		t.Fatalf("reserved encode slots: got %d want %d (overcommit!)", slots, capacity)
	}
}

// TestReleaseFreesBudgetUnderConcurrency: after the concurrent fill, stopping one
// admitted session frees exactly one slot for a subsequent launch.
func TestReleaseFreesBudgetAfterStop(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 2)
	ctx := context.Background()

	a, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch a: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("launch b: %v", err)
	}
	// Full now.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("launch c (full): got %v want ErrCapacityExhausted", err)
	}
	// Release a, then a new launch is admitted.
	if _, err := store.Transition(ctx, a.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("stop a: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("launch d after release: %v", err)
	}
}

// reservedSlots sums the encode slots held by active sessions on a GPU (the
// schema.md availability filter) — the observable check for overcommit.
func reservedSlots(t *testing.T, pool *pgxpool.Pool, gpuID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(SUM(reserved_encode_slots), 0)
		FROM sessions
		WHERE gpu_id::text = $1 AND state IN ('assigned','starting','running')
	`, gpuID).Scan(&n); err != nil {
		t.Fatalf("sum reserved slots: %v", err)
	}
	return n
}
