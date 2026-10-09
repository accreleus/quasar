package agentws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
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

// A restart-scope report is checked against the durable idle journal before
// its sequence is cached, so fabricated attempt ids cannot grow the connection.
func TestFabricatedRestartAttemptsAreNotCached(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, store, nil, boot)
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
		"type": "register", "node_name": "fabricated-restart-test", "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": testEnrollmentToken},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1, "idle_apply": 1},
		"config_policy_groups":   []string{"hardware"},
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
		"config_policy_accepted_groups": []string{"hardware"},
	}); err != nil {
		t.Fatal(err)
	}
	attemptID := "00000000-0000-4000-8000-000000000501"
	grantConnection := "00000000-0000-4000-8000-000000000502"
	digest := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_approvals(id,host_id,group_key,revision,approved_digest,prerequisites_digest,boot_incarnation,review_id,expires_at,state)
		VALUES($1::uuid,$2::uuid,'hardware',1,$3,$3,$4::uuid,'00000000-0000-4000-8000-000000000503',now()+interval '1 hour','offered')`,
		attemptID, hostID, digest, boot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,grant_connection,phase)
		VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,$5::uuid,'offered')`,
		attemptID, hostID, digest, boot, grantConnection); err != nil {
		t.Fatal(err)
	}
	report := func(id, connection string) ConfigPolicyStateMsg {
		return ConfigPolicyStateMsg{
			Type: "config_policy_state", AttemptID: id, HostID: hostID, Group: "hardware", Revision: "1",
			ContentSHA256: digest, Scope: "restart", GrantBootIncarnation: boot,
			GrantConnectionIncarnation: connection, JournalSequence: "1", Phase: "accepted",
		}
	}
	for i := range 3 {
		if err := ws.WriteJSON(report(fmt.Sprintf("00000000-0000-4000-8000-%012d", 600+i), grantConnection)); err != nil {
			t.Fatal(err)
		}
	}
	// The real attempt under a grant connection it was never offered on.
	if err := ws.WriteJSON(report(attemptID, "00000000-0000-4000-8000-000000000504")); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteJSON(report(attemptID, grantConnection)); err != nil {
		t.Fatal(err)
	}
	// Frames are handled in order, so the valid one landing means all were read.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var phase string
		if err := pool.QueryRow(ctx, `SELECT phase FROM host_config_attempts WHERE id=$1::uuid`, attemptID).Scan(&phase); err != nil {
			t.Fatal(err)
		}
		if phase == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("valid restart report not observed: phase=%q", phase)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c, ok := registry.get(hostID)
	if !ok {
		t.Fatal("connection gone")
	}
	if _, cached := c.policySequence[attemptID]; !cached || len(c.policySequence) != 1 || len(c.policySequenceContent) != 1 {
		t.Fatalf("sequence cache holds %d/%d entries, want only the valid attempt", len(c.policySequence), len(c.policySequenceContent))
	}
}
