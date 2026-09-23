package hostcfg

import (
	"context"
	"encoding/json"
	"strings"
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

func TestFreshV2SeedRequiresReconnectBeforeAdmission(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	first := "00000000-0000-4000-8000-000000000151"
	if gated, err := store.BeginPolicyConnection(ctx, hostID, first, map[string]int{"typed_settings": 2}, []string{}, true); err != nil || !gated {
		t.Fatalf("first connection: gated=%v err=%v", gated, err)
	}
	if ok, err := store.ConfirmPolicyGroups(ctx, hostID, first, []string{}); err != nil || !ok {
		t.Fatalf("empty group echo: ok=%v err=%v", ok, err)
	}
	id := "00000000-0000-4000-8000-000000000152"
	if _, ok, err := store.PrepareLegacyDelivery(ctx, hostID, first, id, nil); err != nil || !ok {
		t.Fatalf("full seed map: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, first, id); err != nil || !ok {
		t.Fatalf("seed map ack: ok=%v err=%v", ok, err)
	}
	var available bool
	if err := pool.QueryRow(ctx, `SELECT config_policy_gate_connection IS NULL FROM hosts WHERE id=$1::uuid`, hostID).Scan(&available); err != nil || available {
		t.Fatalf("first connection admitted before seed reconnect: available=%v err=%v", available, err)
	}
	second := "00000000-0000-4000-8000-000000000153"
	if gated, err := store.BeginPolicyConnection(ctx, hostID, second, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil || !gated {
		t.Fatalf("seeded reconnect: gated=%v err=%v", gated, err)
	}
	if ok, err := store.ConfirmPolicyGroups(ctx, hostID, second, []string{"idle_timeout_secs"}); err != nil || !ok {
		t.Fatalf("seeded group echo: ok=%v err=%v", ok, err)
	}
	secondID := "00000000-0000-4000-8000-000000000154"
	if _, ok, err := store.PrepareLegacyDelivery(ctx, hostID, second, secondID, []string{"idle_timeout_secs"}); err != nil || !ok {
		t.Fatalf("seeded map: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, second, secondID); err != nil || !ok {
		t.Fatalf("seeded map ack: ok=%v err=%v", ok, err)
	}
	if err := pool.QueryRow(ctx, `SELECT config_policy_gate_connection IS NULL FROM hosts WHERE id=$1::uuid`, hostID).Scan(&available); err != nil || !available {
		t.Fatalf("seeded reconnect remained gated: available=%v err=%v", available, err)
	}
}

func TestUpgradeRequiredRemedyDistinguishesLegacyWriterFromStickyOwnership(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	if _, err := store.SaveLegacyPatch(ctx, hostID, map[string]any{"idle_timeout_secs": float64(900)}, nil); err != nil {
		t.Fatal(err)
	}
	view, err := store.GetPolicy(ctx, hostID)
	if err != nil || !strings.Contains(*view.Groups["idle_timeout_secs"].Remedy, "legacy writer remains active") {
		t.Fatalf("never-owned remedy = %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET config_policy_ever_owned_groups='["idle_timeout_secs"]'::jsonb WHERE id=$1::uuid`, hostID); err != nil {
		t.Fatal(err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil || !strings.Contains(*view.Groups["idle_timeout_secs"].Remedy, "legacy value is not sent") {
		t.Fatalf("sticky downgrade remedy = %+v, err=%v", view.Groups["idle_timeout_secs"], err)
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
	if err != nil || view.Groups["idle_timeout_secs"].DesiredDigest != nil || view.Groups["idle_timeout_secs"].Remedy == nil || !strings.Contains(*view.Groups["idle_timeout_secs"].Remedy, "baseline_unavailable") {
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
	snapshots := map[string]PolicySnapshot{"idle_timeout_secs": {Kind: "seeded", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	offers, err := store.NextSessionOffers(ctx, hostID, "00000000-0000-4000-8000-000000000115", connection, snapshots, func() string { return "00000000-0000-4000-8000-000000000114" })
	var offer *PolicyOffer
	if len(offers) == 1 {
		offer = offers[0]
	}
	if err != nil || offer == nil || offer.ResolvedSettings["idle_timeout_secs"] != float64(120) || len(offer.Prerequisites) != 2 {
		t.Fatalf("deployment offer = %+v, err=%v", offer, err)
	}
	if offer.PrerequisitesSHA256 != "d4a0cfe061b90e2aa8833c78fb62528c750b0d780a60122efbb4ac621ae6b7df" {
		t.Fatalf("prerequisite digest = %s", offer.PrerequisitesSHA256)
	}
	if err := store.ObserveDeploymentSettings(ctx, hostID, connection, json.RawMessage(`{"idle_timeout_secs":null}`)); err != nil {
		t.Fatal(err)
	}
	if baseline, err := store.DeploymentSettingsForConnection(ctx, hostID, connection); err != nil || baseline != nil {
		t.Fatalf("malformed report retained old baseline: %+v, err=%v", baseline, err)
	}
}

func TestCompleteInventorySnapshotSeparatesSeedFromVerifiedApplication(t *testing.T) {
	pool := testPool(t)
	store := NewStore(pool)
	hostID := seedHost(t, pool)
	ctx := context.Background()
	connection := "00000000-0000-4000-8000-000000000121"
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection, map[string]int{"typed_settings": 2}, []string{"idle_timeout_secs"}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, []string{"idle_timeout_secs"}); err != nil {
		t.Fatal(err)
	}
	view, err := store.SavePolicy(ctx, hostID, "0", map[string]PolicyChoice{"idle_timeout_secs": {Source: "explicit", Value: float64(900)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest := *view.Groups["idle_timeout_secs"].DesiredDigest
	if err := store.ReconcilePolicySnapshots(ctx, hostID, connection, map[string]PolicySnapshot{"idle_timeout_secs": {Kind: "seeded", Digest: digest}}); err != nil {
		t.Fatal(err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["idle_timeout_secs"].Status == "applied" {
		t.Fatalf("seed counted as applied: %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
	if err := store.ReconcilePolicySnapshots(ctx, hostID, "00000000-0000-4000-8000-000000000122", map[string]PolicySnapshot{"idle_timeout_secs": {Kind: "verified", Digest: digest}}); err != nil {
		t.Fatal(err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["idle_timeout_secs"].Status == "applied" {
		t.Fatalf("wrong connection applied: %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
	if err := store.ReconcilePolicySnapshots(ctx, hostID, connection, map[string]PolicySnapshot{"idle_timeout_secs": {Kind: "verified", Digest: digest}}); err != nil {
		t.Fatal(err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["idle_timeout_secs"].Status != "applied" {
		t.Fatalf("verified active snapshot not applied: %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
	if err := store.ReconcilePolicySnapshots(ctx, hostID, connection, map[string]PolicySnapshot{"idle_timeout_secs": {Kind: "verified", Digest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}); err != nil {
		t.Fatal(err)
	}
	view, err = store.GetPolicy(ctx, hostID)
	if err != nil || view.Groups["idle_timeout_secs"].Status != "pending" {
		t.Fatalf("different active snapshot retained proof: %+v, err=%v", view.Groups["idle_timeout_secs"], err)
	}
}
