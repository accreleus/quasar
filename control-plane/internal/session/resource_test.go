package session

import (
	"context"
	"testing"
)

// findGPU returns the availability row for a GPU by id, failing the test if absent.
func findGPU(t *testing.T, avs []GPUAvailability, gpuID string) GPUAvailability {
	t.Helper()
	for _, a := range avs {
		if a.GPUID == gpuID {
			return a
		}
	}
	t.Fatalf("gpu %s not in availability view (%d rows)", gpuID, len(avs))
	return GPUAvailability{}
}

// TestGPUAvailabilityCountsActiveReservation: a reserved, active session shows up
// in the observable per-GPU resource view as reserved-and-subtracted-from-available.
// This is the "visible in control-plane state" surface the Phase-2 governor reads
// — derived from live reservations, not stored.
//
// ENCODE SLOTS ONLY since #383: declared per-app VRAM left admission, so new
// sessions write reserved_vram_mb = 0 and the declared VRAM figures are pinned at
// total/0/total. The live VRAM view is asserted separately, in
// TestGPUAvailabilityExposesLiveVram.
func TestGPUAvailabilityCountsActiveReservation(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4) // GPU: 16384 MB VRAM, 4 encode slots
	ctx := context.Background()

	// Before any launch the GPU is fully available.
	before := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if before.VramMBReserved != 0 || before.SlotsReserved != 0 || before.ActiveSessions != 0 {
		t.Fatalf("idle GPU shows a reservation: %+v", before)
	}
	if before.VramMBAvailable != 16384 || before.SlotsAvailable != 4 {
		t.Fatalf("idle availability: got %d MB / %d slots want 16384 / 4", before.VramMBAvailable, before.SlotsAvailable)
	}

	// Launch reserves 1024 MB + 1 slot, then advance to `starting` to prove a
	// non-`assigned` active state still holds the reservation.
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s)) // NeedEncodeSlots 1
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if _, err := store.Transition(ctx, sess.ID, StateStarting, nil, nil); err != nil {
		t.Fatalf("→ starting: %v", err)
	}

	after := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if after.VramMBReserved != 0 || after.SlotsReserved != 1 {
		t.Fatalf("reserved: got %d MB / %d slots want 0 / 1 (declared VRAM left admission)", after.VramMBReserved, after.SlotsReserved)
	}
	if after.VramMBAvailable != 16384 || after.SlotsAvailable != 3 {
		t.Fatalf("available: got %d MB / %d slots want 16384 / 3", after.VramMBAvailable, after.SlotsAvailable)
	}
	if after.ActiveSessions != 1 {
		t.Fatalf("active_sessions: got %d want 1", after.ActiveSessions)
	}
}

// TestGPUAvailabilityReleasesOnTerminal: driving a session terminal releases its
// reservation from the observable view (a stopped session holds nothing), and the
// hostID filter scopes the view. This is the "stopping releases it, visible in
// control-plane state" half of the acceptance.
func TestGPUAvailabilityReleasesOnTerminal(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// While assigned, the reservation is visible scoped to the host.
	held := findGPU(t, mustAvail(t, store, s.hostID), s.gpuID)
	if held.SlotsReserved != 1 {
		t.Fatalf("held reservation: got %d slots want 1", held.SlotsReserved)
	}

	// Stop it (terminal) — the reservation must be released from the view.
	if _, err := store.Transition(ctx, sess.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("→ stopped: %v", err)
	}
	freed := findGPU(t, mustAvail(t, store, s.hostID), s.gpuID)
	if freed.VramMBReserved != 0 || freed.SlotsReserved != 0 || freed.ActiveSessions != 0 {
		t.Fatalf("terminal session still reserved: %+v", freed)
	}
	if freed.VramMBAvailable != 16384 || freed.SlotsAvailable != 4 {
		t.Fatalf("availability not restored: got %d MB / %d slots want 16384 / 4", freed.VramMBAvailable, freed.SlotsAvailable)
	}
}

// TestGPUAvailabilityRenderNode verifies render_node is always present in the
// availability view — null for a GPU that hasn't reported one, and the stored
// by-path device path once it has (host-observability-2, openapi.yaml
// GPUAvailability.render_node, required).
func TestGPUAvailabilityRenderNode(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	before := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if before.RenderNode != nil {
		t.Fatalf("render_node before any report = %v, want nil", before.RenderNode)
	}

	if _, err := pool.Exec(ctx, `UPDATE gpus SET render_node = $2 WHERE id::text = $1`,
		s.gpuID, "/dev/dri/by-path/pci-0000:04:00.0-render"); err != nil {
		t.Fatalf("seed render_node: %v", err)
	}
	after := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if after.RenderNode == nil || *after.RenderNode != "/dev/dri/by-path/pci-0000:04:00.0-render" {
		t.Fatalf("render_node after seed = %v, want the seeded path", after.RenderNode)
	}
}

// TestGPUAvailabilityExposesLiveVram: the read model surfaces the agent's live
// telemetry (#383 §5) as nullable used/free/sampled_at. Null must stay null all
// the way to the caller — an unsampled GPU is UNKNOWN, and rendering that as 0
// would read as "this GPU is full".
func TestGPUAvailabilityExposesLiveVram(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)

	before := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if before.VramMBUsed != nil || before.VramMBFree != nil || before.VramSampledAt != nil {
		t.Fatalf("unsampled GPU reports live VRAM: used=%v free=%v at=%v",
			before.VramMBUsed, before.VramMBFree, before.VramSampledAt)
	}

	sampleVram(t, pool, s.gpuID, 4096, 12288, 0)

	after := findGPU(t, mustAvail(t, store, ""), s.gpuID)
	if after.VramMBUsed == nil || *after.VramMBUsed != 4096 {
		t.Fatalf("vram_mb_used = %v, want 4096", after.VramMBUsed)
	}
	if after.VramMBFree == nil || *after.VramMBFree != 12288 {
		t.Fatalf("vram_mb_free = %v, want 12288", after.VramMBFree)
	}
	if after.VramSampledAt == nil {
		t.Fatal("vram_sampled_at = nil after a sample")
	}
	// The declared accounting is untouched and pinned at 0 (deprecated, retained).
	if after.VramMBReserved != 0 {
		t.Fatalf("vram_mb_reserved = %d, want 0", after.VramMBReserved)
	}
}

func mustAvail(t *testing.T, store *Store, hostID string) []GPUAvailability {
	t.Helper()
	avs, err := store.GPUAvailability(context.Background(), hostID)
	if err != nil {
		t.Fatalf("GPUAvailability: %v", err)
	}
	return avs
}
