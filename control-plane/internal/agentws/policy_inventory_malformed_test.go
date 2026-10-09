package agentws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

type inventoryHarness struct {
	pool   *pgxpool.Pool
	store  *hostcfg.Store
	h      *Handler
	c      *conn
	hostID string
}

// newInventoryHarness opens a pending journal reconciliation for a fresh host
// and returns a typed connection waiting for its first inventory page.
func newInventoryHarness(t *testing.T) *inventoryHarness {
	t.Helper()
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000301"
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, store, nil)
	t.Cleanup(h.Close)
	c := newConn(hostID, nil)
	c.policyTyped = true
	c.bootIncarnation = "00000000-0000-4000-8000-000000000302"
	c.connectionIncarnation = connection
	c.policyInventoryID = "00000000-0000-4000-8000-000000000303"
	registry.add(c)
	t.Cleanup(func() { registry.remove(c) })
	return &inventoryHarness{pool: pool, store: store, h: h, c: c, hostID: hostID}
}

func (x *inventoryHarness) entry(n int, group, scope string) ConfigPolicyStateMsg {
	return ConfigPolicyStateMsg{
		Type: "config_policy_state", AttemptID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), HostID: x.hostID,
		Group: group, Revision: "1", ContentSHA256: strings.Repeat("b", 64), Scope: scope,
		GrantBootIncarnation:       "00000000-0000-4000-8000-000000000304",
		GrantConnectionIncarnation: "00000000-0000-4000-8000-000000000305",
		JournalSequence:            "1", Phase: "failed",
	}
}

func (x *inventoryHarness) failedEntries(from, n int) []ConfigPolicyStateMsg {
	entries := make([]ConfigPolicyStateMsg, n)
	for i := range entries {
		entries[i] = x.entry(from+i, "idle_timeout_secs", "next_session")
	}
	return entries
}

func (x *inventoryHarness) page(t *testing.T, snapshot string, cursor, next *string, high map[string]string, entries []ConfigPolicyStateMsg) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": x.c.policyInventoryID,
		"snapshot_id": snapshot, "cursor": cursor, "next_cursor": next,
		"revision_high_water": high, "active_snapshots": map[string]any{}, "entries": entries,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// assertStillRestricted: the reconciliation restriction is held, the host is
// draining, and the journal gate never completed.
func (x *inventoryHarness) assertStillRestricted(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	var restrictions int
	if err := x.pool.QueryRow(ctx, `SELECT count(*) FROM host_admission_restrictions
		WHERE host_id=$1::uuid AND owner_kind='reconciliation'`, x.hostID).Scan(&restrictions); err != nil || restrictions != 1 {
		t.Fatalf("reconciliation restriction = %d, err=%v", restrictions, err)
	}
	var status string
	if err := x.pool.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid`, x.hostID).Scan(&status); err != nil || status != "draining" {
		t.Fatalf("host status = %q, err=%v", status, err)
	}
	if gate, err := x.store.JournalGate(ctx, x.hostID); err != nil || gate == "complete" {
		t.Fatalf("journal gate = %q, err=%v", gate, err)
	}
}

func TestScopeMismatchedFailedEntryBlocksInventory(t *testing.T) {
	// hardware is a restart-scope group; "next_session" disagrees with the catalog.
	for _, tc := range []struct{ group, scope string }{
		{"hardware", "next_session"},
		{"idle_timeout_secs", "restart"}, // restart branch, same outcome
		{"idle_timeout_secs", ""},
		{"idle_timeout_secs", "bogus"},
	} {
		t.Run(tc.group+"/"+tc.scope, func(t *testing.T) {
			x := newInventoryHarness(t)
			entry := x.entry(1, tc.group, tc.scope)
			page := x.page(t, "00000000-0000-4000-8000-000000000306", nil, nil, map[string]string{}, []ConfigPolicyStateMsg{entry})
			if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, page); err != nil {
				t.Fatal(err)
			}
			if !x.c.policyInventoryUnknown || !x.c.policyInventoryBlocked.Load() {
				t.Fatalf("scope mismatch not blocked: unknown=%v blocked=%v", x.c.policyInventoryUnknown, x.c.policyInventoryBlocked.Load())
			}
			x.assertStillRestricted(t)
		})
	}
}

func TestMatchingScopeFailedEntryIsTerminalHistory(t *testing.T) {
	x := newInventoryHarness(t)
	entry := x.entry(1, "idle_timeout_secs", "next_session")
	page := x.page(t, "00000000-0000-4000-8000-000000000306", nil, nil, map[string]string{}, []ConfigPolicyStateMsg{entry})
	if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, page); err != nil {
		t.Fatal(err)
	}
	if x.c.policyInventoryBlocked.Load() {
		t.Fatal("matching-scope failed entry blocked the inventory")
	}
}

func TestMalformedMultiPageInventoryKeepsRestriction(t *testing.T) {
	const snapshot = "00000000-0000-4000-8000-000000000306"
	str := func(s string) *string { return &s }
	first := func(x *inventoryHarness, t *testing.T) {
		t.Helper()
		p := x.page(t, snapshot, nil, str("256"), map[string]string{}, x.failedEntries(1, 256))
		if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, p); err != nil {
			t.Fatalf("well-formed first page rejected: %v", err)
		}
		x.assertStillRestricted(t)
	}
	for _, tc := range []struct {
		name string
		run  func(x *inventoryHarness, t *testing.T) []byte
	}{
		{"short non-final page", func(x *inventoryHarness, t *testing.T) []byte {
			return x.page(t, snapshot, nil, str("255"), map[string]string{}, x.failedEntries(1, 255))
		}},
		{"skipped cursor", func(x *inventoryHarness, t *testing.T) []byte {
			return x.page(t, snapshot, nil, str("257"), map[string]string{}, x.failedEntries(1, 256))
		}},
		{"snapshot id changes", func(x *inventoryHarness, t *testing.T) []byte {
			first(x, t)
			return x.page(t, "00000000-0000-4000-8000-000000000307", str("256"), nil, map[string]string{}, x.failedEntries(257, 1))
		}},
		{"header changes", func(x *inventoryHarness, t *testing.T) []byte {
			first(x, t)
			return x.page(t, snapshot, str("256"), nil, map[string]string{"hardware": "2"}, x.failedEntries(257, 1))
		}},
		{"cursor repeats on later page", func(x *inventoryHarness, t *testing.T) []byte {
			first(x, t)
			return x.page(t, snapshot, str("256"), str("256"), map[string]string{}, x.failedEntries(257, 256))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newInventoryHarness(t)
			page := tc.run(x, t)
			if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, page); err == nil {
				t.Fatal("malformed page accepted")
			}
			if x.c.policyInventoryDone.Load() {
				t.Fatal("malformed inventory marked done")
			}
			x.assertStillRestricted(t)
		})
	}
}

func TestMalformedInventoryPageClosesConnection(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteJSON(map[string]any{
		"type": "register", "node_name": "malformed-inventory-test", "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": "test-token"},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1},
		"config_policy_groups":   []string{},
	}); err != nil {
		t.Fatal(err)
	}
	var registered map[string]any
	if err := ws.ReadJSON(&registered); err != nil {
		t.Fatal(err)
	}
	hostID, _ := registered["host_id"].(string)
	if hostID == "" {
		t.Fatalf("registered without host id: %+v", registered)
	}
	if err := ws.WriteJSON(map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}},
		"config_policy_accepted_groups": []string{},
	}); err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	for request["type"] != "config_policy_journal_inventory_request" {
		if err := ws.ReadJSON(&request); err != nil {
			t.Fatal(err)
		}
	}
	// A non-final page must carry exactly 256 entries.
	next := "1"
	if err := ws.WriteJSON(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000308", "cursor": nil, "next_cursor": &next,
		"revision_high_water": map[string]string{}, "active_snapshots": map[string]any{}, "entries": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	for {
		var msg map[string]any
		if err := ws.ReadJSON(&msg); err != nil {
			break // closed
		}
		if msg["type"] == "config_policy_journal_inventory_request" && msg["cursor"] != nil {
			t.Fatal("short non-final page was followed to its next cursor")
		}
	}
	var restrictions int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM host_admission_restrictions
		WHERE host_id=$1::uuid AND owner_kind='reconciliation'`, hostID).Scan(&restrictions); err != nil || restrictions != 1 {
		t.Fatalf("reconciliation restriction = %d, err=%v", restrictions, err)
	}
	if gate, err := store.JournalGate(context.Background(), hostID); err != nil || gate == "complete" {
		t.Fatalf("journal gate = %q, err=%v", gate, err)
	}
}

func TestKnownRestartAttemptReportedUnderAnotherGroupKeepsHold(t *testing.T) {
	x := newInventoryHarness(t)
	ctx := context.Background()
	attemptID := "00000000-0000-4000-8000-000000000401"
	digest := strings.Repeat("a", 64)
	var boot string
	if err := x.pool.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&boot); err != nil {
		t.Fatal(err)
	}
	// A cancelled, never-started hardware grant: the case the idle-apply hold exists for.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO host_config_approvals(id,host_id,group_key,revision,approved_digest,prerequisites_digest,boot_incarnation,review_id,expires_at,state)
			VALUES($1::uuid,$2::uuid,'hardware',1,$3,$3,$4::uuid,'00000000-0000-4000-8000-000000000402',now()+interval '1 hour','cancel_pending')`,
			[]any{attemptID, x.hostID, digest, boot}},
		{`INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,phase)
			VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,'offered')`,
			[]any{attemptID, x.hostID, digest, boot}},
		{`INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
			VALUES($2::uuid,'idle_apply',$1::uuid,'idle_configuration')`,
			[]any{attemptID, x.hostID}},
	} {
		if _, err := x.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	// The agent reports that attempt id under a group whose catalog scope matches.
	entry := x.entry(1, "idle_timeout_secs", "next_session")
	entry.AttemptID = attemptID
	page := x.page(t, "00000000-0000-4000-8000-000000000306", nil, nil, map[string]string{}, []ConfigPolicyStateMsg{entry})
	if err := x.h.acceptPolicyInventoryPage(ctx, x.c, page); err != nil {
		t.Fatal(err)
	}
	if !x.c.policyInventoryUnknown || !x.c.policyInventoryBlocked.Load() {
		t.Fatalf("regrouped attempt not blocked: unknown=%v blocked=%v", x.c.policyInventoryUnknown, x.c.policyInventoryBlocked.Load())
	}
	var phase, approval string
	if err := x.pool.QueryRow(ctx, `SELECT phase FROM host_config_attempts WHERE id=$1::uuid`, attemptID).Scan(&phase); err != nil || phase != "offered" {
		t.Fatalf("attempt phase = %q, err=%v", phase, err)
	}
	if err := x.pool.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE id=$1::uuid`, attemptID).Scan(&approval); err != nil || approval != "cancel_pending" {
		t.Fatalf("approval state = %q, err=%v", approval, err)
	}
	var holds int
	if err := x.pool.QueryRow(ctx, `SELECT count(*) FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, x.hostID, attemptID).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("idle-apply hold = %d, err=%v", holds, err)
	}
	x.assertStillRestricted(t)
}

func TestOverCapPolicyInventoryIsRefused(t *testing.T) {
	const snapshot = "00000000-0000-4000-8000-000000000306"
	t.Run("last page reaches the cap", func(t *testing.T) {
		x := newInventoryHarness(t)
		cursor := strconv.Itoa(maxPolicyInventoryEntries - 256)
		x.c.policyInventoryCursor = &cursor
		page := x.page(t, snapshot, &cursor, nil, map[string]string{}, x.failedEntries(1, 256))
		if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, page); err != nil || !x.c.policyInventoryDone.Load() {
			t.Fatalf("inventory at the cap: done=%v err=%v", x.c.policyInventoryDone.Load(), err)
		}
	})
	t.Run("one entry past it", func(t *testing.T) {
		x := newInventoryHarness(t)
		cursor := strconv.Itoa(maxPolicyInventoryEntries)
		x.c.policyInventoryCursor = &cursor
		page := x.page(t, snapshot, &cursor, nil, map[string]string{}, x.failedEntries(1, 1))
		if err := x.h.acceptPolicyInventoryPage(context.Background(), x.c, page); err == nil {
			t.Fatal("over-cap inventory accepted")
		}
		if !x.c.policyInventoryUnknown || !x.c.policyInventoryBlocked.Load() || x.c.policyInventoryDone.Load() {
			t.Fatalf("over-cap inventory: unknown=%v blocked=%v done=%v", x.c.policyInventoryUnknown, x.c.policyInventoryBlocked.Load(), x.c.policyInventoryDone.Load())
		}
		if len(x.c.policySequence) != 0 {
			t.Fatalf("over-cap page cached %d entries", len(x.c.policySequence))
		}
		x.assertStillRestricted(t)
	})
}
