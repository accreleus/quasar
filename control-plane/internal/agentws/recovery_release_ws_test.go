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

// sendRecoveryReleases sends only to an agent that advertised recovery_release
// (#656). The real connection order is TestAReleaseRequestedWhileDownIsSentWhenTheGateCompletes.
func TestAReconciledConnectionResendsPendingRecoveryReleases(t *testing.T) {
	pool := testPool(t)
	store := hostcfg.NewStore(pool)
	ctx := context.Background()
	hostID := seedHost(t, pool)
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connection := "00000000-0000-4000-8000-000000000656"
	attempt := "00000000-0000-4000-8000-000000000657"
	if _, err := pool.Exec(ctx, `INSERT INTO host_journal_reconciliation(host_id,boot_incarnation,connection_incarnation,state,completed_at)
		VALUES($1::uuid,$2::uuid,$3::uuid,'complete',now())
		ON CONFLICT(host_id) DO UPDATE SET connection_incarnation=excluded.connection_incarnation,state='complete'`, hostID, boot, connection); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,
		boot_incarnation,grant_connection,phase,journal_sequence,started_at,terminal_at,release_requested_at)
		VALUES($1::uuid,$2::uuid,'hardware',repeat('a',64),1,'restart',$3::uuid,$4::uuid,'uncertain',2,now(),now(),now())`,
		attempt, hostID, boot, connection); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, store, nil, boot)
	t.Cleanup(h.Close)
	for _, capable := range []bool{false, true} {
		c := newConn(hostID, nil)
		c.policyTyped, c.policyIdle, c.policyRecoveryRelease = true, true, capable
		c.bootIncarnation, c.connectionIncarnation = h.bootIncarnation, connection
		h.registry.add(c)
		if err := h.sendRecoveryReleases(ctx, c); err != nil {
			t.Fatal(err)
		}
		h.registry.remove(c)
		select {
		case raw := <-c.out:
			var cmd hostcfg.RecoveryReleaseCmd
			if !capable {
				t.Fatalf("sent to an agent without recovery_release: %s", raw)
			}
			if err := json.Unmarshal(raw, &cmd); err != nil || cmd != hostcfg.NewRecoveryReleaseCmd(attempt, h.bootIncarnation, connection) {
				t.Fatalf("release = %s (%v)", raw, err)
			}
		default:
			if capable {
				t.Fatal("a capable reconciled connection sent no release")
			}
		}
	}
}

// A release requested while the agent is down goes out when the reconnecting
// connection's journal gate completes, in the agent's real order: the inventory
// page arrives while the settings-delivery gate is open (its completion returns
// early), and the legacy-map acknowledgement in the next capacity completes the
// gate (#656, lab step 7).
func TestAReleaseRequestedWhileDownIsSentWhenTheGateCompletes(t *testing.T) {
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
	agent := typedAgent{ws: ws}
	agent.send(t, map[string]any{
		"type": "register", "node_name": "release-on-gate-656", "agent_version": "test",
		"auth": map[string]string{"enrollment_token": boundEnrollmentToken(t, pool, "release-on-gate-656")},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1,
			"idle_apply": 1, "recovery_release": 1},
		"config_policy_groups": []string{"hardware", "idle_timeout_secs"},
	})
	registered := agent.readUntil(t, "registered")
	agent.hostID, _ = registered["host_id"].(string)
	connection, _ := registered["connection_incarnation"].(string)
	var echo []string
	snapshots := map[string]any{}
	for _, group := range registered["config_policy_groups"].([]any) {
		echo = append(echo, group.(string))
		snapshots[group.(string)] = map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}
	}
	// What a previous connection left: an uncertain attempt holding a recovery
	// hold, and an administrator's release requested while no agent was connected.
	attemptID := "0000abcd-0000-4000-8000-000000000665"
	grantConnection := "00000000-0000-4000-8000-000000000666"
	digest := strings.Repeat("b", 64)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO host_config_approvals(id,host_id,group_key,revision,approved_digest,prerequisites_digest,boot_incarnation,review_id,expires_at,state)
			VALUES($1::uuid,$2::uuid,'hardware',1,$3,$3,$4::uuid,'00000000-0000-4000-8000-000000000667',now()+interval '1 hour','accepted')`,
			[]any{attemptID, agent.hostID, digest, boot}},
		{`INSERT INTO host_config_attempts(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,grant_connection,phase,journal_sequence,started_at,terminal_at,release_requested_at)
			VALUES($1::uuid,$2::uuid,'hardware',$3,1,'restart',$4::uuid,$5::uuid,'uncertain',8,now(),now(),now())`,
			[]any{attemptID, agent.hostID, digest, boot, grantConnection}},
		{`INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason) VALUES($2::uuid,'recovery',$1::uuid,'configuration_recovery')`,
			[]any{attemptID, agent.hostID}},
	} {
		if _, err := pool.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			t.Fatal(err)
		}
	}
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "amd", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}},
		"config_policy_accepted_groups": echo, "deployment_settings": policyBaseline(),
	}
	agent.send(t, capacity)
	request := agent.readUntil(t, "config_policy_journal_inventory_request")
	entry := map[string]any{
		"attempt_id": attemptID, "host_id": agent.hostID, "group": "hardware", "revision": "1",
		"content_sha256": digest, "scope": "restart", "grant_boot_incarnation": boot,
		"grant_connection_incarnation": grantConnection, "journal_sequence": "8", "phase": "uncertain",
	}
	agent.send(t, map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000668", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{"hardware": "1"},
		"active_snapshots":    snapshots,
		"entries":             []any{entry},
	})
	// Until the legacy map is acknowledged the delivery gate is open: the gate stays
	// pending and nothing may be sent.
	var delivery map[string]any
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	for delivery == nil {
		var msg map[string]any
		if err := ws.ReadJSON(&msg); err != nil {
			t.Fatalf("waiting for the legacy map: %v", err)
		}
		switch msg["type"] {
		case "config_policy_release":
			t.Fatalf("release sent before the journal gate completed: %v", msg)
		case "config_update":
			if msg["settings_delivery_id"] != nil {
				delivery = msg
			}
		}
	}
	if gate, err := store.JournalGate(ctx, agent.hostID); err != nil || gate != "pending" {
		t.Fatalf("gate %q (%v) before the map acknowledgement, want pending", gate, err)
	}
	capacity["config_policy_legacy_map_applied_id"] = delivery["settings_delivery_id"]
	agent.send(t, capacity)
	release := agent.readUntil(t, "config_policy_release")
	if release["attempt_id"] != attemptID || release["boot_incarnation"] != boot || release["connection_incarnation"] != connection {
		t.Fatalf("release = %v, want attempt %s on boot %s connection %s", release, attemptID, boot, connection)
	}
	// The agent's reply, as policy.rs release writes it, ends the hold.
	reply := map[string]any{"type": "config_policy_state"}
	for k, v := range entry {
		reply[k] = v
	}
	reply["journal_sequence"], reply["phase"] = "9", "released"
	agent.send(t, reply)
	waitUntil(t, "the released report to lift the hold", func() bool {
		var held bool
		err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='recovery')`, agent.hostID).Scan(&held)
		return err == nil && !held
	})
}
