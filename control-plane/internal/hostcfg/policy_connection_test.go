package hostcfg

import (
	"context"
	"encoding/json"
	"testing"
)

func TestPolicyDeliveryGateAndLegacyWriterRemainSeparate(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	first := "00000000-0000-4000-8000-000000000101"
	if _, err := store.BeginPolicyConnection(ctx, hostID, first, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, first, []string{"idle_timeout_secs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveLegacyPatch(ctx, hostID, map[string]any{"idle_timeout_secs": float64(900), "gop": float64(90)}, nil); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000102"
	settings, ok, err := store.PrepareLegacyDelivery(ctx, hostID, first, id, []string{"idle_timeout_secs"})
	if err != nil || !ok {
		t.Fatalf("prepare: ok=%v err=%v", ok, err)
	}
	if settings["gop"] != float64(90) || settings["idle_timeout_secs"] != nil {
		t.Fatalf("legacy writer map = %+v", settings)
	}
	if ack, err := store.AcknowledgeInitialDelivery(ctx, hostID, first, "00000000-0000-4000-8000-000000000103"); err != nil || ack {
		t.Fatalf("wrong delivery acknowledged: %v %v", ack, err)
	}
	if ack, err := store.AcknowledgeInitialDelivery(ctx, hostID, first, id); err != nil || !ack {
		t.Fatalf("exact delivery not acknowledged: %v %v", ack, err)
	}
	second := "00000000-0000-4000-8000-000000000104"
	if gated, err := store.BeginPolicyConnection(ctx, hostID, second, nil, nil, false); err != nil || !gated {
		t.Fatalf("downgrade should stay gated: %v %v", gated, err)
	}
	if ack, err := store.AcknowledgeInitialDelivery(ctx, hostID, first, id); err != nil || ack {
		t.Fatalf("displaced socket lifted gate: %v %v", ack, err)
	}
	legacy, err := store.LegacyOwnedOverrides(ctx, hostID, nil)
	if err != nil || legacy["idle_timeout_secs"] != nil || legacy["gop"] != float64(90) {
		t.Fatalf("downgrade map = %+v, err=%v", legacy, err)
	}
}

func TestDeploymentBaselineIsCurrentConnectionEvidence(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000111"
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, []string{"idle_timeout_secs"}); err != nil {
		t.Fatal(err)
	}
	view, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "deployment"}}, nil)
	if err != nil || view.Groups["idle_timeout_secs"].DesiredDigest != nil {
		t.Fatalf("offline deployment intent = %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection, json.RawMessage(`{"idle_timeout_secs":120,"abr_floor_kbps":null}`)); err != nil {
		t.Fatal(err)
	}
	if baseline, err := store.DeploymentSettingsForConnection(ctx, hostID, "00000000-0000-4000-8000-000000000112"); err != nil || baseline != nil {
		t.Fatalf("previous-connection baseline used: %+v, err=%v", baseline, err)
	}
	id := "00000000-0000-4000-8000-000000000113"
	if _, ok, err := store.PrepareLegacyDelivery(ctx, hostID, connection, id, []string{"idle_timeout_secs"}); err != nil || !ok {
		t.Fatalf("delivery: %v %v", ok, err)
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, connection, id); err != nil || !ok {
		t.Fatalf("ack: %v %v", ok, err)
	}
	offer, err := store.NextSessionOffer(ctx, hostID, "00000000-0000-4000-8000-000000000114", "00000000-0000-4000-8000-000000000115", connection)
	if err != nil || offer == nil || offer.ResolvedSettings["idle_timeout_secs"] != float64(120) || len(offer.Prerequisites) != 1 {
		t.Fatalf("deployment offer = %+v, err=%v", offer, err)
	}
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection, json.RawMessage(`{"idle_timeout_secs":null}`)); err != nil {
		t.Fatal(err)
	}
	if baseline, err := store.DeploymentSettingsForConnection(ctx, hostID, connection); err != nil || baseline != nil {
		t.Fatalf("malformed report retained old baseline: %+v, err=%v", baseline, err)
	}
}
