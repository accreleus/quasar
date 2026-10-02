package crud

// First-run-experience S1: the readiness set the agent reports must reach the
// host read path — that is the entire delivery mechanism for the remediation
// text the admin Hosts card and the setup wizard render.
//
// Requires Postgres (make test-db / scripts/dev/dev.sh go-test-db).

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestHostReadinessRoundTripsThroughTheHostReadPath(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()

	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ('readiness-host', 'online') RETURNING id::text`,
	).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	// Before any amendment-aware agent has reported: null, not an empty array.
	// The two are different facts and the UI must be able to tell them apart —
	// "no agent has told us" is not "this host has no problems".
	got, err := s.getHost(ctx, hostID)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	if len(got.Readiness) != 0 {
		t.Fatalf("unreported readiness: got %s, want empty/null", got.Readiness)
	}
	if got.ReadinessReportedAt != nil {
		t.Errorf("unreported readiness_reported_at: got %v, want nil", got.ReadinessReportedAt)
	}
	// json.RawMessage(nil) marshals as JSON null, which is the required
	// present-but-null shape on the API.
	raw, err := json.Marshal(hostToResp(got))
	if err != nil {
		t.Fatalf("marshal host: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal host resp: %v", err)
	}
	if v, ok := decoded["readiness"]; !ok || string(v) != "null" {
		t.Fatalf("readiness on the wire: got %s (present=%v), want null", v, ok)
	}
	if _, ok := decoded["readiness_reported_at"]; !ok {
		t.Error("readiness_reported_at must always be serialized, not omitted")
	}

	// After a report: the whole set — remediation included — is served back.
	if _, err := pool.Exec(ctx, `
		UPDATE hosts SET readiness = $2::jsonb, readiness_reported_at = now() WHERE id::text = $1`,
		hostID, `[{"id":"nvidia_lib32_gl","status":"fail","summary":"no 32-bit NVIDIA GL",
		           "remediation":"sudo dnf install -y nvidia-driver-libs.i686"}]`); err != nil {
		t.Fatalf("seed readiness: %v", err)
	}

	got, err = s.getHost(ctx, hostID)
	if err != nil {
		t.Fatalf("getHost after report: %v", err)
	}
	var checks []struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Summary     string `json:"summary"`
		Remediation string `json:"remediation"`
	}
	if err := json.Unmarshal(got.Readiness, &checks); err != nil {
		t.Fatalf("unmarshal readiness: %v", err)
	}
	if len(checks) != 1 || checks[0].ID != "nvidia_lib32_gl" || checks[0].Status != "fail" {
		t.Fatalf("readiness: got %+v", checks)
	}
	if checks[0].Remediation == "" {
		t.Error("remediation dropped on the read path — the fix instruction is the payload")
	}
	if got.ReadinessReportedAt == nil {
		t.Error("readiness_reported_at must be served so the UI can show freshness")
	}

	// The list path serves it too (hostToResp is shared), so the Hosts index can
	// badge an unhealthy host without a per-host fetch.
	hosts, _, err := s.listHosts(ctx, "", 50)
	if err != nil {
		t.Fatalf("listHosts: %v", err)
	}
	var found bool
	for _, h := range hosts {
		if h.ID == hostID {
			found = true
			if len(h.Readiness) == 0 {
				t.Error("listHosts dropped readiness")
			}
		}
	}
	if !found {
		t.Fatalf("seeded host missing from listHosts")
	}
}

// TestAmendment11ReadinessServedVerbatimByTheHostReadPath: amendment 11 (#260)
// adds observed_at/source/blocks and the `unknown` status, all optional and
// additive, to each readiness check. hosts.readiness is stored "verbatim and
// never re-encoded" (schema.md), so the read path (getHost/listHosts +
// hostToResp) needs no change — this proves it, seeding the row exactly the
// way the test above does (crud cannot import agentws to drive the real write
// seam: crud -> internal/images -> agentws is already a cycle in the other
// direction). agentws/readiness_amendment11_db_test.go proves the write seam
// (ValidReadiness + upsertHostReadiness) doesn't alter this same payload
// before it reaches this column, so together the two tests cover the whole
// path.
func TestAmendment11ReadinessServedVerbatimByTheHostReadPath(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	ctx := context.Background()

	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ('readiness-amendment11-host', 'online') RETURNING id::text`,
	).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}

	// The same checks as agentws/readiness_amendment11_db_test.go's
	// amendment11Payload: a gpu-scope block with all three optional fields, a
	// host-scope block enforced by the agent, an indeterminate probe, an
	// untouched legacy check, and a check carrying an unrecognized `source`,
	// `blocks.scope`, and extra top-level key (all agent-owned per
	// agent-api.md `capacity.readiness` — a consumer must pass them through).
	payload := `[
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
	if _, err := pool.Exec(ctx, `
		UPDATE hosts SET readiness = $2::jsonb, readiness_reported_at = now() WHERE id::text = $1`,
		hostID, payload); err != nil {
		t.Fatalf("seed readiness: %v", err)
	}

	var want []map[string]any
	if err := json.Unmarshal([]byte(payload), &want); err != nil {
		t.Fatalf("decode expected payload: %v", err)
	}

	assertRoundTrip := func(t *testing.T, raw json.RawMessage, reportedAt bool, from string) {
		t.Helper()
		if !reportedAt {
			t.Errorf("%s: readiness_reported_at not served", from)
		}
		var got []map[string]any
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("%s: decode served readiness: %v (%s)", from, err, raw)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: readiness mismatch:\n want %#v\n got  %#v", from, want, got)
		}
		gpuCheck := got[0]["blocks"].(map[string]any)
		if gpuCheck["scope"] != "gpu" || gpuCheck["gpu_index"] != float64(1) || gpuCheck["enforced_by"] != "control_plane" {
			t.Errorf("%s: gpu-scoped blocks mismatch: %+v", from, gpuCheck)
		}
		hostCheck := got[1]["blocks"].(map[string]any)
		if hostCheck["scope"] != "host" || hostCheck["enforced_by"] != "agent" {
			t.Errorf("%s: host-scoped blocks mismatch: %+v", from, hostCheck)
		}
		if _, present := hostCheck["gpu_index"]; present {
			t.Errorf("%s: host-scoped block carries gpu_index, want it absent entirely: %+v", from, hostCheck)
		}
		if got[2]["status"] != "unknown" {
			t.Errorf("%s: unknown-status check: got %+v", from, got[2])
		}
		legacy := got[3]
		if len(legacy) != 4 {
			t.Errorf("%s: legacy check must keep exactly its 4 keys, got %d: %+v", from, len(legacy), legacy)
		}
		for _, k := range []string{"observed_at", "source", "blocks"} {
			if _, present := legacy[k]; present {
				t.Errorf("%s: legacy check gained a null-valued key %q it never sent: %+v", from, k, legacy)
			}
		}
		unrecognized := got[4]
		if unrecognized["source"] != "quantum_probe" {
			t.Errorf("%s: unrecognized `source` was not passed through: %+v", from, unrecognized)
		}
		if unrecognized["confidence"] != 0.42 {
			t.Errorf("%s: unrecognized extra field `confidence` was dropped: %+v", from, unrecognized)
		}
		unrecognizedBlocks := unrecognized["blocks"].(map[string]any)
		if unrecognizedBlocks["scope"] != "vram_pool" {
			t.Errorf("%s: unrecognized `blocks.scope` was not passed through: %+v", from, unrecognizedBlocks)
		}
	}

	got, err := s.getHost(ctx, hostID)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	assertRoundTrip(t, got.Readiness, got.ReadinessReportedAt != nil, "getHost")

	// hostToResp is the JSON-wire shape both handleGetHost and handleListHosts
	// serve; go through it too so a future re-encode there is caught.
	resp := hostToResp(got)
	assertRoundTrip(t, resp.Readiness, resp.ReadinessReportedAt != nil, "hostToResp")

	hosts, _, err := s.listHosts(ctx, "", 50)
	if err != nil {
		t.Fatalf("listHosts: %v", err)
	}
	var found bool
	for _, h := range hosts {
		if h.ID == hostID {
			found = true
			assertRoundTrip(t, h.Readiness, h.ReadinessReportedAt != nil, "listHosts")
		}
	}
	if !found {
		t.Fatalf("seeded host missing from listHosts")
	}
}
