package agentws

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

// policyBaseline is a complete, valid agent deployment baseline.
func policyBaseline() map[string]any {
	baseline := map[string]any{}
	for _, knob := range hostcfg.Catalog() {
		baseline[knob.Key] = knob.Default
	}
	baseline["abr_floor_kbps"] = nil
	baseline["home_root"] = "/srv/homes"
	return baseline
}

// policyExplicit is one valid explicit value per next-session key.
var policyExplicit = map[string]any{
	"abr_enabled": false, "abr_floor_kbps": float64(1), "abr_floor_ratio": float64(1), "abr_mode": "protective",
	"abr_ewma_alpha": 0.5, "abr_deadband": 0.5, "abr_max_up_step": 0.5, "abr_min_interval_ms": float64(1),
	"abr_max_down_step": 0.5, "abr_down_dwell_ms": float64(0), "abr_cliff_guard_frac": 0.5, "abr_ladder": true,
	"abr_ladder_max_bias": float64(255), "abr_ladder_engage_dwell": float64(1), "abr_ladder_recover_dwell": float64(255),
	"abr_ladder_resolution": true, "abr_ladder_res_exponent": 0.5, "abr_ladder_res_engage_frac": 0.2,
	"abr_ladder_res_recover_frac": float64(1), "abr_ladder_res_engage_dwell": float64(60), "abr_ladder_res_recover_dwell": float64(1),
	"abr_ladder_res_min_step_s": float64(5), "abr_ladder_res_min_height": float64(2160), "abr_ladder_fps": true,
	"abr_ladder_floor_follows_rung": false, "abr_ladder_order": "fps_first", "gop": float64(1), "slices": float64(8),
	"target_usage": float64(7), "queue_buffers": float64(1), "zerocopy": true, "latency_probe": false,
	"idle_timeout_secs": float64(0), "app_boot_timeout_secs": float64(0), "home_root": "/srv/homes/users", "nvidia_lib32_path": "",
}

// typedAgent is a version 2 agent socket that has completed registration,
// ownership echo, journal inventory and the initial legacy map.
type typedAgent struct {
	ws     *websocket.Conn
	hostID string
}

func (a typedAgent) send(t *testing.T, v any) {
	t.Helper()
	if err := a.ws.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}

// readUntil returns the next message of type kind, skipping others.
func (a typedAgent) readUntil(t *testing.T, kind string) map[string]any {
	t.Helper()
	_ = a.ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var msg map[string]any
		if err := a.ws.ReadJSON(&msg); err != nil {
			t.Fatalf("waiting for %s: %v", kind, err)
		}
		if msg["type"] == kind {
			return msg
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// collectOffers sends heartbeats until every group has one offer. Startup map
// acknowledgement can also dispatch while the first heartbeat is in flight,
// so claims observed between polls do not identify one dispatch pass.
func (a typedAgent) collectOffers(t *testing.T, pool *pgxpool.Pool, want int) map[string]map[string]any {
	t.Helper()
	offers := map[string]map[string]any{}
	claimed := func() int {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM host_reconcile_obligations WHERE host_id=$1::uuid AND next_attempt_at>now()`, a.hostID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	start := claimed()
	for len(offers) < want {
		before := start + len(offers)
		a.send(t, map[string]any{"type": "heartbeat", "running_sessions": []string{}, "ts_unix_ms": time.Now().UnixMilli()})
		var sent int
		waitUntil(t, "an offer pass", func() bool { sent = claimed() - before; return sent > 0 })
		for i := 0; i < sent; i++ {
			offer := a.readUntil(t, "config_policy_offer")
			group := offer["group"].(string)
			if scope, ok := hostcfg.PolicyGroupScope(group); !ok || scope != "next_session" {
				t.Fatalf("unexpected group offer %q", group)
			}
			if _, duplicate := offers[group]; duplicate {
				t.Fatalf("duplicate offer for group %q", group)
			}
			offers[group] = offer
			if len(offers) > want {
				t.Fatalf("received %d groups, expected %d", len(offers), want)
			}
		}
	}
	return offers
}

func connectTypedAgent(t *testing.T, pool *pgxpool.Pool, h *Handler, advertised []string, capacityExtra map[string]any) (typedAgent, []string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	agent := typedAgent{ws: ws}
	agent.send(t, map[string]any{
		"type": "register", "node_name": "rh05-336-groups", "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": "test-token"},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1},
		"config_policy_groups":   advertised,
	})
	registered := agent.readUntil(t, "registered")
	agent.hostID, _ = registered["host_id"].(string)
	var echo []string
	for _, group := range registered["config_policy_groups"].([]any) {
		echo = append(echo, group.(string))
	}
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2, "codecs": []string{"h264", "h265", "av1"}}},
		"config_policy_accepted_groups": echo,
		"deployment_settings":           policyBaseline(),
	}
	for key, value := range capacityExtra {
		capacity[key] = value
	}
	agent.send(t, capacity)
	request := agent.readUntil(t, "config_policy_journal_inventory_request")
	snapshots := map[string]any{}
	for _, group := range echo {
		snapshots[group] = map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}
	}
	agent.send(t, map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000336", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "active_snapshots": snapshots, "entries": []any{},
	})
	delivery := agent.readUntil(t, "config_update")
	for delivery["settings_delivery_id"] == nil {
		delivery = agent.readUntil(t, "config_update")
	}
	capacity["config_policy_legacy_map_applied_id"] = delivery["settings_delivery_id"]
	agent.send(t, capacity)
	waitUntil(t, "admission gate", func() bool {
		var gated bool
		err := pool.QueryRow(context.Background(), `SELECT config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid`, agent.hostID).Scan(&gated)
		return err == nil && !gated
	})
	return agent, echo
}

func TestRegisterEchoesEveryNextSessionGroupButNotHardware(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	h := NewHandler(pool, "test-token", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	advertised := append(hostcfg.NextSessionPolicyGroups(), "hardware", "not_a_group")
	sort.Strings(advertised)
	_, echo := connectTypedAgent(t, pool, h, advertised, nil)
	if strings.Join(echo, ",") != strings.Join(hostcfg.NextSessionPolicyGroups(), ",") {
		t.Fatalf("echo = %v", echo)
	}
}

// TestTypedAgentEveryNextSessionGroupReportsIndependently drives one offer per
// group through the agent socket, without any operator event after the save:
// the heartbeat sends the offers. Each group's state lands on that group only.
func TestTypedAgentEveryNextSessionGroupReportsIndependently(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	h := NewHandler(pool, "test-token", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	groups := hostcfg.NextSessionPolicyGroups()
	agent, _ := connectTypedAgent(t, pool, h, groups, map[string]any{
		"codecs": []string{"h264", "h265", "av1"}, "effective_settings": map[string]string{"zerocopy": "false"},
	})
	changes := map[string]hostcfg.PolicyChoice{}
	for i, group := range groups {
		if i%2 == 0 {
			changes[group] = hostcfg.PolicyChoice{Source: "explicit", Value: policyExplicit[group]}
		} else {
			changes[group] = hostcfg.PolicyChoice{Source: "deployment"}
		}
	}
	changes["zerocopy"] = hostcfg.PolicyChoice{Source: "explicit", Value: true}
	if _, err := store.SavePolicy(ctx, agent.hostID, "0", changes, nil); err != nil {
		t.Fatal(err)
	}
	offers := agent.collectOffers(t, pool, len(groups))
	baseline := policyBaseline()
	for group, offer := range offers {
		resolved := offer["resolved_settings"].(map[string]any)
		want := changes[group].Value
		if changes[group].Source == "deployment" {
			want = baseline[group]
		}
		if len(resolved) != 1 || resolved[group] != want {
			t.Fatalf("%s offer resolved %v, want %v", group, resolved, want)
		}
	}
	// gop: invalid intent. slices: transient failure. gop's neighbour
	// target_usage: applied readback with the wrong value. Every other group:
	// applied with matching readback.
	outcome := map[string]string{"gop": "invalid_value", "slices": "journal_write_failed", "target_usage": "wrong_readback"}
	for group, offer := range offers {
		state := map[string]any{
			"type": "config_policy_state", "attempt_id": offer["attempt_id"], "host_id": agent.hostID,
			"group": group, "revision": offer["revision"], "content_sha256": offer["content_sha256"], "scope": "next_session",
			"grant_boot_incarnation": offer["boot_incarnation"], "grant_connection_incarnation": offer["connection_incarnation"],
			"journal_sequence": "1", "phase": "applied", "active_scope": "next_session", "error": nil,
			"evidence": map[string]any{
				"revision": offer["revision"], "content_sha256": offer["content_sha256"], "resolved_settings": offer["resolved_settings"],
				"agent_process_id": "4242", "observed_at": time.Now().UTC().Format(time.RFC3339), "evidence_ids": []string{},
			},
		}
		switch outcome[group] {
		case "invalid_value", "journal_write_failed":
			state["phase"], state["active_scope"], state["evidence"], state["error"] = "failed", nil, nil, outcome[group]
		case "wrong_readback":
			state["evidence"].(map[string]any)["resolved_settings"] = map[string]any{group: "wrong"}
		}
		agent.send(t, state)
	}
	want := func(group string) string {
		switch outcome[group] {
		case "invalid_value":
			return "failed"
		case "journal_write_failed", "wrong_readback":
			return "pending"
		}
		return "applied"
	}
	var view hostcfg.PolicyView
	waitUntil(t, "independent group outcomes", func() bool {
		var err error
		view, err = store.GetPolicy(ctx, agent.hostID)
		if err != nil {
			return false
		}
		for _, group := range groups {
			if view.Groups[group].Status != want(group) {
				return false
			}
		}
		return true
	})
	if r := view.Groups["gop"].Remedy; r == nil || !strings.HasPrefix(*r, "validation_failed:") {
		t.Fatalf("invalid intent remedy = %v", r)
	}
	if g := view.Groups["slices"]; g.NextRetryAt == nil {
		t.Fatalf("transient failure has no scheduled retry: %+v", g)
	}
	var obligations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM host_reconcile_obligations WHERE host_id=$1::uuid AND resource_key='gop'`, agent.hostID).Scan(&obligations); err != nil || obligations != 0 {
		t.Fatalf("invalid intent keeps an obligation: %d err=%v", obligations, err)
	}
	// zerocopy changed from the value the codec claims were probed under.
	var hostCodecs, gpuCodecs string
	if err := pool.QueryRow(ctx, `SELECT h.codecs::text,g.codecs::text FROM hosts h JOIN gpus g ON g.host_id=h.id WHERE h.id=$1::uuid`, agent.hostID).Scan(&hostCodecs, &gpuCodecs); err != nil {
		t.Fatal(err)
	}
	if hostCodecs != `["h264"]` || gpuCodecs != `["h264"]` {
		t.Fatalf("stale probe codecs kept after zerocopy apply: host=%s gpu=%s", hostCodecs, gpuCodecs)
	}
	// The retry is driven by the heartbeat once the backoff expires; the
	// invalid group is never offered again.
	if _, err := pool.Exec(ctx, `UPDATE host_reconcile_obligations SET next_attempt_at=now()-interval '1 second' WHERE host_id=$1::uuid`, agent.hostID); err != nil {
		t.Fatal(err)
	}
	retried := agent.collectOffers(t, pool, 2)
	if len(retried) != 2 || retried["slices"] == nil || retried["target_usage"] == nil {
		t.Fatalf("retry pass offered %v; only the two transient groups may return", keys(retried))
	}
}

func TestGroupExecutionUnavailableParksAnyKnownGroup(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	h := NewHandler(pool, "test-token", slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, store, nil)
	t.Cleanup(h.Close)
	agent, _ := connectTypedAgent(t, pool, h, []string{"gop", "slices"}, nil)
	if _, err := store.SavePolicy(ctx, agent.hostID, "0", map[string]hostcfg.PolicyChoice{"gop": {Source: "explicit", Value: float64(90)}, "slices": {Source: "explicit", Value: float64(4)}}, nil); err != nil {
		t.Fatal(err)
	}
	var connection string
	if err := pool.QueryRow(ctx, `SELECT deployment_settings_connection::text FROM hosts WHERE id=$1::uuid`, agent.hostID).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	agent.send(t, map[string]any{"type": "config_policy_feature_error", "code": "group_execution_unavailable", "group": "gop", "connection_incarnation": connection})
	waitUntil(t, "gop parked", func() bool {
		view, err := store.GetPolicy(ctx, agent.hostID)
		return err == nil && view.Groups["gop"].Status == "upgrade_required" && view.Groups["slices"].Status == "pending"
	})
}

func TestWithdrawStaleProbeCodecsOnlyWhenTheProbedValueDiffers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	s := &agentStore{pool: pool}
	for _, tc := range []struct {
		effective, want string
	}{
		{`{"zerocopy":"false"}`, `["h264"]`},
		{`{"zerocopy":"true"}`, `["h264", "h265", "av1"]`},
		{`{}`, `["h264"]`},
	} {
		var hostID string
		if err := pool.QueryRow(ctx, `INSERT INTO hosts (node_name,status,codecs,effective_settings) VALUES ('rh05-336-probe','online','["h264","h265","av1"]',$1::jsonb) RETURNING id::text`, tc.effective).Scan(&hostID); err != nil {
			t.Fatal(err)
		}
		if err := s.withdrawStaleProbeCodecs(ctx, hostID, "zerocopy", true); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := pool.QueryRow(ctx, `SELECT codecs::text FROM hosts WHERE id=$1::uuid`, hostID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("effective %s: codecs %s, want %s", tc.effective, got, tc.want)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM hosts WHERE id=$1::uuid`, hostID); err != nil {
			t.Fatal(err)
		}
	}
}

func keys(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
