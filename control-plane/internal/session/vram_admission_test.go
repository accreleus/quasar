package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Live free-VRAM admission veto (#383 §4). Encode slots stay the transactional
// reservation; these prove the ADVISORY veto layered on top: it refuses a GPU
// that is already out of memory, it abstains on every kind of unknown, and it
// can never be the reason a healthy fleet stops accepting work.
//
// Integration tests — need Postgres (scripts/dev/dev.sh go-test-db).

// testVeto is the tuning these tests use: a 1024 MB floor, a 1024 MB per-session
// in-flight debit, and a 20 s freshness window — the production defaults.
func testVeto() VramAdmission {
	return VramAdmission{MinFreeMB: 1024, InflightMB: 1024, StalenessSecs: 20}
}

// vetoStore builds a Store with the live-VRAM veto armed.
func vetoStore(pool *pgxpool.Pool, opts ...StoreOption) *Store {
	return NewStore(pool, append([]StoreOption{WithVramAdmission(testVeto())}, opts...)...)
}

// sampleVram writes a live telemetry sample onto a GPU, aged ageSecs into the
// past. This is exactly what the agentws ingest path writes (DB now() for the
// timestamp), so admission sees production-shaped data.
func sampleVram(t *testing.T, pool *pgxpool.Pool, gpuID string, used, free int32, ageSecs int) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE gpus SET vram_mb_used = $2, vram_mb_free = $3,
		                vram_sampled_at = now() - make_interval(secs => $4::int),
		                vram_sample_agent_ms = $5
		WHERE id::text = $1
	`, gpuID, used, free, ageSecs, time.Now().UnixMilli())
	must(t, err)
}

// clearVram returns a GPU to "never sampled" — what an agent reconnect does.
func clearVram(t *testing.T, pool *pgxpool.Pool, gpuID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE gpus SET vram_mb_used = NULL, vram_mb_free = NULL,
		                vram_sampled_at = NULL, vram_sample_agent_ms = NULL
		WHERE id::text = $1
	`, gpuID)
	must(t, err)
}

// --- the gate ---------------------------------------------------------------

// TestVetoAdmitsAboveFloorRejectsBelow is the core behaviour: with encode slots
// free, a GPU whose fresh sample clears the floor accepts the launch, and one
// whose sample is below it is refused with the retryable capacity_exhausted —
// persisting no session row.
func TestVetoAdmitsAboveFloorRejectsBelow(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 8) // slots are deliberately NOT the constraint
	setQuota(t, pool, s.userID, 20)
	ctx := context.Background()

	// Comfortably above the 1024 MB floor even after the first launch's debit.
	sampleVram(t, pool, s.gpuID, 8192, 8192, 0)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("launch above the floor: %v", err)
	}

	// Now the GPU reports 512 MB free — below the floor with nothing in flight.
	sampleVram(t, pool, s.gpuID, 15872, 512, 0)
	before := countSessions(t, pool)
	_, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("launch below the floor: got %v want ErrCapacityExhausted", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("vetoed launch persisted a row: %d → %d", before, after)
	}

	// And it is diagnosable: the error carries the GPU and the numbers, so a
	// misconfigured floor is not an unexplainable 503 (review finding #10).
	var veto *VramVetoRejection
	if !errors.As(err, &veto) {
		t.Fatalf("veto rejection is not diagnosable: %T %v", err, err)
	}
	if len(veto.Candidates) != 1 {
		t.Fatalf("veto diagnosis: %d candidates, want 1", len(veto.Candidates))
	}
	c := veto.Candidates[0]
	if c.GPUID != s.gpuID || c.VramMBFree == nil || *c.VramMBFree != 512 {
		t.Fatalf("veto diagnosis: gpu=%s free=%v, want %s / 512", c.GPUID, c.VramMBFree, s.gpuID)
	}
	if c.SampledAt == nil || c.SampleAgeMs == nil {
		t.Fatalf("veto diagnosis missing sample provenance: %+v", c)
	}
	if veto.Veto.MinFreeMB != 1024 {
		t.Fatalf("veto diagnosis floor = %d, want 1024", veto.Veto.MinFreeMB)
	}
	// It still unwraps to the contract error, so status mapping is unchanged.
	if !errors.Is(veto, ErrCapacityExhausted) {
		t.Fatal("VramVetoRejection must unwrap to ErrCapacityExhausted")
	}
}

// TestVetoFailsOpen covers ALL FIVE abstain paths (#383 §2 property 2, §7.1).
// Unknown, stale, or structurally-unsatisfiable telemetry must never brick the
// fleet: an nvidia-smi hiccup, an un-upgraded agent, or an APU whose whole
// carve-out is smaller than the floor all fall back to slots-only admission.
//
// Every case seeds a GPU that WOULD be vetoed if the veto engaged, then asserts
// the launch is admitted anyway.
func TestVetoFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int
		// arrange makes the GPU look vetoable-but-unknowable.
		arrange func(t *testing.T, pool *pgxpool.Pool, gpuID string)
		// store overrides the veto tuning for the "kill switch" case.
		store func(pool *pgxpool.Pool) *Store
	}{
		{
			name:  "never sampled (NULL columns)",
			total: 16384,
			// An un-upgraded agent, or one whose sampler has produced nothing yet.
			arrange: func(*testing.T, *pgxpool.Pool, string) {},
		},
		{
			name:  "NULL sampled_at with a number present",
			total: 16384,
			arrange: func(t *testing.T, pool *pgxpool.Pool, gpuID string) {
				sampleVram(t, pool, gpuID, 16000, 1, 0)
				_, err := pool.Exec(context.Background(),
					`UPDATE gpus SET vram_sampled_at = NULL WHERE id::text = $1`, gpuID)
				must(t, err)
			},
		},
		{
			name:  "stale sample",
			total: 16384,
			// The sampler died; the last thing it said was "full". Slowly
			// strangling the host because of that is exactly what §2 forbids.
			arrange: func(t *testing.T, pool *pgxpool.Pool, gpuID string) {
				sampleVram(t, pool, gpuID, 16383, 1, 120) // 120 s > the 20 s window
			},
		},
		{
			name: "floor exceeds the card's whole pool (AMD APU carve-out)",
			// hermes is a Renoir APU: mem_info_vram_total is the BIOS UMA
			// carve-out, not a real pool — most of a session's memory is
			// GTT-backed. Acting on it would permanently veto the host
			// (review finding #1). Abstaining structurally beats APU detection.
			total: 1024, // == the floor
			arrange: func(t *testing.T, pool *pgxpool.Pool, gpuID string) {
				sampleVram(t, pool, gpuID, 1024, 0, 0) // reports ZERO free
			},
		},
		{
			name:  "kill switch: min_free = 0 omits the clause entirely",
			total: 16384,
			arrange: func(t *testing.T, pool *pgxpool.Pool, gpuID string) {
				sampleVram(t, pool, gpuID, 16384, 0, 0)
			},
			store: func(pool *pgxpool.Pool) *Store {
				return NewStore(pool, WithVramAdmission(VramAdmission{MinFreeMB: 0, InflightMB: 1024, StalenessSecs: 20}))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			store := vetoStore(pool)
			if tc.store != nil {
				store = tc.store(pool)
			}
			s := seedCap(t, pool, tc.total, 4)
			tc.arrange(t, pool, s.gpuID)

			if _, err := store.ScheduleAndCreate(context.Background(), launchParams(s)); err != nil {
				t.Fatalf("veto must ABSTAIN here, got %v", err)
			}
		})
	}
}

// TestVetoReconnectDoesNotAdmitBlock closes the loop the agentws ingest tests
// open (review finding #4): a "GPU is full" sample taken 5 s before an agent
// crash must not keep blocking launches through the whole staleness window after
// the agent comes back. The reconnect path NULLs the telemetry; here we prove
// that NULLing is what unblocks admission.
func TestVetoReconnectDoesNotAdmitBlock(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 16383, 1, 0) // pre-crash: full
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("full GPU: got %v want ErrCapacityExhausted", err)
	}

	clearVram(t, pool, s.gpuID) // what reconnectHost/enrollHost/upsertCapacity do
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("post-reconnect launch blocked by pre-crash data: %v", err)
	}
}

// --- the in-flight debit ----------------------------------------------------

// TestVetoInflightDebit: the sample lags reality twice over — by the sampling
// interval, and by however long a session takes to actually allocate. Launches
// inside one grace window are debited so a burst cannot all be admitted against
// the same stale "plenty free" reading (review finding #3).
//
// Sample says 4096 MB free, debit is 1024 MB/session, floor is 1024 MB:
// four launches fit (4096 − 4×1024 = 0 on the FIFTH, which fails ≥ 1024).
func TestVetoInflightDebit(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 20) // slots are not the constraint
	setQuota(t, pool, s.userID, 50)
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 12288, 4096, 0)

	admitted := make([]Session, 0, 4)
	for i := 0; i < 4; i++ {
		sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
		if err != nil {
			t.Fatalf("launch %d inside the grace window: %v", i+1, err)
		}
		admitted = append(admitted, sess)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("5th launch (debit exceeds the sampled free figure): got %v want ErrCapacityExhausted", err)
	}

	// A LONG-RUNNING session past its grace window is NOT double-counted: its
	// allocation is already visible in the sample, so debiting it again would
	// charge the same memory twice and shrink the fleet's usable capacity over
	// time. Age one session's started_at well before the grace window opens.
	if _, err := store.Transition(ctx, admitted[0].ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}
	if _, err := store.Transition(ctx, admitted[0].ID, StateRunning, nil, nil); err != nil {
		t.Fatalf("→ running: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sessions SET started_at = (SELECT vram_sampled_at FROM gpus WHERE id::text = $2) - interval '60 seconds'
		WHERE id::text = $1`, admitted[0].ID, s.gpuID); err != nil {
		t.Fatalf("age started_at: %v", err)
	}

	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("launch after a session aged out of the debit: %v (it is being double-counted)", err)
	}
}

// TestVetoDebitCountsStopping: a `stopping` pipeline is still draining Vulkan
// image refs (migration 0029), so it holds real VRAM even though activeStatesSQL
// excludes it from RESERVATIONS. Reservations and residency are different
// questions, and the debit asks the residency one.
func TestVetoDebitCountsStopping(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 20)
	setQuota(t, pool, s.userID, 50)
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 14336, 2048, 0)

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	// 2048 − 1×1024 = 1024 ≥ 1024, so a second launch fits right now.
	second, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("second launch: %v", err)
	}
	// Drive both into stopping: they release their SLOT reservations, but they
	// still occupy memory, so the debit must keep counting them.
	for _, id := range []string{sess.ID, second.ID} {
		if _, err := store.Transition(ctx, id, StateStopping, nil, nil); err != nil {
			t.Fatalf("→ stopping: %v", err)
		}
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("launch against two draining pipelines: got %v want ErrCapacityExhausted", err)
	}
}

// --- rejection classification ----------------------------------------------

// TestFloorAboveTotalStillClassifiesAsCapacityExhausted — the hermes case,
// live-reproduced 2026-07-26. A Renoir APU reports a 512 MB UMA carve-out
// against the 1024 MB default floor.
//
// An earlier revision of this test asserted ErrNoHostAvailable here, following
// review finding #9 ("a floor above every total can never be satisfied, so
// don't tell the client to retry"). That was wrong, and the harness caught it:
// finding #1's structural abstain means a GPU whose total is at or below the
// floor is never vetoed at all, so it IS servable. Reporting no_host_available
// turned ordinary encode-slot exhaustion on a healthy host into a
// non-retryable "the fleet cannot do this".
//
// The two findings contradicted each other; the abstain wins, and #9's scenario
// is unreachable by construction.
func TestFloorAboveTotalStillClassifiesAsCapacityExhausted(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool) // floor 1024
	s := seedCap(t, pool, 512, 1)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// The structural abstain (total <= floor) admits the first launch — the pool
	// is not measurable, so the veto keeps its hands off.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("first launch (structural abstain): %v", err)
	}
	// Slots are now gone. That is a transient, retryable condition: the small
	// VRAM total must not make it look permanent.
	_, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("slot exhaustion on a small-VRAM GPU: got %v want ErrCapacityExhausted", err)
	}
	if errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("slot exhaustion must stay retryable, got ErrNoHostAvailable")
	}
}

// --- bypass -----------------------------------------------------------------

// TestConsoleLaunchBypassesVetoAndCertDoesNot (review finding #11). The console
// drives the operator's local physical display and a launch failure feeds a
// crash-loop backoff that gives up after six fast failures — a transient VRAM
// veto would kill the local console semi-permanently with only an error log. The
// certification bench keeps the veto: it is pinned to one host, and a genuinely
// VRAM-pressured host is a real certification finding.
func TestConsoleLaunchBypassesVetoAndCertDoesNot(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 16383, 1, 0) // the GPU is out of memory

	// A normal launch is refused...
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("normal launch on a full GPU: got %v want ErrCapacityExhausted", err)
	}
	// ...and so is a pinned cert-bench-shaped launch, which leaves SkipVramVeto
	// at its zero value exactly as launchCertCell does.
	certLike := launchParams(s)
	certLike.PinHostID = s.hostID
	if _, err := store.ScheduleAndCreate(ctx, certLike); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("cert-bench launch on a full GPU: got %v want ErrCapacityExhausted (the bench must NOT bypass)", err)
	}

	// The console path bypasses it and comes up.
	sessID, err := coord.LaunchConsoleSession(ctx, s.hostID, s.userID, s.appID, "dual_output", 1280, 720, 60)
	if err != nil {
		t.Fatalf("console auto-start must bypass the veto: %v", err)
	}
	if sessID == "" {
		t.Fatal("console auto-start returned no session id")
	}

	// Encode slots still gate the console: only the advisory VRAM veto is skipped.
	// host-2 has a GPU with ZERO encode slots, and the console launch is pinned
	// to it, so the reservation has nowhere to go.
	noSlots, _ := seedSecondHost(t, pool, 16384, 0)
	if _, err := coord.LaunchConsoleSession(ctx, noSlots, s.userID, s.appID, "dual_output", 1280, 720, 60); err == nil {
		t.Fatal("console auto-start bypassed the ENCODE SLOT reservation too")
	}
}

// --- placement --------------------------------------------------------------

// TestLocalityPlacementSurvivesVetoArgs is review finding #7's regression guard.
// policyOrderSQL used to hard-code $3/$4 for the locality subquery, "after the
// fixed $1=vram_mb and $2=encode_slots". #383 changed that layout, and adding the
// veto changes it again depending on whether the veto is armed — a hard-coded $3
// would send an int into `$3::uuid` and error on EVERY launch under
// QUASAR_PLACEMENT_POLICY=locality. Both layouts are exercised.
func TestLocalityPlacementSurvivesVetoArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []StoreOption
	}{
		{"veto armed (3 fixed args)", []StoreOption{WithPlacementPolicy(PolicyLocality), WithVramAdmission(testVeto())}},
		{"veto off (1 fixed arg)", []StoreOption{WithPlacementPolicy(PolicyLocality)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool, tc.opts...)
			s := seed(t, pool, 4)
			h2, g2 := seedSecondHost(t, pool, 16384, 8) // MORE headroom than the home host
			managedApp := seedManagedApp(t, pool, `{}`)
			setQuota(t, pool, s.userID, 10)
			ctx := context.Background()

			// Both GPUs report plenty free, so the veto abstains on merit and only
			// the ordering decides.
			sampleVram(t, pool, s.gpuID, 2048, 14336, 0)
			sampleVram(t, pool, g2, 2048, 14336, 0)
			seedHome(t, pool, s.userID, managedApp, s.hostID)

			sess, err := store.ScheduleAndCreate(ctx, managedLaunchParams(s, managedApp))
			if err != nil {
				t.Fatalf("locality launch: %v", err)
			}
			if sess.HostID == nil || *sess.HostID != s.hostID {
				t.Fatalf("locality placed on %v, want the home host %s (host-2 is %s)", sess.HostID, s.hostID, h2)
			}
		})
	}
}

// TestSpreadPrefersFreshLiveVram (review finding #14): with encode slots tied,
// ranking falls to live free VRAM — but ONLY when the sample is fresh. A stale
// sample showing a huge free figure must not promote a GPU that is actually full.
func TestSpreadPrefersFreshLiveVram(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 4) // host-1: fresh, modest free
	h2, g2 := seedSecondHost(t, pool, 16384, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 8192, 8192, 0) // fresh, 8 GB free
	sampleVram(t, pool, g2, 1, 16383, 600)      // STALE, claims 16 GB free

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if sess.HostID == nil || *sess.HostID != s.hostID {
		t.Fatalf("ranked on a STALE sample: placed on %v, want host-1 (%s); host-2 is %s", sess.HostID, s.hostID, h2)
	}
}

// --- pick / re-check agreement ----------------------------------------------

// TestPickAndRecheckAgree is review finding #8's guard. ScheduleAndCreate picks a
// candidate with an UNLOCKED read, then re-derives the same predicate under the
// GPU's advisory lock. If those two predicates ever diverge — a transcription
// slip in the placeholder numbering, a clause added to one and not the other —
// the loop re-picks the same GPU and rejects it forever: 50 attempts, 50 advisory
// locks, and a spurious `capacity_exhausted` on a completely idle fleet, with
// nothing in the logs to say why.
//
// vramVetoSQL exists precisely so there is one renderer; this asserts the
// observable consequence, so the guard survives future edits.
func TestPickAndRecheckAgree(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 4096, 12288, 0)

	var maxAttempt int
	attemptObserver = func(attempt int) {
		if attempt > maxAttempt {
			maxAttempt = attempt
		}
	}
	t.Cleanup(func() { attemptObserver = nil })

	// A handful of sequential launches (zero concurrency ⇒ nothing can race).
	for i := 0; i < 3; i++ {
		if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
			t.Fatalf("launch %d: %v", i+1, err)
		}
	}
	if maxAttempt != 0 {
		t.Fatalf("scheduleAttempt retried (max attempt index %d) with no concurrency — "+
			"the candidate predicate and the under-lock re-check DISAGREE", maxAttempt)
	}

	// Same assertion on the rejection path: a vetoed launch must be rejected on
	// the FIRST attempt, not after burning the retry budget.
	sampleVram(t, pool, s.gpuID, 16000, 100, 0)
	maxAttempt = 0
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("vetoed launch: got %v want ErrCapacityExhausted", err)
	}
	if maxAttempt != 0 {
		t.Fatalf("a vetoed launch burned %d retries; the veto must reject at the PICK, not the re-check", maxAttempt+1)
	}
}

// --- swap fit ---------------------------------------------------------------

// TestSwapFitIsSlotsOnly (#383 §5): the swap-fit rule dropped its VRAM half.
// Sessions now reserve 0 MB, so comparing app.DefaultVramMB against
// sess.ReservedVram would reject EVERY swap into an app with any declared VRAM
// at all — which is every app in the catalog.
func TestSwapFitIsSlotsOnly(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 4)
	disp := newFakeDispatcher(true)
	coord := newTestCoordinator(t, store, disp, testLogger())
	ctx := context.Background()

	sampleVram(t, pool, s.gpuID, 4096, 12288, 0)
	sess := runningSession(t, store, s) // reserves 1 slot, 0 MB
	if sess.ReservedVram != 0 {
		t.Fatalf("session reserved %d MB declared VRAM, want 0", sess.ReservedVram)
	}

	// Declared VRAM far above anything the session "reserved" — must NOT matter.
	hungryApp := insertApp(t, pool, "hungryApp", 65536, 1)
	if _, err := coord.Swap(ctx, sess.ID, hungryApp); err != nil {
		t.Fatalf("swap rejected on declared VRAM: %v (the fit rule is slots-only)", err)
	}

	// Slots still bind.
	sess2 := runningSession(t, store, s)
	wideApp := insertApp(t, pool, "wideApp", 0, 2)
	if _, err := coord.Swap(ctx, sess2.ID, wideApp); !errors.Is(err, ErrSwapExceedsReservation) {
		t.Fatalf("swap needing 2 slots against 1 reserved: got %v want ErrSwapExceedsReservation", err)
	}
}
