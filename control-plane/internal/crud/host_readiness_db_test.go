package crud

// First-run-experience S1: the readiness set the agent reports must reach the
// host read path — that is the entire delivery mechanism for the remediation
// text the admin Hosts card and the setup wizard render.
//
// Requires Postgres (make test-db / scripts/dev/dev.sh go-test-db).

import (
	"context"
	"encoding/json"
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
