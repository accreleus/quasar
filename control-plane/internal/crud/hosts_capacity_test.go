package crud

// hosts_capacity_test.go — Host.capacity (UI v3 amendment, 2026-08-28): the
// per-host roll-up of what GET /v1/hosts/{id}/gpus already serves per GPU, so a
// fleet gauge stops doing one call per host.
//
// Requires Postgres: make test-db.
//
// What is load-bearing here:
//   - the roll-up sums the SAME set the per-GPU route lists (reported GPUs on a
//     capacity_detection='ok' host) with the SAME "used" semantics, or the two
//     views disagree and an operator has to decide which one is lying;
//   - null, not a zeroed object, when there is nothing to sum. "No capacity
//     report" and "zero capacity" are different facts and a gauge must not draw
//     the first as the second;
//   - one aggregate query for the whole page — the whole point of the field.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedCapacityHost inserts an online host with two reported GPUs (2 + 3 encode
// slots, 8000 + 16000 MB) and one running session holding one encode slot on the
// FIRST GPU, which is also sampled at 4000 MB used.
func seedCapacityHost(t *testing.T, pool *pgxpool.Pool) (hostID string) {
	t.Helper()
	ctx := context.Background()
	var userID, appID, gpu0 string
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('cap@crud.test','cap','x') RETURNING id::text`).Scan(&userID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, default_vram_mb, default_encode_slots, default_width, default_height, default_fps, default_bitrate_kbps)
		VALUES ('cap-app', 1024, 1, 1280, 720, 60, 6000) RETURNING id::text`).Scan(&appID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('cap-host','online','ok') RETURNING id::text`).Scan(&hostID))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total, vram_mb_used, vram_sampled_at)
		VALUES ($1, 0, 8000, 2, 4000, now()) RETURNING id::text`, hostID).Scan(&gpu0))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total)
		VALUES ($1, 1, 16000, 3) RETURNING id::text`, hostID).Scan(new(string)))
	// The reservation. reserved_vram_mb stays 0, exactly as every session created
	// since #383 removed declared VRAM from admission — which is precisely why
	// capacity.vram_mb_used reads the live sample instead.
	if _, err := pool.Exec(ctx, `
		INSERT INTO sessions (user_id, app_id, host_id, gpu_id, state,
		                      width, height, fps, bitrate_kbps,
		                      reserved_encode_slots, reserved_vram_mb)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'running', 1280, 720, 60, 6000, 1, 0)
	`, userID, appID, hostID, gpu0); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return hostID
}

func TestHostCapacityRollsUpEveryGPU(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()
	hostID := seedCapacityHost(t, pool)

	got, err := s.getHost(ctx, hostID)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	if got.Capacity == nil {
		t.Fatalf("capacity is null for a host with two reported GPUs")
	}
	want := HostCapacity{
		SlotsTotal: 5, SlotsUsed: 1,
		VramMBTotal: 24000, VramMBUsed: 4000,
		ActiveSessions: 1, GPUCount: 2,
	}
	if *got.Capacity != want {
		t.Errorf("capacity = %+v, want %+v", *got.Capacity, want)
	}

	// The list path serves it too (one aggregate for the page), so the fleet view
	// needs no per-host follow-up.
	hosts, _, err := s.listHosts(ctx, "", 50)
	if err != nil {
		t.Fatalf("listHosts: %v", err)
	}
	var found bool
	for _, h := range hosts {
		if h.ID != hostID {
			continue
		}
		found = true
		if h.Capacity == nil || *h.Capacity != want {
			t.Errorf("listHosts capacity = %+v, want %+v", h.Capacity, want)
		}
	}
	if !found {
		t.Fatalf("seeded host missing from listHosts")
	}
}

// TestHostCapacityIsNullWithoutGPUs: a host that has never reported hardware has
// nothing to sum, and the field must say so rather than report a fleet of zeros.
func TestHostCapacityIsNullWithoutGPUs(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()

	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ('bare-host','online') RETURNING id::text`,
	).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	got, err := s.getHost(ctx, hostID)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	if got.Capacity != nil {
		t.Fatalf("capacity = %+v for a host with no GPUs, want null", *got.Capacity)
	}

	// Present-but-null on the wire: the key is always serialized, so a client can
	// tell "unknown" from "absent field on an old server".
	raw, err := json.Marshal(hostToResp(got))
	if err != nil {
		t.Fatalf("marshal host: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal host resp: %v", err)
	}
	if v, ok := decoded["capacity"]; !ok || string(v) != "null" {
		t.Fatalf("capacity on the wire: got %s (present=%v), want null", v, ok)
	}
}

// TestHostCapacityExcludesUnschedulableGPUs: the roll-up must list exactly what
// GET /v1/hosts/{id}/gpus lists. An unreported GPU (agent said nothing this boot)
// and a host whose capacity detection failed are both absent there, so both must
// be absent here — otherwise the summary claims capacity the detail view denies.
func TestHostCapacityExcludesUnschedulableGPUs(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()

	var okHost, failedHost string
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection)
		VALUES ('mixed-host','online','ok') RETURNING id::text`).Scan(&okHost))
	mustExec(t, pool.QueryRow(ctx, `INSERT INTO hosts (node_name, status, capacity_detection, capacity_reason)
		VALUES ('failed-host','online','failed','nvidia-smi not found') RETURNING id::text`).Scan(&failedHost))
	if _, err := pool.Exec(ctx, `
		INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total, reported) VALUES
		  ($1, 0, 8000, 2, true),
		  ($1, 1, 8000, 2, false),
		  ($2, 0, 8000, 2, true)
	`, okHost, failedHost); err != nil {
		t.Fatalf("seed gpus: %v", err)
	}

	got, err := s.getHost(ctx, okHost)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	if got.Capacity == nil {
		t.Fatalf("capacity is null for a host with one reported GPU")
	}
	if got.Capacity.GPUCount != 1 || got.Capacity.SlotsTotal != 2 || got.Capacity.VramMBTotal != 8000 {
		t.Errorf("capacity = %+v; the unreported GPU must not be summed", *got.Capacity)
	}

	failed, err := s.getHost(ctx, failedHost)
	if err != nil {
		t.Fatalf("getHost(failed): %v", err)
	}
	if failed.Capacity != nil {
		t.Errorf("capacity = %+v on a capacity_detection='failed' host, want null "+
			"(GET /v1/hosts/{id}/gpus returns nothing for it)", *failed.Capacity)
	}
}
