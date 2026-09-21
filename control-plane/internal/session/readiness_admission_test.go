package session

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The readiness gate at admission (#262, control-api.md "Evidence-gated
// readiness"). Admission never parses a report: it reads hosts.readiness_block_host,
// hosts.readiness_block_homes and gpus.readiness_blocked, and only while
// hosts.readiness_reported_at is inside the staleness window. These tests set
// those columns directly, as the verdict writer does.

// reportReadiness marks a host as having reported ageSecs ago with the given
// host-level blocks. ageSecs < 0 means "never reported" (readiness NULL).
func reportReadiness(t *testing.T, pool *pgxpool.Pool, hostID string, ageSecs int, blockHost, blockHomes bool) {
	t.Helper()
	ctx := context.Background()
	if ageSecs < 0 {
		_, err := pool.Exec(ctx, `UPDATE hosts SET readiness = NULL, readiness_reported_at = NULL,
			readiness_block_host = $2, readiness_block_homes = $3 WHERE id::text = $1`, hostID, blockHost, blockHomes)
		must(t, err)
		return
	}
	_, err := pool.Exec(ctx, `UPDATE hosts SET readiness = '[]'::jsonb,
		readiness_reported_at = now() - make_interval(secs => $2::int),
		readiness_block_host = $3, readiness_block_homes = $4 WHERE id::text = $1`,
		hostID, ageSecs, blockHost, blockHomes)
	must(t, err)
}

func blockGPU(t *testing.T, pool *pgxpool.Pool, gpuID string, blocked bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE gpus SET readiness_blocked = $2 WHERE id::text = $1`, gpuID, blocked)
	must(t, err)
}

func addGPU(t *testing.T, pool *pgxpool.Pool, hostID string, index, encodeSlots int) string {
	t.Helper()
	var id string
	must(t, pool.QueryRow(context.Background(), `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, $2, 16384, $3) RETURNING id::text`, hostID, index, encodeSlots).Scan(&id))
	return id
}

func release(t *testing.T, pool *pgxpool.Pool, sess Session) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE sessions SET state='stopped' WHERE id::text=$1`, sess.ID)
	must(t, err)
}

// watchAttempts fails the test if any launch in it needed a retry: with no
// concurrency, a retry means the pick and the under-lock re-check disagree.
func watchAttempts(t *testing.T) func(what string) {
	t.Helper()
	var maxAttempt int
	attemptObserver = func(attempt int) {
		if attempt > maxAttempt {
			maxAttempt = attempt
		}
	}
	t.Cleanup(func() { attemptObserver = nil })
	return func(what string) {
		t.Helper()
		if maxAttempt != 0 {
			t.Fatalf("%s: %d retries with no concurrency — the readiness filter is not the same in the "+
				"candidate query and the re-check", what, maxAttempt)
		}
		maxAttempt = 0
	}
}

func sessionCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sessions`).Scan(&n))
	return n
}

// TestReadinessBlockedHostIsSkippedAndAnotherChosen: host-1 has far more free
// slots, so the spread policy picks it every time unless the gate excludes it.
func TestReadinessBlockedHostIsSkippedAndAnotherChosen(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	_, gpuB := addHost(t, pool, "host-2", 1)
	noRetries := watchAttempts(t)
	ctx := context.Background()

	reportReadiness(t, pool, s.hostID, 5, true, false)
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch with one blocked host and one ready host: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != gpuB {
		t.Fatalf("placed on %v, want the ready host's GPU %s", sess.GPUID, gpuB)
	}
	noRetries("fallback to the ready host")

	// The ready host is now full; the blocked one still has 8 free slots. The
	// caller's remedy is to retry, so this is exhaustion, not host_not_ready.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("ready host full + other host blocked: got %v, want ErrCapacityExhausted", err)
	}
	if errors.Is(err, ErrHostNotReady) {
		t.Fatal("unreachable")
	}
	noRetries("refusal with the ready host full")

	// Unblocked, the big host takes the launch again.
	reportReadiness(t, pool, s.hostID, 5, false, false)
	sess, err = store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil || sess.GPUID == nil || *sess.GPUID != s.gpuID {
		t.Fatalf("after the block cleared: gpu=%v err=%v, want %s", sess.GPUID, err, s.gpuID)
	}
}

// TestHostNotReadyOnlyWhenReadinessIsTheSoleReason is the classification matrix.
func TestHostNotReadyOnlyWhenReadinessIsTheSoleReason(t *testing.T) {
	type fleet func(t *testing.T, pool *pgxpool.Pool, s seedIDs)
	blockedOnly := func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
		reportReadiness(t, pool, s.hostID, 5, true, false)
	}
	cases := []struct {
		name  string
		fleet fleet
		tweak func(p *CreateParams, s seedIDs)
		want  error
	}{
		{"the only host is blocked", blockedOnly, nil, ErrHostNotReady},
		{"the only host's only GPU is blocked", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			reportReadiness(t, pool, s.hostID, 5, false, false)
			blockGPU(t, pool, s.gpuID, true)
		}, nil, ErrHostNotReady},
		{"every host is blocked", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			hostB, _ := addHost(t, pool, "host-2", 4)
			reportReadiness(t, pool, hostB, 5, true, false)
		}, nil, ErrHostNotReady},
		{"one host blocked, the other offline", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			hostB, _ := addHost(t, pool, "host-2", 4)
			_, err := pool.Exec(context.Background(), `UPDATE hosts SET status='offline' WHERE id::text=$1`, hostB)
			must(t, err)
		}, nil, ErrHostNotReady},
		{"one host blocked, the other draining", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			hostB, _ := addHost(t, pool, "host-2", 4)
			_, err := pool.Exec(context.Background(), `UPDATE hosts SET status='draining' WHERE id::text=$1`, hostB)
			must(t, err)
		}, nil, ErrHostNotReady},
		{"pinned to the blocked host while another is ready", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			addHost(t, pool, "host-2", 4)
		}, func(p *CreateParams, s seedIDs) { p.PinHostID = s.hostID }, ErrHostNotReady},

		{"nothing online", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			_, err := pool.Exec(context.Background(), `UPDATE hosts SET status='offline'`)
			must(t, err)
		}, nil, ErrNoHostAvailable},
		{"blocked host could never serve the request anyway", blockedOnly,
			func(p *CreateParams, s seedIDs) { p.NeedEncodeSlots = 64 }, ErrNoHostAvailable},
		{"blocked host is also full", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			_, err := pool.Exec(context.Background(), `UPDATE gpus SET encode_slots_total = 1 WHERE id::text=$1`, s.gpuID)
			must(t, err)
			if _, err := NewStore(pool).ScheduleAndCreate(context.Background(), launchParams(s)); err != nil {
				t.Fatalf("fill the host: %v", err)
			}
			blockedOnly(t, pool, s)
		}, nil, ErrCapacityExhausted},
		{"one host blocked, the other ready but full", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			blockedOnly(t, pool, s)
			hostB, _ := addHost(t, pool, "host-2", 1)
			p := launchParams(s)
			p.PinHostID = hostB
			if _, err := NewStore(pool).ScheduleAndCreate(context.Background(), p); err != nil {
				t.Fatalf("fill host-2: %v", err)
			}
		}, nil, ErrCapacityExhausted},
		{"ready host with no block is full (no readiness involved)", func(t *testing.T, pool *pgxpool.Pool, s seedIDs) {
			_, err := pool.Exec(context.Background(), `UPDATE gpus SET encode_slots_total = 1 WHERE id::text=$1`, s.gpuID)
			must(t, err)
			reportReadiness(t, pool, s.hostID, 5, false, false)
			if _, err := NewStore(pool).ScheduleAndCreate(context.Background(), launchParams(s)); err != nil {
				t.Fatalf("fill the host: %v", err)
			}
		}, nil, ErrCapacityExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool)
			s := seed(t, pool, 4)
			setQuota(t, pool, s.userID, 20)
			tc.fleet(t, pool, s)
			noRetries := watchAttempts(t)
			before := sessionCount(t, pool)

			p := launchParams(s)
			if tc.tweak != nil {
				tc.tweak(&p, s)
			}
			_, err := store.ScheduleAndCreate(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			for _, other := range []error{ErrHostNotReady, ErrNoHostAvailable, ErrCapacityExhausted} {
				if other != tc.want && errors.Is(err, other) {
					t.Fatalf("%v also matches %v; the three refusals must stay distinct", err, other)
				}
			}
			noRetries("the refusal")
			if after := sessionCount(t, pool); after != before {
				t.Fatalf("a refused launch persisted a session row (%d → %d)", before, after)
			}
		})
	}
}

// TestHostNotReadyBesideTheVramVeto: a blocked host with room plus a ready host
// the veto refuses is still exhaustion — the veto clears on its own.
func TestHostNotReadyBesideTheVramVeto(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 4)
	_, gpuB := addHost(t, pool, "host-2", 4)
	ctx := context.Background()

	reportReadiness(t, pool, s.hostID, 5, true, false)
	sampleVram(t, pool, s.gpuID, 4096, 12288, 0)
	sampleVram(t, pool, gpuB, 16000, 100, 0) // below the floor
	noRetries := watchAttempts(t)

	_, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrCapacityExhausted) || errors.Is(err, ErrHostNotReady) {
		t.Fatalf("blocked host + vetoed ready host: got %v, want ErrCapacityExhausted", err)
	}
	var veto *VramVetoRejection
	if !errors.As(err, &veto) || len(veto.Candidates) != 1 || veto.Candidates[0].GPUID != gpuB {
		t.Fatalf("the veto diagnostic must still name only the vetoed READY gpu, got %+v", veto)
	}
	noRetries("the refusal")

	// A blocked host the veto ALSO refuses: readiness is not the sole reason.
	sampleVram(t, pool, s.gpuID, 16000, 100, 0)
	_, err = pool.Exec(ctx, `UPDATE hosts SET status='offline' WHERE node_name='host-2'`)
	must(t, err)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("blocked AND vetoed: got %v, want ErrCapacityExhausted", err)
	}
}

// TestReadinessBlockedGPUIsSkippedOnATwoGPUHost: the gpu scope excludes one GPU,
// not its host.
func TestReadinessBlockedGPUIsSkippedOnATwoGPUHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8) // gpu index 0: far more room, so spread prefers it
	gpu1 := addGPU(t, pool, s.hostID, 1, 1)
	setQuota(t, pool, s.userID, 20)
	noRetries := watchAttempts(t)
	ctx := context.Background()

	reportReadiness(t, pool, s.hostID, 5, false, false)
	blockGPU(t, pool, s.gpuID, true)

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch with gpu0 blocked: %v", err)
	}
	if sess.GPUID == nil || *sess.GPUID != gpu1 {
		t.Fatalf("placed on %v, want the unblocked GPU %s", sess.GPUID, gpu1)
	}
	noRetries("skip the blocked GPU")

	// gpu1 (1 slot) is full; gpu0 has 8 free but is blocked. A ready GPU that is
	// full is exhaustion.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("unblocked GPU full: got %v, want ErrCapacityExhausted", err)
	}
	noRetries("refusal with the unblocked GPU full")

	blockGPU(t, pool, gpu1, true)
	release(t, pool, sess)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("both GPUs blocked: got %v, want ErrHostNotReady", err)
	}
}

// TestReadinessHomesScopeBlocksOnlyHomeMountingLaunches.
func TestReadinessHomesScopeBlocksOnlyHomeMountingLaunches(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 20)
	noRetries := watchAttempts(t)
	ctx := context.Background()

	reportReadiness(t, pool, s.hostID, 5, false, true)

	plain := launchParams(s)
	sess, err := store.ScheduleAndCreate(ctx, plain)
	if err != nil {
		t.Fatalf("a launch that mounts no home must ignore the homes scope: %v", err)
	}
	noRetries("plain launch")
	// Ended first: the per-(user, home) single-writer guard runs before placement.
	release(t, pool, sess)

	home := launchParams(s)
	home.ManagedHome = true
	if _, err := store.ScheduleAndCreate(ctx, home); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("home-mounting launch on a homes-blocked host: got %v, want ErrHostNotReady", err)
	}
	noRetries("home launch refusal")

	// With a second host whose homes are fine, the home launch goes there.
	_, gpuB := addHost(t, pool, "host-2", 1)
	sess, err = store.ScheduleAndCreate(ctx, home)
	if err != nil || sess.GPUID == nil || *sess.GPUID != gpuB {
		t.Fatalf("home launch fallback: gpu=%v err=%v, want %s", sess.GPUID, err, gpuB)
	}
	noRetries("home launch fallback")
}

// TestReadinessGateAbstainsOnAStaleOrAbsentReport: stale evidence must never
// strand a fleet. The derived columns still say "blocked"; only freshness differs.
func TestReadinessGateAbstainsOnAStaleOrAbsentReport(t *testing.T) {
	cases := []struct {
		name    string
		ageSecs int
		opts    []StoreOption
		want    error
	}{
		{"fresh: gated", 5, nil, ErrHostNotReady},
		{"just inside the default 60 s window: gated", 55, nil, ErrHostNotReady},
		{"past the default window: abstains", 65, nil, nil},
		{"an hour old: abstains", 3600, nil, nil},
		{"never reported: abstains", -1, nil, nil},
		{"QUASAR_READINESS_STALE_SECS widened: a 90 s old report gates", 90, []StoreOption{WithReadinessStaleSecs(120)}, ErrHostNotReady},
		{"QUASAR_READINESS_STALE_SECS narrowed: a 20 s old report abstains", 20, []StoreOption{WithReadinessStaleSecs(10)}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testDB(t)
			store := NewStore(pool, tc.opts...)
			s := seed(t, pool, 4)
			noRetries := watchAttempts(t)
			reportReadiness(t, pool, s.hostID, tc.ageSecs, true, true)
			blockGPU(t, pool, s.gpuID, true)

			p := launchParams(s)
			p.ManagedHome = true
			_, err := store.ScheduleAndCreate(context.Background(), p)
			if tc.want == nil && err != nil {
				t.Fatalf("the gate must abstain here, got %v", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			noRetries("launch")
		})
	}
}

// TestReadinessGateAndRecheckAgree extends TestPickAndRecheckAgree: every shape
// of the gate, accepted and refused, settles on the first attempt.
func TestReadinessGateAndRecheckAgree(t *testing.T) {
	pool := testDB(t)
	store := vetoStore(pool)
	s := seed(t, pool, 8)
	gpu1 := addGPU(t, pool, s.hostID, 1, 8)
	setQuota(t, pool, s.userID, 50)
	sampleVram(t, pool, s.gpuID, 4096, 12288, 0)
	sampleVram(t, pool, gpu1, 4096, 12288, 0)
	noRetries := watchAttempts(t)
	ctx := context.Background()

	steps := []struct {
		name                string
		age                 int
		host, homes, g0, g1 bool
		managedHome         bool
		want                error
	}{
		{"nothing blocked", 5, false, false, false, false, false, nil},
		{"gpu0 blocked", 5, false, false, true, false, false, nil},
		{"gpu1 blocked", 5, false, false, false, true, false, nil},
		{"both gpus blocked", 5, false, false, true, true, false, ErrHostNotReady},
		{"host blocked", 5, true, false, false, false, false, ErrHostNotReady},
		{"homes blocked, plain launch", 5, false, true, false, false, false, nil},
		{"homes blocked, home launch", 5, false, true, false, false, true, ErrHostNotReady},
		{"everything blocked but stale", 600, true, true, true, true, true, nil},
		{"everything blocked but never reported", -1, true, true, true, true, false, nil},
	}
	for _, st := range steps {
		reportReadiness(t, pool, s.hostID, st.age, st.host, st.homes)
		blockGPU(t, pool, s.gpuID, st.g0)
		blockGPU(t, pool, gpu1, st.g1)
		p := launchParams(s)
		p.ManagedHome = st.managedHome
		sess, err := store.ScheduleAndCreate(ctx, p)
		if st.want == nil {
			if err != nil {
				t.Fatalf("%s: %v", st.name, err)
			}
			if st.g0 && *sess.GPUID == s.gpuID || st.g1 && *sess.GPUID == gpu1 {
				if st.age == 5 {
					t.Fatalf("%s: placed on a blocked GPU", st.name)
				}
			}
			release(t, pool, sess)
		} else if !errors.Is(err, st.want) {
			t.Fatalf("%s: got %v, want %v", st.name, err, st.want)
		}
		noRetries(st.name)
	}
}

// TestSwapIsNotReadinessGated: a swap places nothing, so the gate does not apply
// — also when the host it runs on becomes blocked mid-session.
func TestSwapIsNotReadinessGated(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	coord := newTestCoordinator(t, store, newFakeDispatcher(true), testLogger())
	ctx := context.Background()

	sess := runningSession(t, store, s)
	reportReadiness(t, pool, s.hostID, 5, true, true)
	blockGPU(t, pool, s.gpuID, true)

	other := insertApp(t, pool, "otherApp", 1024, 1)
	if _, err := coord.Swap(ctx, sess.ID, other); err != nil {
		t.Fatalf("swap on a readiness-blocked host: %v (swap is not gated)", err)
	}
	// The same host refuses a NEW launch.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("new launch on the blocked host: got %v, want ErrHostNotReady", err)
	}
}

// TestRegisterToCapacityWindowRefusesThenAdmits pins the transition a
// reconnecting agent walks through: register leaves the host unplaceable
// (capacity_detection='unavailable', gpus.reported=false — agentws/store.go's
// markGPUsStaleAndClearVramSQL, enrollHost's upsert, and reconnectHostSQL all
// set this on register/reconnect) until its first capacity report lands. A
// launch inside that window must fail with ErrNoHostAvailable — retryable,
// distinct from ErrHostNotReady/ErrCapacityExhausted — and admit again once
// capacity is reported, honouring the readiness gate's own
// stale/absent-abstains rule.
func TestRegisterToCapacityWindowRefusesThenAdmits(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	registerWindow := func(t *testing.T) {
		t.Helper()
		// Mirrors agentws/store.go's register/reconnect SQL: capacity unknown
		// until the agent's first `capacity` message lands.
		_, err := pool.Exec(ctx, `UPDATE hosts SET capacity_detection = 'unavailable' WHERE id::text = $1`, s.hostID)
		must(t, err)
		_, err = pool.Exec(ctx, `UPDATE gpus SET reported = false WHERE host_id = $1`, s.hostID)
		must(t, err)
	}
	capacityReceived := func(t *testing.T) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE hosts SET capacity_detection = 'ok' WHERE id::text = $1`, s.hostID)
		must(t, err)
		_, err = pool.Exec(ctx, `UPDATE gpus SET reported = true WHERE host_id = $1`, s.hostID)
		must(t, err)
	}

	// (a) Register window: a readiness row left over from the host's prior
	// life is fresh and would normally gate (ErrHostNotReady) — but
	// capacity_detection='unavailable' makes totalsQuery fail first, so the
	// refusal must be ErrNoHostAvailable, not the readiness gate's error.
	registerWindow(t)
	reportReadiness(t, pool, s.hostID, 5, true, false)
	_, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("register window: got %v, want ErrNoHostAvailable", err)
	}
	if errors.Is(err, ErrHostNotReady) || errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("register window: %v must not also be ErrHostNotReady/ErrCapacityExhausted", err)
	}

	// (b) Capacity received, readiness never reported (NULL/NULL): the gate
	// fails open on an absent report — the transition's whole point is that
	// this now admits.
	capacityReceived(t)
	reportReadiness(t, pool, s.hostID, -1, true, false)
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("capacity received, readiness absent: got %v, want success", err)
	}
	release(t, pool, sess)

	// (c) Capacity received, a 90s-old blocking report: stale fails open too
	// (default staleness window is 60s).
	reportReadiness(t, pool, s.hostID, 90, true, false)
	sess, err = store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("capacity received, readiness stale: got %v, want success", err)
	}
	release(t, pool, sess)

	// (d) Capacity received, a fresh 5s-old blocking report: the gate now
	// applies for real.
	reportReadiness(t, pool, s.hostID, 5, true, false)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrHostNotReady) {
		t.Fatalf("capacity received, readiness fresh: got %v, want ErrHostNotReady", err)
	}
}

// TestNoHostRejectionCarriesFleetCounts pins the diagnostic attached to a
// no_host_available refusal (the same register window as
// TestRegisterToCapacityWindowRefusesThenAdmits): still errors.Is-compatible
// with ErrNoHostAvailable, and its counts describe the register window
// correctly.
func TestNoHostRejectionCarriesFleetCounts(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Mirrors agentws/store.go's register/reconnect SQL (markGPUsStaleAndClearVramSQL,
	// enrollHost's upsert, reconnectHostSQL): capacity unknown until the
	// agent's first `capacity` message lands.
	_, err := pool.Exec(ctx, `UPDATE hosts SET capacity_detection = 'unavailable', last_registered_at = now() WHERE id::text = $1`, s.hostID)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE gpus SET reported = false WHERE host_id = $1`, s.hostID)
	must(t, err)

	_, err = store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("got %v, want ErrNoHostAvailable", err)
	}
	var rej *NoHostRejection
	if !errors.As(err, &rej) {
		t.Fatalf("got %v, want a *NoHostRejection", err)
	}
	if rej.OnlineHosts != 1 {
		t.Fatalf("online_hosts: got %d, want 1", rej.OnlineHosts)
	}
	if rej.HostsCapacityNotOK != 1 {
		t.Fatalf("hosts_capacity_not_ok: got %d, want 1", rej.HostsCapacityNotOK)
	}
	if rej.GPUsUnreported != 1 {
		t.Fatalf("gpus_unreported: got %d, want 1", rej.GPUsUnreported)
	}
	if rej.HostsRecentlyRegistered != 1 {
		t.Fatalf("hosts_recently_registered: got %d, want 1", rej.HostsRecentlyRegistered)
	}
}
