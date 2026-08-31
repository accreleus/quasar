package agentws

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Live free-VRAM telemetry ingest (#383 §3.3). These prove the four properties
// the admission veto depends on being able to trust: the sample lands, an absent
// key never clobbers, a zombie connection cannot rewind it, and an implausible
// reading is stored as UNKNOWN rather than acted on. Integration tests — need
// Postgres (scripts/dev/dev.sh go-test-db).

// vramRow is the stored telemetry for one GPU. Every field is nullable because
// NULL is the load-bearing "unknown" value.
type vramRow struct {
	used      *int32
	free      *int32
	sampledAt *time.Time
	agentMs   *int64
}

func readVram(t *testing.T, pool *pgxpool.Pool, hostID string, index int) vramRow {
	t.Helper()
	var r vramRow
	if err := pool.QueryRow(context.Background(), `
		SELECT vram_mb_used, vram_mb_free, vram_sampled_at, vram_sample_agent_ms
		FROM gpus WHERE host_id::text = $1 AND index = $2
	`, hostID, index).Scan(&r.used, &r.free, &r.sampledAt, &r.agentMs); err != nil {
		t.Fatalf("read vram telemetry (index %d): %v", index, err)
	}
	return r
}

// seedGPURow inserts one reported GPU directly (no capacity handshake needed).
func seedGPURow(t *testing.T, pool *pgxpool.Pool, hostID string, index, vramMBTotal int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO gpus (host_id, index, vendor, model, vram_mb_total, encode_slots_total, reported)
		VALUES ($1::uuid, $2, 'nvidia', 'test-gpu', $3, 2, true)
	`, hostID, index, vramMBTotal); err != nil {
		t.Fatalf("seed gpu %d: %v", index, err)
	}
}

func i32(v int32) *int32 { return &v }

// TestVramSamplePersistsAndAbsentKeyDoesNotClobber: a heartbeat carrying samples
// writes used/free/sampled_at/agent_ms; a subsequent heartbeat with NO gpu_vram
// key (an un-upgraded agent) leaves the stored values exactly as they were. A
// clobber-to-NULL here would be survivable (the veto abstains) but would make
// the admin UI flicker to "unknown" on every mixed-version fleet heartbeat.
func TestVramSamplePersistsAndAbsentKeyDoesNotClobber(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	seedGPURow(t, pool, hostID, 0, 16384)
	ctx := context.Background()

	before := time.Now().Add(-time.Second)
	if err := s.applyVramSamples(ctx, hostID, 1000, []GPUVramSample{
		{Index: 0, UsedMB: i32(2048), FreeMB: i32(14336)},
	}); err != nil {
		t.Fatalf("apply sample: %v", err)
	}

	got := readVram(t, pool, hostID, 0)
	if got.used == nil || *got.used != 2048 || got.free == nil || *got.free != 14336 {
		t.Fatalf("stored sample: used=%v free=%v, want 2048/14336", got.used, got.free)
	}
	if got.agentMs == nil || *got.agentMs != 1000 {
		t.Fatalf("stored agent_ms: %v, want 1000", got.agentMs)
	}
	// The timestamp must be the DB clock, not the agent's ts_unix_ms (which here
	// is 1000 ms after the epoch — 1970). Anything derived from the agent clock
	// would silently kill the in-flight debit, which compares this against
	// sessions.started_at (also DB now()).
	if got.sampledAt == nil || got.sampledAt.Before(before) {
		t.Fatalf("vram_sampled_at = %v, want a DB now() after %v (NOT the agent clock)", got.sampledAt, before)
	}
	stamped := *got.sampledAt

	// A heartbeat with no gpu_vram key at all: nil slice ⇒ no-op.
	if err := s.applyVramSamples(ctx, hostID, 2000, nil); err != nil {
		t.Fatalf("apply absent samples: %v", err)
	}
	after := readVram(t, pool, hostID, 0)
	if after.used == nil || *after.used != 2048 || after.free == nil || *after.free != 14336 {
		t.Fatalf("absent gpu_vram clobbered the sample: used=%v free=%v", after.used, after.free)
	}
	if after.sampledAt == nil || !after.sampledAt.Equal(stamped) {
		t.Fatalf("absent gpu_vram moved vram_sampled_at: %v → %v", stamped, after.sampledAt)
	}
	if after.agentMs == nil || *after.agentMs != 1000 {
		t.Fatalf("absent gpu_vram advanced agent_ms to %v", after.agentMs)
	}
}

// TestVramSampleMonotonicGuard: an out-of-order write carrying an OLDER agent
// timestamp is ignored (review finding #5). `Registry.add` displaces a stale
// connection without closing its socket, so a zombie read loop can keep emitting
// heartbeats for up to the 20 s read deadline — and its pre-restart numbers,
// stamped with a fresh DB now(), would look authoritative.
func TestVramSampleMonotonicGuard(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	seedGPURow(t, pool, hostID, 0, 16384)
	ctx := context.Background()

	mustApply := func(agentMs int64, free int32) {
		t.Helper()
		if err := s.applyVramSamples(ctx, hostID, agentMs, []GPUVramSample{
			{Index: 0, UsedMB: i32(16384 - free), FreeMB: i32(free)},
		}); err != nil {
			t.Fatalf("apply sample (agentMs=%d): %v", agentMs, err)
		}
	}

	mustApply(5000, 8000) // the live connection
	mustApply(4000, 100)  // the zombie, arriving late with pre-restart data

	got := readVram(t, pool, hostID, 0)
	if got.free == nil || *got.free != 8000 {
		t.Fatalf("zombie write won: free=%v, want 8000 (the newer sample)", got.free)
	}
	if got.agentMs == nil || *got.agentMs != 5000 {
		t.Fatalf("agent_ms rewound to %v, want 5000", got.agentMs)
	}

	// A strictly newer sample still lands — the guard is monotonic, not a latch.
	mustApply(6000, 42)
	if got := readVram(t, pool, hostID, 0); got.free == nil || *got.free != 42 {
		t.Fatalf("newer sample rejected: free=%v, want 42", got.free)
	}

	// An equal timestamp is a duplicate delivery, not new information.
	mustApply(6000, 999)
	if got := readVram(t, pool, hostID, 0); got.free == nil || *got.free != 42 {
		t.Fatalf("equal-timestamp replay accepted: free=%v, want 42", got.free)
	}
}

// TestVramSampleImplausibleStoresNull: a driver glitch must be stored as UNKNOWN
// (NULL ⇒ the veto abstains), never as a number admission would act on
// (review finding #16). The timestamps still advance: we DID hear from the agent,
// it just told us nothing usable.
func TestVramSampleImplausibleStoresNull(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	const total = 8192
	seedGPURow(t, pool, hostID, 0, total)
	ctx := context.Background()

	for i, tc := range []struct {
		name       string
		used, free *int32
	}{
		{"negative free", i32(100), i32(-1)},
		{"negative used", i32(-5), i32(100)},
		{"free exceeds total", i32(0), i32(total + 1)},
		{"used+free exceeds total by more than 5%", i32(total), i32(total)},
		// A faulted / falling-off-the-bus GPU prints `0, 0`, which satisfies every
		// other clause. Stored as a reading it is worse than wrong: free = 0 fails
		// the veto for every launch, and because the driver keeps emitting it the
		// sample never goes stale, so the GPU is refused forever behind a
		// "retryable" capacity_exhausted. Unknown abstains; that is fail-open.
		{"both halves zero on a card with a positive total", i32(0), i32(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Land a good sample first so a NULL result cannot be a false pass.
			if err := s.applyVramSamples(ctx, hostID, int64(1000+i*10), []GPUVramSample{
				{Index: 0, UsedMB: i32(1), FreeMB: i32(2)},
			}); err != nil {
				t.Fatalf("seed good sample: %v", err)
			}
			if got := readVram(t, pool, hostID, 0); got.free == nil {
				t.Fatal("seed good sample did not store")
			}

			if err := s.applyVramSamples(ctx, hostID, int64(1001+i*10), []GPUVramSample{
				{Index: 0, UsedMB: tc.used, FreeMB: tc.free},
			}); err != nil {
				t.Fatalf("apply implausible sample: %v", err)
			}
			got := readVram(t, pool, hostID, 0)
			if got.used != nil || got.free != nil {
				t.Fatalf("implausible sample stored: used=%v free=%v, want NULL/NULL", got.used, got.free)
			}
			if got.sampledAt == nil {
				t.Fatal("implausible sample should still stamp vram_sampled_at (we did hear from the agent)")
			}
		})
	}

	// A within-tolerance overlap (used+free slightly over total, e.g. a reserved
	// carve-out counted twice) is plausible — 5% slack, per spec.
	if err := s.applyVramSamples(ctx, hostID, 9000, []GPUVramSample{
		{Index: 0, UsedMB: i32(4096), FreeMB: i32(4200)},
	}); err != nil {
		t.Fatalf("apply near-total sample: %v", err)
	}
	if got := readVram(t, pool, hostID, 0); got.free == nil || *got.free != 4200 {
		t.Fatalf("within-tolerance sample rejected: free=%v, want 4200", got.free)
	}
}

// TestVramSampleCappedAtReportedGPUCount: the array is bounded by the host's own
// reported inventory, so an agent cannot make the control plane issue an
// unbounded number of UPDATEs by inflating one heartbeat field.
func TestVramSampleCappedAtReportedGPUCount(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	seedGPURow(t, pool, hostID, 0, 8192)
	seedGPURow(t, pool, hostID, 1, 8192)
	ctx := context.Background()

	samples := make([]GPUVramSample, 0, 64)
	for i := 0; i < 64; i++ {
		samples = append(samples, GPUVramSample{Index: i, UsedMB: i32(1), FreeMB: i32(int32(100 + i))})
	}
	if err := s.applyVramSamples(ctx, hostID, 1000, samples); err != nil {
		t.Fatalf("apply oversized sample array: %v", err)
	}
	// The first two (in array order) are the ones inside the cap.
	if got := readVram(t, pool, hostID, 0); got.free == nil || *got.free != 100 {
		t.Fatalf("gpu 0 free=%v, want 100", got.free)
	}
	if got := readVram(t, pool, hostID, 1); got.free == nil || *got.free != 101 {
		t.Fatalf("gpu 1 free=%v, want 101", got.free)
	}
}

// TestVramSampleInvalidatedOnReconnect: reconnect and re-enrollment must NULL the
// telemetry (review finding #4). Without this, a "GPU is full" reading taken 5 s
// before an agent crash stays authoritative for the whole staleness window after
// reconnect — blocking relaunch at exactly the moment an operator is recovering
// from a host loss. And because gpus.index is an enumeration position over sorted
// cardN paths, a removed card SHIFTS indices and a surviving sample gets
// attributed to a different physical GPU.
//
// A routine capacity report must NOT invalidate — see the sub-test below. That
// distinction was learned the hard way on hermes.
func TestVramSampleInvalidatedOnReconnect(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	ctx := context.Background()

	prime := func(hostID string) {
		t.Helper()
		if err := s.applyVramSamples(ctx, hostID, time.Now().UnixMilli(), []GPUVramSample{
			{Index: 0, UsedMB: i32(16000), FreeMB: i32(1)}, // "this GPU is full"
		}); err != nil {
			t.Fatalf("prime sample: %v", err)
		}
		if got := readVram(t, pool, hostID, 0); got.free == nil {
			t.Fatal("prime sample did not store")
		}
	}
	assertCleared := func(hostID, after string) {
		t.Helper()
		got := readVram(t, pool, hostID, 0)
		if got.used != nil || got.free != nil || got.sampledAt != nil || got.agentMs != nil {
			t.Fatalf("%s left stale telemetry: %+v", after, got)
		}
	}

	t.Run("reconnect", func(t *testing.T) {
		res, err := s.enrollHost(ctx, "vram-reconnect", "v0", "tok", "tok")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		seedGPURow(t, pool, res.HostID, 0, 16384)
		prime(res.HostID)
		if _, err := s.reconnectHost(ctx, "vram-reconnect", "v0", res.NodeSecret); err != nil {
			t.Fatalf("reconnect: %v", err)
		}
		assertCleared(res.HostID, "reconnectHost")
	})

	t.Run("re-enrollment", func(t *testing.T) {
		res, err := s.enrollHost(ctx, "vram-reenroll", "v0", "tok", "tok")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		seedGPURow(t, pool, res.HostID, 0, 16384)
		prime(res.HostID)
		if _, err := s.enrollHost(ctx, "vram-reenroll", "v0", "tok", "tok"); err != nil {
			t.Fatalf("re-enroll: %v", err)
		}
		assertCleared(res.HostID, "enrollHost")
	})

	// A capacity report for an UNCHANGED GPU must PRESERVE the sample. The agent
	// re-sends capacity on console hotplug, on config_update, and after every
	// session stop — hermes emits one roughly every 5 s. An earlier revision NULLed
	// telemetry on this path too, which erased the sample as fast as the heartbeat
	// could write it: the admission harness reported "vram_sampled_at is null
	// (never sampled)" against a host that had been reporting fine minutes earlier.
	t.Run("routine capacity report preserves the sample", func(t *testing.T) {
		res, err := s.enrollHost(ctx, "vram-capacity", "v0", "tok", "tok")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		gpu := GPUCapacity{Index: 0, Vendor: "nvidia", Model: "test-gpu", VRAMMBTotal: 16384, EncodeSlotsTotal: 2}
		host := HostCapacity{CPUCores: 8, MemMB: 16384}
		if err := s.upsertCapacity(ctx, res.HostID, host, nil, []GPUCapacity{gpu}); err != nil {
			t.Fatalf("first capacity: %v", err)
		}
		prime(res.HostID)
		if err := s.upsertCapacity(ctx, res.HostID, host, nil, []GPUCapacity{gpu}); err != nil {
			t.Fatalf("repeat capacity: %v", err)
		}
		got := readVram(t, pool, res.HostID, 0)
		if got.free == nil || *got.free != 1 || got.sampledAt == nil {
			t.Fatalf("a routine capacity report wiped live telemetry: %+v", got)
		}
	})

	t.Run("identity change at the same index", func(t *testing.T) {
		res, err := s.enrollHost(ctx, "vram-identity", "v0", "tok", "tok")
		if err != nil {
			t.Fatalf("enroll: %v", err)
		}
		gpu := GPUCapacity{Index: 0, Vendor: "nvidia", Model: "old-gpu", VRAMMBTotal: 16384, EncodeSlotsTotal: 2}
		host := HostCapacity{CPUCores: 8, MemMB: 16384}
		if err := s.upsertCapacity(ctx, res.HostID, host, nil, []GPUCapacity{gpu}); err != nil {
			t.Fatalf("first capacity: %v", err)
		}
		prime(res.HostID)
		// Same index, DIFFERENT card. Even if the stale-marking step above were
		// ever skipped, the upsert's own identity check must clear the sample.
		gpu.Model = "new-gpu"
		if err := s.upsertCapacity(ctx, res.HostID, host, nil, []GPUCapacity{gpu}); err != nil {
			t.Fatalf("second capacity: %v", err)
		}
		assertCleared(res.HostID, "upsertCapacity across a card swap")
	})
}

// TestVramQueueDrainsAndCoalesces: the ingest path is off the websocket read loop
// (review finding #17), bounded, and latest-value coalescing per host. The read
// loop also carries acks, session_state and the signaling relay, and a stall past
// the 20 s read deadline marks the host offline and REAPS its sessions —
// telemetry must never be able to cause that.
func TestVramQueueDrainsAndCoalesces(t *testing.T) {
	pool := testPool(t)
	store := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	seedGPURow(t, pool, hostID, 0, 16384)

	q := newVramQueue(store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(q.close) // #406
	for i := 1; i <= 200; i++ {
		q.enqueue(vramSampleBatch{hostID: hostID, agentMs: int64(i), samples: []GPUVramSample{
			{Index: 0, UsedMB: i32(1), FreeMB: i32(int32(i))},
		}})
	}
	// An empty batch is not work.
	q.enqueue(vramSampleBatch{hostID: hostID, agentMs: 999})

	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, _, _, processed := q.stats()
		if pending == 0 && processed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("vram queue did not drain: pending=%d processed=%d", pending, processed)
		}
		time.Sleep(time.Millisecond)
	}
	// Whatever the coalescing collapsed, the monotonic guard means the STORED
	// value is always the newest one that was actually applied — never a rewind.
	got := readVram(t, pool, hostID, 0)
	if got.free == nil || got.agentMs == nil {
		t.Fatalf("queue drained but nothing stored: %+v", got)
	}
	if int64(*got.free) != *got.agentMs {
		t.Fatalf("stored a torn sample: free=%d agent_ms=%d", *got.free, *got.agentMs)
	}
}
