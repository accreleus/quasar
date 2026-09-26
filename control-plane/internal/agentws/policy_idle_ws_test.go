package agentws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

// Once the durable current-connection journal and legacy map are complete,
// a reviewed idle offer must not depend on an in-memory delivery ID.
func TestIdleOfferAfterCompletedCurrentJournal(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000213"
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection,
		map[string]int{"typed_settings": 2, "execution_journal": 1, "idle_apply": 1},
		[]string{"hardware", "idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, []string{"hardware", "idle_timeout_secs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SavePolicy(ctx, hostID, "0", map[string]hostcfg.PolicyChoice{
		"encoder": {Source: "explicit", Value: "openh264"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetInitialDelivery(ctx, hostID, connection, "00000000-0000-4000-8000-000000000214"); err != nil || !ok {
		t.Fatalf("set initial map delivery: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, connection, "00000000-0000-4000-8000-000000000214"); err != nil || !ok {
		t.Fatalf("ack initial map: ok=%v err=%v", ok, err)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, connection, nil,
		map[string]hostcfg.PolicySnapshot{"hardware": {Kind: "seeded", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection,
		json.RawMessage(`{"encoder":"openh264","render_node":"","cuda_device":0}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveIdleHeartbeat(ctx, hostID, connection, []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET source_preparation='{"steam":{"images":[]}}'::jsonb,
		source_preparation_reported_at=now(),last_registered_at=now()-interval '1 minute' WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewIdleApply(ctx, hostID, "hardware")
	if err != nil || preview == nil || !preview.Available {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	if _, err := store.ApproveIdleApply(ctx, hostID, "hardware", hostcfg.ApprovalReview{
		ExpectedRevision: preview.Revision, ContentSHA256: preview.ContentSHA256,
		PrerequisitesSHA256: preview.PrerequisitesSHA256, Prerequisites: preview.Prerequisites,
		ApprovalBootIncarnation: preview.ApprovalBootIncarnation, ApprovalReviewID: preview.ApprovalReviewID,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, store, nil, boot)
	t.Cleanup(h.Close)
	c := newConn(hostID, nil)
	c.policyTyped = true
	c.policyIdle = true
	c.policyAcknowledged.Store(true)
	c.policyInventoryDone.Store(true)
	c.bootIncarnation = h.bootIncarnation
	c.connectionIncarnation = connection
	registry.add(c)
	t.Cleanup(func() { registry.remove(c) })
	h.offerIdlePolicy(ctx, c)
	select {
	case raw := <-c.out:
		var offer map[string]any
		if err := json.Unmarshal(raw, &offer); err != nil || offer["type"] != "config_policy_offer" {
			t.Fatalf("idle offer = %+v, err=%v", offer, err)
		}
	default:
		t.Fatal("durably ready idle approval was not offered")
	}
}
