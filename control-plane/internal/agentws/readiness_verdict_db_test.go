package agentws

// The write half of the readiness gate (#262): storing a report, or changing the
// GPU set, derives hosts.readiness_block_* and gpus.readiness_blocked in the
// SAME transaction. Admission reads only those columns, so a derived value that
// lags the stored report is a host that schedules against evidence saying it
// must not.
//
// Requires Postgres (TEST_DATABASE_URL); see store_test.go's testPool.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func hostBlocks(t *testing.T, pool *pgxpool.Pool, hostID string) (host, homes bool) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT readiness_block_host, readiness_block_homes FROM hosts WHERE id::text = $1`,
		hostID).Scan(&host, &homes); err != nil {
		t.Fatalf("read host verdict: %v", err)
	}
	return host, homes
}

// blockedGPUs is the set of blocked GPU indexes on the host, as admission sees it.
func blockedGPUs(t *testing.T, pool *pgxpool.Pool, hostID string) map[int]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT index, readiness_blocked FROM gpus WHERE host_id::text = $1 ORDER BY index`, hostID)
	if err != nil {
		t.Fatalf("read gpu verdicts: %v", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var idx int
		var blocked bool
		if err := rows.Scan(&idx, &blocked); err != nil {
			t.Fatalf("scan gpu verdict: %v", err)
		}
		if blocked {
			out[idx] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate gpu verdicts: %v", err)
	}
	return out
}

func addGPURow(t *testing.T, pool *pgxpool.Pool, hostID string, index int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO gpus (host_id, index, vram_mb_total, encode_slots_total, reported)
		VALUES ($1, $2, 16384, 2, true)`, hostID, index); err != nil {
		t.Fatalf("add gpu %d: %v", index, err)
	}
}

const hostFail = `[{"id":"input_probe","status":"fail","summary":"s","remediation":"r",
	"blocks":{"scope":"host","enforced_by":"control_plane"}}]`

// TestReadinessReportDerivesTheHostVerdictInOneCall: ONE upsert must leave the
// columns describing the report it just stored. A second pass would be a window
// in which the host is blocked on paper and schedulable in fact.
func TestReadinessReportDerivesTheHostVerdictInOneCall(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(hostFail)); err != nil {
		t.Fatalf("upsert failing report: %v", err)
	}
	if host, homes := hostBlocks(t, pool, hostID); !host || homes {
		t.Fatalf("host-scope fail: block_host=%v block_homes=%v, want true/false", host, homes)
	}

	passing := `[{"id":"input_probe","status":"pass","summary":"s","remediation":"",
		"blocks":{"scope":"host","enforced_by":"control_plane"}}]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(passing)); err != nil {
		t.Fatalf("upsert passing report: %v", err)
	}
	if host, _ := hostBlocks(t, pool, hostID); host {
		t.Fatal("a passing report left the host blocked")
	}

	// An explicit [] is a real, fresh report: it clears everything.
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(hostFail)); err != nil {
		t.Fatalf("re-block: %v", err)
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if host, homes := hostBlocks(t, pool, hostID); host || homes {
		t.Fatalf("an empty report left block_host=%v block_homes=%v", host, homes)
	}
}

// TestReadinessAbsentOrMalformedReportChangesNothing: keep-if-absent must not
// derive anything either — including the freshness stamp the gate keys on.
func TestReadinessAbsentOrMalformedReportChangesNothing(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(hostFail)); err != nil {
		t.Fatalf("seed failing report: %v", err)
	}
	_, beforeAt := rawReadiness(t, pool, hostID)

	if err := s.upsertHostReadiness(ctx, hostID, nil); err != nil {
		t.Fatalf("absent report: %v", err)
	}
	if host, _ := hostBlocks(t, pool, hostID); !host {
		t.Fatal("an absent report cleared the derived verdict")
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(`{"nope":true}`)); err == nil {
		t.Fatal("a malformed report was accepted")
	}
	if host, _ := hostBlocks(t, pool, hostID); !host {
		t.Fatal("a malformed report cleared the derived verdict")
	}
	if _, at := rawReadiness(t, pool, hostID); at == nil || beforeAt == nil || *at != *beforeAt {
		t.Errorf("the freshness stamp moved: %v -> %v", beforeAt, at)
	}
}

// TestReadinessGPUScopeBlocksOnlyTheNamedGPU, and an index the host does not
// have blocks nothing — the write ignores it by construction.
func TestReadinessGPUScopeBlocksOnlyTheNamedGPU(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	addGPURow(t, pool, hostID, 0)
	addGPURow(t, pool, hostID, 1)
	ctx := context.Background()

	report := `[{"id":"media_probe_gpu1","status":"fail","summary":"s","remediation":"r",
		"blocks":{"scope":"gpu","gpu_index":1,"enforced_by":"control_plane"}}]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(report)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := blockedGPUs(t, pool, hostID); len(got) != 1 || !got[1] {
		t.Fatalf("blocked gpus = %v, want only index 1", got)
	}
	if host, homes := hostBlocks(t, pool, hostID); host || homes {
		t.Fatalf("a gpu-scope check blocked the host: %v/%v", host, homes)
	}

	absent := `[{"id":"media_probe_gpu7","status":"fail","summary":"s","remediation":"r",
		"blocks":{"scope":"gpu","gpu_index":7,"enforced_by":"control_plane"}}]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(absent)); err != nil {
		t.Fatalf("upsert absent index: %v", err)
	}
	if got := blockedGPUs(t, pool, hostID); len(got) != 0 {
		t.Fatalf("an index the host lacks blocked %v; and gpu 1 must have been cleared", got)
	}
}

// TestReadinessVerdictFollowsALaterGPUSet: the GPU row is created AFTER the
// report that names it. Without a recompute on the capacity path it would start
// unblocked and be schedulable until the next report.
func TestReadinessVerdictFollowsALaterGPUSet(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	report := `[{"id":"media_probe_gpu0","status":"fail","summary":"s","remediation":"r",
		"blocks":{"scope":"gpu","gpu_index":0,"enforced_by":"control_plane"}}]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(report)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if got := blockedGPUs(t, pool, hostID); len(got) != 0 {
		t.Fatalf("no GPU rows yet, but %v is blocked", got)
	}

	// A capacity message carrying no readiness at all.
	if err := s.upsertCapacity(ctx, hostID, HostCapacity{CPUCores: 8, MemMB: 16000}, nil,
		[]GPUCapacity{{Index: 0, Vendor: "nvidia", Model: "m", VRAMMBTotal: 16384, EncodeSlotsTotal: 2}}); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if got := blockedGPUs(t, pool, hostID); !got[0] {
		t.Fatalf("the new GPU row is schedulable although the stored report names it: %v", got)
	}
}

// TestReadinessOverrideLiftsTheBlockOnRecompute, and never an agent-enforced
// one: the agent refuses those launches itself. #263 writes these rows; here
// they are inserted directly, which is the only thing the verdict function cares
// about.
func TestReadinessOverrideLiftsTheBlockOnRecompute(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	ctx := context.Background()

	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(hostFail)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if host, _ := hostBlocks(t, pool, hostID); !host {
		t.Fatal("setup: the host should be blocked")
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO host_readiness_overrides (host_id, check_id) VALUES ($1::uuid, 'input_probe')`,
		hostID); err != nil {
		t.Fatalf("insert override: %v", err)
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(hostFail)); err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if host, _ := hostBlocks(t, pool, hostID); host {
		t.Fatal("the override did not lift the block on the next recompute")
	}

	agentEnforced := `[{"id":"startup_cleanup","status":"fail","summary":"s","remediation":"r",
		"blocks":{"scope":"host","enforced_by":"agent"}}]`
	if _, err := pool.Exec(ctx,
		`INSERT INTO host_readiness_overrides (host_id, check_id) VALUES ($1::uuid, 'startup_cleanup')`,
		hostID); err != nil {
		t.Fatalf("insert agent-enforced override: %v", err)
	}
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(agentEnforced)); err != nil {
		t.Fatalf("agent-enforced report: %v", err)
	}
	if host, _ := hostBlocks(t, pool, hostID); !host {
		t.Fatal("an override lifted an agent-enforced block")
	}
}

// TestReadinessProxyAndUnknownNeverDerive: only a check carrying `blocks` with
// status exactly fail may set a column. This is the false-negative outage the
// advisory rule was written to prevent (ADR 0005).
func TestReadinessProxyAndUnknownNeverDerive(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	addGPURow(t, pool, hostID, 0)
	ctx := context.Background()

	report := `[
		{"id":"render_node","status":"fail","summary":"proxy","remediation":"r"},
		{"id":"uinput","status":"fail","summary":"proxy","remediation":"r"},
		{"id":"audio_probe","status":"unknown","summary":"inconclusive","remediation":"",
		 "blocks":{"scope":"host","enforced_by":"control_plane"}},
		{"id":"homes_free_space","status":"warn","summary":"tight","remediation":"",
		 "blocks":{"scope":"homes","enforced_by":"control_plane"}},
		{"id":"media_probe_gpu0","status":"skip","summary":"n/a","remediation":"",
		 "blocks":{"scope":"gpu","gpu_index":0,"enforced_by":"control_plane"}}
	]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(report)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	host, homes := hostBlocks(t, pool, hostID)
	if host || homes || len(blockedGPUs(t, pool, hostID)) != 0 {
		t.Fatalf("a proxy / unknown / warn / skip report blocked something: host=%v homes=%v gpus=%v",
			host, homes, blockedGPUs(t, pool, hostID))
	}
}

// TestReadinessUnsupportedIsStoredVerbatimAndBlocksNothing (#311): a codec the GPU
// has no encoder for reports `unsupported`. It reaches storage as sent and derives
// no block, even beside a passing H.264 probe on the same GPU.
func TestReadinessUnsupportedIsStoredVerbatimAndBlocksNothing(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)
	addGPURow(t, pool, hostID, 0)
	addGPURow(t, pool, hostID, 1)
	ctx := context.Background()

	report := `[
		{"id":"media_probe_gpu1","status":"pass","summary":"encoded","remediation":"",
		 "source":"host_probe","blocks":{"scope":"gpu","gpu_index":1,"enforced_by":"control_plane"}},
		{"id":"media_probe_gpu1_av1","status":"unsupported",
		 "summary":"GPU 1 does not encode av1; sessions will not use av1 on this GPU: vulkanav1enc: the encode pipeline could not reach READY",
		 "remediation":"","source":"host_probe"}
	]`
	if err := s.upsertHostReadiness(ctx, hostID, json.RawMessage(report)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	host, homes := hostBlocks(t, pool, hostID)
	if host || homes || len(blockedGPUs(t, pool, hostID)) != 0 {
		t.Fatalf("an unsupported check blocked something: host=%v homes=%v gpus=%v",
			host, homes, blockedGPUs(t, pool, hostID))
	}

	raw, _ := rawReadiness(t, pool, hostID)
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if len(got) != 2 || got[1]["id"] != "media_probe_gpu1_av1" || got[1]["status"] != "unsupported" {
		t.Fatalf("stored readiness = %s, want the unsupported check verbatim", raw)
	}
	if summary, _ := got[1]["summary"].(string); !strings.Contains(summary, "could not reach READY") {
		t.Errorf("the unsupported check's evidence was not stored verbatim: %+v", got[1])
	}
}

// TestReadinessHomesScopeDerivesItsOwnColumn keeps the two host-level columns
// distinct: `homes` excludes only the launches that mount a managed home.
func TestReadinessHomesScopeDerivesItsOwnColumn(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHost(t, pool)

	report := `[{"id":"homes_root_writable","status":"fail","summary":"s","remediation":"r",
		"blocks":{"scope":"homes","enforced_by":"control_plane"}}]`
	if err := s.upsertHostReadiness(context.Background(), hostID, json.RawMessage(report)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if host, homes := hostBlocks(t, pool, hostID); host || !homes {
		t.Fatalf("homes-scope fail: block_host=%v block_homes=%v, want false/true", host, homes)
	}
}
