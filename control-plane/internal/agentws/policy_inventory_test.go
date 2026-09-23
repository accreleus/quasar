package agentws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

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
	if got := registry.PolicyActiveSnapshot(hostID, connection); got != nil {
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
}
