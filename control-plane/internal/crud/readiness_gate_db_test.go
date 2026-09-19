package crud

// The host body's readiness_gate / readiness_overrides (#262, amendment 11).
// Both are ALWAYS serialized, and `blocking` is populated whatever the state:
// while abstaining the entries are facts for the console, not exclusions.
//
// Requires Postgres (TEST_DATABASE_URL).

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const gateReport = `[
	{"id":"input_probe","status":"fail","summary":"s","remediation":"r",
	 "blocks":{"scope":"host","enforced_by":"control_plane"}},
	{"id":"media_probe_gpu1","status":"fail","summary":"s","remediation":"r",
	 "blocks":{"scope":"gpu","gpu_index":1,"enforced_by":"control_plane"}},
	{"id":"uinput","status":"fail","summary":"a proxy","remediation":"r"}
]`

// seedGateHost inserts a host; ageSecs < 0 means "never reported".
func seedGateHost(t *testing.T, pool *pgxpool.Pool, name, report string, ageSecs int) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`, name).Scan(&id); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if ageSecs >= 0 {
		if _, err := pool.Exec(ctx, `UPDATE hosts SET readiness = $2::jsonb,
			readiness_reported_at = now() - make_interval(secs => $3::int) WHERE id::text = $1`,
			id, report, ageSecs); err != nil {
			t.Fatalf("seed report: %v", err)
		}
	}
	return id
}

// gateOf serves the host and returns the decoded readiness_gate plus the raw
// readiness_overrides, exactly as a client would see them.
func gateOf(t *testing.T, s *store, hostID string) (ReadinessGate, json.RawMessage) {
	t.Helper()
	h, err := s.getHost(context.Background(), hostID)
	if err != nil {
		t.Fatalf("getHost: %v", err)
	}
	raw, err := json.Marshal(hostToResp(h))
	if err != nil {
		t.Fatalf("marshal host: %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal host: %v", err)
	}
	overrides, ok := body["readiness_overrides"]
	if !ok {
		t.Fatal("readiness_overrides is omitted; openapi.yaml Host requires it")
	}
	if string(overrides) != "[]" {
		t.Fatalf("readiness_overrides = %s, want [] (#263 fills it; it is never null)", overrides)
	}
	gateRaw, ok := body["readiness_gate"]
	if !ok {
		t.Fatal("readiness_gate is omitted; openapi.yaml Host requires it")
	}
	var gate ReadinessGate
	if err := json.Unmarshal(gateRaw, &gate); err != nil {
		t.Fatalf("unmarshal readiness_gate: %v", err)
	}
	if gate.Blocking == nil {
		t.Fatalf("readiness_gate.blocking is null, want []: %s", gateRaw)
	}
	return gate, overrides
}

func TestHostBodyServesTheReadinessGate(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}

	t.Run("never reported", func(t *testing.T) {
		gate, _ := gateOf(t, s, seedGateHost(t, pool, "gate-never", "", -1))
		if gate.State != "abstaining" || len(gate.Blocking) != 0 {
			t.Fatalf("gate = %+v, want abstaining with no entries", gate)
		}
	})

	t.Run("fresh and blocked", func(t *testing.T) {
		gate, _ := gateOf(t, s, seedGateHost(t, pool, "gate-fresh", gateReport, 5))
		if gate.State != "active" {
			t.Fatalf("state = %q, want active", gate.State)
		}
		// The proxy check is not listed: it carries no `blocks`.
		if len(gate.Blocking) != 2 {
			t.Fatalf("blocking = %+v, want the two evidence checks", gate.Blocking)
		}
		if gate.Blocking[0].CheckID != "input_probe" || gate.Blocking[0].GPUIndex != nil {
			t.Errorf("host-scope entry = %+v, want gpu_index null", gate.Blocking[0])
		}
		if gate.Blocking[1].GPUIndex == nil || *gate.Blocking[1].GPUIndex != 1 {
			t.Errorf("gpu-scope entry = %+v, want gpu_index 1", gate.Blocking[1])
		}
		if gate.Blocking[0].Overridden || gate.Blocking[1].Overridden {
			t.Errorf("nothing is overridden here: %+v", gate.Blocking)
		}
	})

	t.Run("stale: abstaining, still populated", func(t *testing.T) {
		gate, _ := gateOf(t, s, seedGateHost(t, pool, "gate-stale", gateReport, 3600))
		if gate.State != "abstaining" {
			t.Fatalf("state = %q, want abstaining", gate.State)
		}
		if len(gate.Blocking) != 2 {
			t.Fatalf("a stale report must still show its failing checks: %+v", gate.Blocking)
		}
	})

	t.Run("overridden", func(t *testing.T) {
		hostID := seedGateHost(t, pool, "gate-overridden", gateReport, 5)
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO host_readiness_overrides (host_id, check_id) VALUES ($1::uuid, 'input_probe')`,
			hostID); err != nil {
			t.Fatalf("insert override: %v", err)
		}
		gate, _ := gateOf(t, s, hostID)
		if len(gate.Blocking) != 2 {
			t.Fatalf("an override must never hide a failing check: %+v", gate.Blocking)
		}
		if !gate.Blocking[0].Overridden || gate.Blocking[1].Overridden {
			t.Fatalf("overridden flags = %+v, want only input_probe", gate.Blocking)
		}
	})
}

// TestHostListServesTheReadinessGate: the Hosts index badges a blocked host
// without a per-host fetch, so the list path must fill it too.
func TestHostListServesTheReadinessGate(t *testing.T) {
	pool := testPool(t)
	s := &store{pool: pool}
	hostID := seedGateHost(t, pool, "gate-list", gateReport, 5)

	hosts, _, err := s.listHosts(context.Background(), "", 50)
	if err != nil {
		t.Fatalf("listHosts: %v", err)
	}
	for _, h := range hosts {
		if h.ID != hostID {
			continue
		}
		if h.ReadinessGate.State != "active" || len(h.ReadinessGate.Blocking) != 2 {
			t.Fatalf("listHosts gate = %+v", h.ReadinessGate)
		}
		return
	}
	t.Fatal("seeded host missing from listHosts")
}

// TestReadinessGateStateUsesTheConfiguredWindow: the served state must be the
// one admission reaches, so the window is the same value, from the same config.
func TestReadinessGateStateUsesTheConfiguredWindow(t *testing.T) {
	pool := testPool(t)
	hostID := seedGateHost(t, pool, "gate-window", gateReport, 90)

	if gate, _ := gateOf(t, &store{pool: pool}, hostID); gate.State != "abstaining" {
		t.Fatalf("90 s old against the default 60 s window: %q, want abstaining", gate.State)
	}
	if gate, _ := gateOf(t, &store{pool: pool, readinessStaleSecs: 120}, hostID); gate.State != "active" {
		t.Fatalf("90 s old against a 120 s window: %q, want active", gate.State)
	}
}
