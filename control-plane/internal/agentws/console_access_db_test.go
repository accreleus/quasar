package agentws

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/console"
)

// captureAuditor records every Record call so a test can assert amendment
// 18's console.access.restored event fired exactly once per applied reset.
type captureAuditor struct {
	mu      sync.Mutex
	actions []string
}

func (a *captureAuditor) Record(_ context.Context, _, action, _, _ string, _ map[string]any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actions = append(a.actions, action)
	return nil
}

func (a *captureAuditor) count(action string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, act := range a.actions {
		if act == action {
			n++
		}
	}
	return n
}

func consoleAccessHandler(t *testing.T) (*Handler, *pgxpool.Pool, *captureAuditor) {
	t.Helper()
	pool := testPool(t)
	aud := &captureAuditor{}
	registry := NewRegistry(slog.Default())
	h := &Handler{
		store:        &agentStore{pool: pool, isAgentConnected: registry.IsConnected},
		log:          slog.Default(),
		events:       noopEvents{},
		registry:     registry,
		consoleStore: console.NewStore(pool),
		consoleAuto:  newConsoleAutoState(),
		auditor:      aud,
	}
	return h, pool, aud
}

func capacityRaw(t *testing.T, msg CapacityMsg) []byte {
	t.Helper()
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("encode capacity: %v", err)
	}
	return raw
}

func abool(b bool) *bool    { return &b }
func astr(s string) *string { return &s }

func placementHoldPendingRaw(t *testing.T, pool *pgxpool.Pool, hostID string) bool {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT capabilities FROM console_capabilities WHERE host_id::text = $1`, hostID).Scan(&raw)
	if err != nil {
		return false
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode console_capabilities: %v", err)
	}
	v, _ := out["_placement_hold_pending"].(bool)
	return v
}

// A capacity report that carries console_capabilities but no `access` clears
// stored access via the ordinary UpsertCapabilities path (full replacement,
// access simply omitted). A capacity with no console_capabilities AT ALL
// takes the ClearAccess path instead — connectors/etc from the last report
// must survive that one untouched.
func TestProcessCapacityWithoutConsoleCapabilitiesClearsAccessOnly(t *testing.T) {
	h, pool, _ := consoleAccessHandler(t)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	ac := &conn{hostID: hostID}

	withAccess := capacityRaw(t, CapacityMsg{
		ConsoleCapabilities: &console.Capabilities{
			Connectors: []string{"DP-1"}, InputDevices: []console.InputDevicePath{},
			Access: &console.Access{State: "on", Target: abool(true), RequestID: astr("req-clear-1"), Summary: "on"},
		},
	})
	if err := h.processCapacity(ctx, ac, withAccess); err != nil {
		t.Fatalf("process capacity 1: %v", err)
	}
	caps, err := h.consoleStore.GetCapabilities(ctx, hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if caps.Access == nil || caps.Access.State != "on" {
		t.Fatalf("access = %v, want stored", caps.Access)
	}

	// No console_capabilities at all: connectors survive, access clears.
	noConsole := capacityRaw(t, CapacityMsg{})
	if err := h.processCapacity(ctx, ac, noConsole); err != nil {
		t.Fatalf("process capacity 2: %v", err)
	}
	caps, err = h.consoleStore.GetCapabilities(ctx, hostID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if caps.Access != nil {
		t.Fatalf("access = %v, want cleared by a capacity with no console_capabilities", caps.Access)
	}
	if len(caps.Connectors) != 1 || caps.Connectors[0] != "DP-1" {
		t.Fatalf("connectors = %v, want [DP-1] to survive the clear", caps.Connectors)
	}
}

// A restored report whose target equals the stored `enabled` flips it,
// pushes config_update, and audits console.access.restored exactly once —
// even if the agent resends the same settled report again.
func TestProcessCapacityRestoredResetsAuditsAndSettlesHold(t *testing.T) {
	h, pool, aud := consoleAccessHandler(t)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	ac := &conn{hostID: hostID}

	if err := h.consoleStore.Upsert(ctx, hostID, map[string]any{"enabled": true}, nil); err != nil {
		t.Fatalf("seed console_config: %v", err)
	}
	if err := h.consoleStore.SetPlacementHoldPending(ctx, hostID, true); err != nil {
		t.Fatalf("set hold: %v", err)
	}

	restored := capacityRaw(t, CapacityMsg{
		ConsoleCapabilities: &console.Capabilities{
			Connectors: []string{}, InputDevices: []console.InputDevicePath{},
			Access: &console.Access{State: "restored", Target: abool(true), RequestID: astr("req-flow-1"),
				Reason: astr("unhealthy"), Summary: "restored"},
		},
	})
	if err := h.processCapacity(ctx, ac, restored); err != nil {
		t.Fatalf("process capacity: %v", err)
	}

	sparse, err := h.consoleStore.Get(ctx, hostID)
	if err != nil {
		t.Fatalf("get console_config: %v", err)
	}
	resolved, err := console.Resolve(sparse)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Enabled {
		t.Fatal("enabled did not flip to false after the restored reset")
	}
	if aud.count("console.access.restored") != 1 {
		t.Fatalf("console.access.restored fired %d times, want 1", aud.count("console.access.restored"))
	}
	if placementHoldPendingRaw(t, pool, hostID) {
		t.Fatal("placement hold did not settle after the restored report agreed with the flipped enabled")
	}

	// Resend the identical (settled) report: every capacity resend carries
	// the current access, so this must not audit or apply again.
	if err := h.processCapacity(ctx, ac, restored); err != nil {
		t.Fatalf("process capacity replay: %v", err)
	}
	if aud.count("console.access.restored") != 1 {
		t.Fatalf("console.access.restored fired %d times after replay, want still 1", aud.count("console.access.restored"))
	}
}
