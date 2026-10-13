package console

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// testPool is this package's DB-test bootstrap (mirrors agentws/store_test.go
// and internal/session/lifecycle_test.go): skipped without TEST_DATABASE_URL.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM console_capabilities; DELETE FROM console_config;
		DELETE FROM hosts WHERE node_name LIKE 'console-test-%';
		DELETE FROM users WHERE email LIKE 'console-test-%';
		DELETE FROM apps WHERE name LIKE 'console-test-%'`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedTestHost(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO hosts (node_name, status) VALUES ($1, 'online') RETURNING id::text`, name).Scan(&id)
	if err != nil {
		t.Fatalf("seed host: %v", err)
	}
	return id
}

func seedTestUser(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(), `INSERT INTO users (email, username, password_hash)
		VALUES ($1, $1, 'x') RETURNING id::text`, "console-test-"+name+"@test.local").Scan(&id)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// rawCaps reads the console_capabilities JSONB verbatim, including the
// control-plane-owned bookkeeping keys Capabilities never decodes.
func rawCaps(t *testing.T, pool *pgxpool.Pool, hostID string) map[string]any {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT capabilities FROM console_capabilities WHERE host_id::text = $1`, hostID).Scan(&raw)
	if err == pgx.ErrNoRows {
		return map[string]any{}
	}
	if err != nil {
		t.Fatalf("query console_capabilities: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode console_capabilities: %v", err)
	}
	return out
}

func handledRequestID(t *testing.T, pool *pgxpool.Pool, hostID string) any {
	return rawCaps(t, pool, hostID)["_handled_restored_request_id"]
}

func placementHoldPending(t *testing.T, pool *pgxpool.Pool, hostID string) bool {
	v, _ := rawCaps(t, pool, hostID)["_placement_hold_pending"].(bool)
	return v
}

func markHandled(t *testing.T, pool *pgxpool.Pool, hostID, requestID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE console_capabilities
		   SET capabilities = jsonb_set(capabilities, '{_handled_restored_request_id}', to_jsonb($2::text), true)
		 WHERE host_id::text = $1
	`, hostID, requestID)
	if err != nil {
		t.Fatalf("mark handled: %v", err)
	}
}

func rawConsoleConfig(t *testing.T, pool *pgxpool.Pool, hostID string) (updatedBy *string, enabled bool) {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(context.Background(),
		`SELECT config, updated_by FROM console_config WHERE host_id::text = $1`, hostID).Scan(&raw, &updatedBy)
	if err == pgx.ErrNoRows {
		return nil, false
	}
	if err != nil {
		t.Fatalf("query console_config: %v", err)
	}
	cfg := map[string]any{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode console_config: %v", err)
	}
	enabled, _ = cfg["enabled"].(bool)
	return updatedBy, enabled
}

// UpsertCapabilities must preserve the control-plane's own bookkeeping keys
// (_handled_restored_request_id, _placement_hold_pending) across an ordinary
// capacity resend, and must drop the hold marker (but not the handled-id
// record) the moment a report carries no `access` at all — amendment 18's
// "never held" rule for a host whose agent stops reporting it.
func TestUpsertCapabilitiesPreservesBookkeeping(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-bookkeeping")
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{"DP-1"}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "on", Target: boolPtr(true), RequestID: strPtr("req-1"), Summary: "on"},
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := store.SetPlacementHoldPending(ctx, hostID, true); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	markHandled(t, pool, hostID, "req-1")

	// An unrelated resend (different connectors, access repeated) must not
	// drop either bookkeeping key.
	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{"DP-1", "HDMI-A-1"}, InputDevices: []InputDevicePath{},
		Access: &Access{State: "on", Target: boolPtr(true), RequestID: strPtr("req-1"), Summary: "on"},
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, err := store.GetCapabilities(ctx, hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Connectors) != 2 {
		t.Fatalf("connectors = %v, want the new report", got.Connectors)
	}
	if v := handledRequestID(t, pool, hostID); v != "req-1" {
		t.Fatalf("handled request_id = %v, want req-1 preserved", v)
	}
	if !placementHoldPending(t, pool, hostID) {
		t.Fatal("placement hold was dropped by an unrelated capacity resend")
	}

	// A report with no access at all clears access AND the hold, keeping the
	// handled-request-id record.
	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{"DP-1", "HDMI-A-1"}, InputDevices: []InputDevicePath{},
	}); err != nil {
		t.Fatalf("upsert 3: %v", err)
	}
	got, err = store.GetCapabilities(ctx, hostID)
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if got.Access != nil {
		t.Fatalf("access = %v, want cleared", got.Access)
	}
	if placementHoldPending(t, pool, hostID) {
		t.Fatal("placement hold survived a report with no access")
	}
	if v := handledRequestID(t, pool, hostID); v != "req-1" {
		t.Fatalf("handled request_id = %v, want req-1 kept", v)
	}
}

// ClearAccess (the no-console_capabilities-at-all path) must remove only
// `access` and the placement hold, leaving connectors/etc and the handled-id
// record untouched.
func TestClearAccessLeavesOtherFieldsAlone(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-clearaccess")
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.UpsertCapabilities(ctx, hostID, Capabilities{
		Connectors: []string{"DP-1"}, InputDevices: []InputDevicePath{{Path: "/dev/input/event1", Label: "Keyboard"}},
		Access: &Access{State: "on", Target: boolPtr(true), RequestID: strPtr("req-9"), Summary: "on"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetPlacementHoldPending(ctx, hostID, true); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	markHandled(t, pool, hostID, "req-9")

	if err := store.ClearAccess(ctx, hostID); err != nil {
		t.Fatalf("clear access: %v", err)
	}

	got, err := store.GetCapabilities(ctx, hostID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Access != nil {
		t.Fatalf("access = %v, want cleared", got.Access)
	}
	if len(got.Connectors) != 1 || got.Connectors[0] != "DP-1" {
		t.Fatalf("connectors = %v, want [DP-1] untouched", got.Connectors)
	}
	if len(got.InputDevices) != 1 {
		t.Fatalf("input devices = %v, want untouched", got.InputDevices)
	}
	if placementHoldPending(t, pool, hostID) {
		t.Fatal("placement hold survived ClearAccess")
	}
	if v := handledRequestID(t, pool, hostID); v != "req-9" {
		t.Fatalf("handled request_id = %v, want req-9 kept", v)
	}
}

// ResetEnabledOnRestoredAccess is the once-per-request_id reset: a restored
// report whose target matches the stored `enabled` flips it to !target,
// stamps updated_by NULL, and marks the request_id handled; a replay of the
// same request_id (every capacity resend carries the current access) is a
// no-op, so an admin's later "try again" PATCH is never undone.
func TestResetEnabledOnRestoredAccessAppliesOnce(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-restored")
	store := NewStore(pool)
	ctx := context.Background()

	adminID := seedTestUser(t, pool, "restore-admin")
	if err := store.Upsert(ctx, hostID, map[string]any{"enabled": true}, &adminID); err != nil {
		t.Fatalf("seed console_config: %v", err)
	}

	access := Access{State: "restored", Target: boolPtr(true), RequestID: strPtr("req-restore-1"), Reason: strPtr("unhealthy"), Summary: "restored"}
	applied, resolved, err := store.ResetEnabledOnRestoredAccess(ctx, hostID, access)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if !applied {
		t.Fatal("reset did not apply on the first report")
	}
	if resolved.Enabled {
		t.Fatal("resolved.Enabled = true, want false (the host kept !target)")
	}
	updatedBy, cfgEnabled := rawConsoleConfig(t, pool, hostID)
	if updatedBy != nil {
		t.Fatalf("updated_by = %v, want NULL after a system reset", *updatedBy)
	}
	if cfgEnabled {
		t.Fatal("stored enabled did not flip to false")
	}

	// Replay: the same request_id, agent resending the same settled report.
	applied2, resolved2, err := store.ResetEnabledOnRestoredAccess(ctx, hostID, access)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if applied2 {
		t.Fatal("a replayed request_id must not apply twice")
	}
	if resolved2.Enabled {
		t.Fatal("replay must not change the resolved enabled it reports back")
	}

	// The admin retries: PATCH sets enabled back to true (a fresh decision,
	// distinct from the request_id above).
	if err := store.Upsert(ctx, hostID, map[string]any{"enabled": true}, &adminID); err != nil {
		t.Fatalf("admin retry: %v", err)
	}
	// The stale report (same request_id) must not undo the admin's retry.
	applied3, resolved3, err := store.ResetEnabledOnRestoredAccess(ctx, hostID, access)
	if err != nil {
		t.Fatalf("stale replay after retry: %v", err)
	}
	if applied3 {
		t.Fatal("a stale request_id replay undid the admin's retry")
	}
	if !resolved3.Enabled {
		t.Fatal("admin's retried enabled=true was undone by a stale restored replay")
	}
}

// A restored report whose target no longer matches the stored `enabled`
// (the admin already moved on) is a no-op, not an error.
func TestResetEnabledOnRestoredAccessNoopOnTargetMismatch(t *testing.T) {
	pool := testPool(t)
	hostID := seedTestHost(t, pool, "console-test-restored-mismatch")
	store := NewStore(pool)
	ctx := context.Background()

	if err := store.Upsert(ctx, hostID, map[string]any{"enabled": false}, nil); err != nil {
		t.Fatalf("seed console_config: %v", err)
	}

	access := Access{State: "restored", Target: boolPtr(true), RequestID: strPtr("req-mismatch"), Summary: "restored"}
	applied, resolved, err := store.ResetEnabledOnRestoredAccess(ctx, hostID, access)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if applied {
		t.Fatal("target (true) did not match stored enabled (false); must not apply")
	}
	if resolved.Enabled {
		t.Fatal("resolved.Enabled should still read the stored value (false)")
	}
}
