package agentws

// Amendment 11 (#260, evidence-gated readiness): the write seam — ValidReadiness
// (decode) and upsertHostReadiness (storage) — must accept the new optional
// per-check fields (observed_at/source/blocks) and the new `unknown` status and
// store them verbatim, exactly like the pre-amendment unrecognized-field case
// TestUpsertHostReadinessStoresTheWholeCheckSet already covers.
//
// This package cannot also reach crud's read path in one test binary: crud
// imports internal/images, which imports agentws — the read-path proof for
// this same payload lives in crud/host_readiness_db_test.go instead, seeded
// with the identical JSON this test proves the write seam does not alter.
//
// Requires Postgres (TEST_DATABASE_URL); see store_test.go's testPool.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

// amendment11Payload exercises every amendment-11 shape in one report: a
// gpu-scope block with all three optional fields, a host-scope block enforced
// by the agent itself, an indeterminate probe, an untouched legacy check, and
// a check a hypothetically newer-still agent sent (unrecognized `source`,
// `blocks.scope`, and an extra top-level key) — the contract's "id, status,
// source and blocks.scope are agent-owned; pass an unrecognized one through"
// rule (agent-api.md `capacity.readiness`). The same checks as
// the seed payload in crud/host_readiness_db_test.go's
// TestAmendment11ReadinessServedVerbatimByTheHostReadPath so the two tests
// prove the same fact on each side of the write/read seam.
const amendment11Payload = `[
	{"id":"media_probe_gpu1","status":"fail",
	 "summary":"GPU 1 could not composite and encode",
	 "remediation":"check the render node and the driver volume",
	 "observed_at":"2026-09-18T10:00:00Z","source":"host_probe",
	 "blocks":{"scope":"gpu","gpu_index":1,"enforced_by":"control_plane"}},
	{"id":"runtime_endpoint","status":"fail",
	 "summary":"container runtime unreachable",
	 "remediation":"restart the container runtime",
	 "observed_at":"2026-09-18T10:05:00Z","source":"runtime",
	 "blocks":{"scope":"host","enforced_by":"agent"}},
	{"id":"audio_probe","status":"unknown",
	 "summary":"a host probe deadline passed with no result yet","remediation":""},
	{"id":"uinput","status":"pass","summary":"/dev/uinput present and writable","remediation":""},
	{"id":"future_check","status":"warn","summary":"a future observation","remediation":"",
	 "observed_at":"2026-09-18T10:10:00Z","source":"quantum_probe",
	 "blocks":{"scope":"vram_pool","enforced_by":"agent"},"confidence":0.42}
]`

func TestAmendment11ReadinessSurvivesTheWriteSeam(t *testing.T) {
	pool := testPool(t)
	s := storeWithMintedTokens(pool, nil)
	hostID := seedHost(t, pool)
	ctx := context.Background()

	payload := json.RawMessage(amendment11Payload)

	// The wire-decode seam: no status allow-list, so `unknown` must pass, and
	// none of the new per-check keys are modeled — they only need to not
	// trip validation (ReadinessCheck is a validation view, never storage).
	checks, ok := ValidReadiness(payload)
	if !ok || len(checks) != 5 {
		t.Fatalf("ValidReadiness rejected an amendment-11 report: ok=%v checks=%+v", ok, checks)
	}
	if checks[2].Status != "unknown" {
		t.Fatalf("status `unknown` did not survive typed decode: %+v", checks[2])
	}

	if err := s.upsertHostReadiness(ctx, hostID, payload); err != nil {
		t.Fatalf("upsert amendment-11 readiness: %v", err)
	}

	raw, at := rawReadiness(t, pool, hostID)
	if at == nil {
		t.Error("readiness_reported_at must be stamped by the write")
	}

	var want, got []map[string]any
	if err := json.Unmarshal(payload, &want); err != nil {
		t.Fatalf("decode expected payload: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode stored readiness: %v (%s)", err, raw)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("stored readiness mismatch:\n want %#v\n got  %#v", want, got)
	}

	gpuCheck := got[0]["blocks"].(map[string]any)
	if gpuCheck["scope"] != "gpu" || gpuCheck["gpu_index"] != float64(1) || gpuCheck["enforced_by"] != "control_plane" {
		t.Errorf("gpu-scoped blocks mismatch: %+v", gpuCheck)
	}
	hostCheck := got[1]["blocks"].(map[string]any)
	if hostCheck["scope"] != "host" || hostCheck["enforced_by"] != "agent" {
		t.Errorf("host-scoped blocks mismatch: %+v", hostCheck)
	}
	if _, present := hostCheck["gpu_index"]; present {
		t.Errorf("host-scoped block carries gpu_index, want it absent entirely: %+v", hostCheck)
	}
	if got[2]["status"] != "unknown" {
		t.Errorf("unknown-status check: got %+v", got[2])
	}
	legacy := got[3]
	if len(legacy) != 4 {
		t.Errorf("legacy check must keep exactly its 4 keys, got %d: %+v", len(legacy), legacy)
	}
	for _, k := range []string{"observed_at", "source", "blocks"} {
		if _, present := legacy[k]; present {
			t.Errorf("legacy check gained a null-valued key %q it never sent: %+v", k, legacy)
		}
	}
	unrecognized := got[4]
	if unrecognized["source"] != "quantum_probe" {
		t.Errorf("unrecognized `source` was not passed through: %+v", unrecognized)
	}
	if unrecognized["confidence"] != 0.42 {
		t.Errorf("unrecognized extra field `confidence` was dropped: %+v", unrecognized)
	}
	unrecognizedBlocks := unrecognized["blocks"].(map[string]any)
	if unrecognizedBlocks["scope"] != "vram_pool" {
		t.Errorf("unrecognized `blocks.scope` was not passed through: %+v", unrecognizedBlocks)
	}
}
