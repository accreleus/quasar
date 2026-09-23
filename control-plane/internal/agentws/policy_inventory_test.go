package agentws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
)

func TestFreshV2EmptyGroupSeedMapAckKeepsAdmissionClosed(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, "test-token", log, nil, nil, nil, store, nil)
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
		"type": "register", "node_name": "fresh-v2-seed-test", "agent_version": "test",
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
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}},
		"config_policy_accepted_groups": []string{},
	}
	if err := ws.WriteJSON(capacity); err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	for {
		if err := ws.ReadJSON(&request); err != nil {
			t.Fatal(err)
		}
		if request["type"] == "config_policy_journal_inventory_request" {
			break
		}
	}
	if err := ws.WriteJSON(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000255", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "active_snapshots": map[string]any{}, "entries": []any{},
	}); err != nil {
		t.Fatal(err)
	}
	var delivery map[string]any
	for {
		if err := ws.ReadJSON(&delivery); err != nil {
			t.Fatal(err)
		}
		if delivery["type"] == "config_update" && delivery["settings_delivery_id"] != nil {
			break
		}
	}
	capacity["config_policy_legacy_map_applied_id"] = delivery["settings_delivery_id"]
	capacity["host"] = map[string]any{"cpu_cores": 9, "mem_mb": 32000}
	if err := ws.WriteJSON(capacity); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var cores int
		if err := pool.QueryRow(context.Background(), `SELECT cpu_cores FROM hosts WHERE id=$1::uuid`, hostID).Scan(&cores); err != nil {
			t.Fatal(err)
		}
		if cores == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("seed-map acknowledgement capacity not processed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var gated bool
	if err := pool.QueryRow(context.Background(), `SELECT config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid`, hostID).Scan(&gated); err != nil || !gated {
		t.Fatalf("fresh v2 admitted after seed-map acknowledgement: gated=%v err=%v", gated, err)
	}
}

func TestBlockedPolicyInventoryRefreshesOnCurrentConnection(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000209"
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET config_policy_versions='{"typed_settings":2}'::jsonb WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	if err := store.HoldPolicyConnection(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, "test-token", log, registry, nil, nil, store, nil)
	t.Cleanup(h.Close)
	c := newConn(hostID, nil)
	c.policyTyped = true
	c.connectionIncarnation = connection
	c.bootIncarnation = "00000000-0000-4000-8000-000000000210"
	c.policyInventoryDone.Store(true)
	c.policyInventoryBlocked.Store(true)
	c.policyDeliveryID = "00000000-0000-4000-8000-000000000212"
	c.policyInitialMapApplied.Store(true)
	registry.add(c)
	t.Cleanup(func() { registry.remove(c) })
	if err := h.restartPolicyInventory(ctx, c); err != nil {
		t.Fatal(err)
	}
	if c.policyInventoryID == "" || c.policyInventoryBlocked.Load() || c.policyRefreshPending ||
		c.policyDeliveryID != "" || c.policyInitialMapApplied.Load() {
		t.Fatal("blocked pending inventory did not request a fresh snapshot")
	}
	select {
	case raw := <-c.out:
		var request ConfigPolicyInventoryRequest
		if err := json.Unmarshal(raw, &request); err != nil || request.Type != "config_policy_journal_inventory_request" || request.InventoryID != c.policyInventoryID {
			t.Fatalf("refresh request = %+v, err=%v", request, err)
		}
	default:
		t.Fatal("blocked pending inventory remained stranded")
	}
}

func TestPolicyInventoryMatchingHistoricalAttemptPermitsInitialMap(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000201"
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, []string{"idle_timeout_secs"}); err != nil {
		t.Fatal(err)
	}
	view, err := store.SavePolicy(ctx, hostID, "0", map[string]hostcfg.PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	group := view.Groups["idle_timeout_secs"]
	if _, err := store.StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, "test-token", log, registry, nil, nil, store, nil)
	t.Cleanup(h.Close)
	c := newConn(hostID, nil)
	c.policyTyped = true
	c.policyAccepted = []string{"idle_timeout_secs"}
	c.policyAcknowledged.Store(true)
	c.bootIncarnation = "00000000-0000-4000-8000-000000000202"
	c.connectionIncarnation = connection
	c.policyInventoryID = "00000000-0000-4000-8000-000000000203"
	registry.add(c)
	t.Cleanup(func() { registry.remove(c) })
	entry := ConfigPolicyStateMsg{
		Type: "config_policy_state", AttemptID: "00000000-0000-4000-8000-000000000204", HostID: hostID,
		Group: "idle_timeout_secs", Revision: group.DesiredRevision, ContentSHA256: *group.DesiredDigest, Scope: "next_session",
		GrantBootIncarnation:       "00000000-0000-4000-8000-000000000205",
		GrantConnectionIncarnation: "00000000-0000-4000-8000-000000000206",
		JournalSequence:            "1", Phase: "accepted",
	}
	page, err := json.Marshal(ConfigPolicyInventoryPage{
		Type: "config_policy_journal_inventory_page", InventoryID: c.policyInventoryID,
		SnapshotID:        "00000000-0000-4000-8000-000000000207",
		RevisionHighWater: map[string]string{"idle_timeout_secs": group.DesiredRevision},
		ActiveSnapshots: map[string]struct {
			Kind   string `json:"kind"`
			Digest string `json:"digest"`
		}{"idle_timeout_secs": {Kind: "seeded", Digest: strings.Repeat("a", 64)}},
		Entries: []ConfigPolicyStateMsg{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.acceptPolicyInventoryPage(ctx, c, page); err != nil {
		t.Fatal(err)
	}
	if !c.policyInventoryDone.Load() || c.policyInventoryBlocked.Load() || c.policyDeliveryID == "" {
		t.Fatalf("matching historical attempt should allow map delivery: done=%v blocked=%v delivery=%q", c.policyInventoryDone.Load(), c.policyInventoryBlocked.Load(), c.policyDeliveryID)
	}
	if got := registry.PolicyActiveSnapshots(hostID, connection); got != nil {
		t.Fatalf("unfinished attempt allowed a competing offer: %+v", got)
	}
	if !registry.PolicyRestartConflict(hostID) {
		t.Fatal("unfinished attempt allowed a legacy restart edit")
	}
	if !policyGrantMatches(c, entry) {
		t.Fatal("inventoried historical grant was rejected after reconnect")
	}
	if fresh, err := c.acceptPolicySequence(entry); err != nil || fresh {
		t.Fatalf("replayed inventory entry: fresh=%v err=%v", fresh, err)
	}
	terminal := entry
	terminal.JournalSequence = "2"
	terminal.Phase = "applied"
	if fresh, err := c.acceptPolicySequence(terminal); err != nil || !fresh {
		t.Fatalf("new terminal sequence: fresh=%v err=%v", fresh, err)
	}
	if fresh, err := c.acceptPolicySequence(entry); err != nil || fresh {
		t.Fatalf("older accepted frame reopened terminal: fresh=%v err=%v", fresh, err)
	}
	conflict := terminal
	conflict.Phase = "failed"
	if _, err := c.acceptPolicySequence(conflict); err == nil {
		t.Fatal("conflicting equal journal sequence was accepted")
	}
	firstReject := entry
	firstReject.JournalSequence = "0"
	firstReject.Phase = "failed"
	firstReject.Error = json.RawMessage(`"host_busy"`)
	secondReject := firstReject
	secondReject.Error = json.RawMessage(`"prerequisite_mismatch"`)
	for _, rejection := range []ConfigPolicyStateMsg{firstReject, secondReject} {
		if fresh, err := c.acceptPolicySequence(rejection); err != nil || !fresh {
			t.Fatalf("delivery rejection was mistaken for journal sequence: fresh=%v err=%v", fresh, err)
		}
	}
	select {
	case raw := <-c.out:
		var sent ConfigUpdateCmd
		if err := json.Unmarshal(raw, &sent); err != nil || sent.Type != "config_update" || sent.SettingsDeliveryID != c.policyDeliveryID {
			t.Fatalf("initial full map = %+v, err=%v", sent, err)
		}
	default:
		t.Fatal("matching historical attempt did not receive initial full map")
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, connection, c.policyDeliveryID); err != nil || !ok {
		t.Fatalf("current map could not lift gate: ok=%v err=%v", ok, err)
	}
	c.policyInitialMapApplied.Store(true)
	if err := store.CompleteJournalReconciliation(ctx, hostID, connection, c.rh05RestartEntries, c.rh05Snapshots); err != nil {
		t.Fatal(err)
	}
	if err := h.restartPolicyInventory(ctx, c); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-c.out:
		var request ConfigPolicyInventoryRequest
		if err := json.Unmarshal(raw, &request); err != nil || request.Type != "config_policy_journal_inventory_request" {
			t.Fatalf("refresh request = %+v, err=%v", request, err)
		}
	default:
		t.Fatal("current connection did not request fresh journal")
	}
	refreshPage, err := json.Marshal(ConfigPolicyInventoryPage{
		Type: "config_policy_journal_inventory_page", InventoryID: c.policyInventoryID,
		SnapshotID:        "00000000-0000-4000-8000-000000000208",
		RevisionHighWater: map[string]string{"idle_timeout_secs": group.DesiredRevision},
		ActiveSnapshots: map[string]struct {
			Kind   string `json:"kind"`
			Digest string `json:"digest"`
		}{"idle_timeout_secs": {Kind: "seeded", Digest: strings.Repeat("a", 64)}},
		Entries: []ConfigPolicyStateMsg{entry},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.acceptPolicyInventoryPage(ctx, c, refreshPage); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-c.out:
		t.Fatalf("refresh resent initial map after acknowledged gate: %s", raw)
	default:
	}
	if !c.policyInitialMapApplied.Load() {
		t.Fatal("refresh forgot already acknowledged map")
	}
	if err := store.HoldPolicyConnection(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	priorDelivery := c.policyDeliveryID
	if err := h.restartPolicyInventory(ctx, c); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-c.out:
		var request ConfigPolicyInventoryRequest
		if err := json.Unmarshal(raw, &request); err != nil || request.Type != "config_policy_journal_inventory_request" {
			t.Fatalf("gated refresh request = %+v, err=%v", request, err)
		}
	default:
		t.Fatal("open delivery gate did not request a fresh journal")
	}
	var refreshDoc map[string]any
	if err := json.Unmarshal(refreshPage, &refreshDoc); err != nil {
		t.Fatal(err)
	}
	refreshDoc["inventory_id"] = c.policyInventoryID
	refreshDoc["snapshot_id"] = "00000000-0000-4000-8000-000000000211"
	gatedPage, err := json.Marshal(refreshDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.acceptPolicyInventoryPage(ctx, c, gatedPage); err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-c.out:
		var sent ConfigUpdateCmd
		if err := json.Unmarshal(raw, &sent); err != nil || sent.Type != "config_update" ||
			sent.SettingsDeliveryID == "" || sent.SettingsDeliveryID == priorDelivery {
			t.Fatalf("reopened gate did not get fresh map: %+v, err=%v", sent, err)
		}
	default:
		t.Fatal("reopened delivery gate stranded without map")
	}
}
