package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/crud"
	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
)

// restartProcess is one control-plane process on the shared database: the boot step
// cmd/quasar-control runs, its own registry, the agent endpoint and the admin routes.
type restartProcess struct {
	agentURL string
	api      *httptest.Server
	coord    *Coordinator
	stop     func()
}

func startRestartProcess(t *testing.T, pool *pgxpool.Pool) restartProcess {
	t.Helper()
	log := testLogger()
	cfg := hostcfg.NewStore(pool)
	boot, err := cfg.StartRH05Boot(context.Background())
	must(t, err)
	registry := agentws.NewRegistry(log)
	coord := newTestCoordinator(t, NewStore(pool), registry, log, WithAgentConnectivity(registry))
	h := agentws.NewHandler(pool, log, registry, coord, nil, cfg, nil, boot)
	agents := httptest.NewServer(h)
	hosts := crud.NewHandler(pool)
	hosts.SetRegistry(registry)
	mux := http.NewServeMux()
	open := func(next http.Handler) http.Handler { return next }
	hosts.Register(mux, open, open)
	api := httptest.NewServer(mux)
	stop := func() { h.Close(); agents.Close(); api.Close() }
	t.Cleanup(stop)
	return restartProcess{agentURL: "ws" + strings.TrimPrefix(agents.URL, "http"), api: api, coord: coord, stop: stop}
}

// typedRegister sends a typed-settings agent's register and returns the first reply.
func typedRegister(t *testing.T, url, nodeName string, auth map[string]string) (*websocket.Conn, map[string]any) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	must(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))
	must(t, ws.WriteJSON(map[string]any{
		"type": "register", "node_name": nodeName, "agent_version": "test", "auth": auth,
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1},
		"config_policy_groups":   hostcfg.NextSessionPolicyGroups(),
	}))
	var reply map[string]any
	must(t, ws.ReadJSON(&reply))
	return ws, reply
}

// enrollTypedHost enrolls a typed-settings agent and finishes its handshake (ownership
// echo, deployment baseline, journal inventory, the initial settings map), so the host
// is online and connected. Returns the host id and its node secret.
func enrollTypedHost(t *testing.T, pool *pgxpool.Pool, url, nodeName, token string) (string, string) {
	t.Helper()
	ws, registered := typedRegister(t, url, nodeName, map[string]string{"enrollment_token": token})
	hostID, _ := registered["host_id"].(string)
	secret, _ := registered["node_secret"].(string)
	if registered["type"] != "registered" || hostID == "" || secret == "" {
		t.Fatalf("enrolling %s: %+v", nodeName, registered)
	}
	groups, _ := registered["config_policy_groups"].([]any)
	baseline := map[string]any{}
	for _, knob := range hostcfg.Catalog() {
		baseline[knob.Key] = knob.Default
	}
	baseline["abr_floor_kbps"] = nil
	baseline["home_root"] = "/srv/homes"
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}},
		"config_policy_accepted_groups": groups, "deployment_settings": baseline,
	}
	must(t, ws.WriteJSON(capacity))
	var msg map[string]any
	for msg["type"] != "config_policy_journal_inventory_request" {
		must(t, ws.ReadJSON(&msg))
	}
	snapshots := map[string]any{}
	for _, group := range groups {
		snapshots[group.(string)] = map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}
	}
	must(t, ws.WriteJSON(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": msg["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000596", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "active_snapshots": snapshots, "entries": []any{},
	}))
	for msg["type"] != "config_update" || msg["settings_delivery_id"] == nil {
		must(t, ws.ReadJSON(&msg))
	}
	capacity["config_policy_legacy_map_applied_id"] = msg["settings_delivery_id"]
	must(t, ws.WriteJSON(capacity))
	waitFor(t, func() bool { return hostStatus(t, pool, hostID) == "online" })
	return hostID, secret
}

func seedBoundSingleUseToken(t *testing.T, pool *pgxpool.Pool, token, nodeName string) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(context.Background(), `INSERT INTO host_enrollments (token_hash, created_by, node_name, max_uses, expires_at, note)
		VALUES ($1, NULL, $2, 1, NULL, 'test fixture')
		ON CONFLICT (token_hash) DO UPDATE SET used_count=0, revoked_at=NULL, expires_at=NULL`, hex.EncodeToString(sum[:]), nodeName)
	must(t, err)
}

// #596: a control-plane restart records no disconnect, and its boot gives every
// typed-settings host a reconciliation hold that only a connected agent clears. So
// every host, cordoned or not, reads draining and unstamped until its agent is back:
// it must refuse a re-enrollment, an uncordon must not open it, and deleting the host
// is the way to re-add a machine that never returns.
func TestAfterAControlPlaneRestartAHostRefusesReEnrollmentUntilItIsDeleted(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	const firstToken = "restart-596-first"
	seedEnrollmentToken(t, pool, firstToken)

	// The process that is about to die has its own pool: closing it first is what
	// makes its teardown record nothing, as a real exit does.
	dying, err := pgxpool.New(ctx, os.Getenv("TEST_DATABASE_URL"))
	must(t, err)
	t.Cleanup(dying.Close)
	old := startRestartProcess(t, dying)

	kinds := []struct {
		name       string
		cordoned   bool
		uncordonTo error
	}{
		{name: "never-cordoned", uncordonTo: ErrHostNotResumable},
		{name: "operator-cordoned", cordoned: true},
	}
	type host struct{ id, node, secret, bound string }
	hosts := map[string]host{}
	for _, k := range kinds {
		for _, fate := range []string{"gone", "returns"} {
			node := "restart-596-" + k.name + "-" + fate
			id, secret := enrollTypedHost(t, pool, old.agentURL, node, firstToken)
			if k.cordoned {
				if _, err := old.coord.DrainHost(ctx, id, false); err != nil {
					t.Fatalf("cordon %s: %v", node, err)
				}
			}
			bound := "bound-" + node
			seedBoundSingleUseToken(t, pool, bound, node)
			hosts[k.name+"/"+fate] = host{id: id, node: node, secret: secret, bound: bound}
		}
	}

	dying.Close()
	old.stop()
	restarted := startRestartProcess(t, pool)

	refused := func(t *testing.T, h host, when string) {
		t.Helper()
		_, reply := typedRegister(t, restarted.agentURL, h.node, map[string]string{"enrollment_token": h.bound})
		if reply["type"] != "error" || reply["code"] != "auth_failed" {
			t.Fatalf("enrollment onto %s %s: got %v/%v, want an auth_failed error", h.node, when, reply["type"], reply["code"])
		}
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			for _, fate := range []string{"gone", "returns"} {
				h := hosts[k.name+"/"+fate]
				var stamped bool
				must(t, pool.QueryRow(ctx, `SELECT agent_disconnected_at IS NOT NULL FROM hosts WHERE id=$1::uuid`, h.id).Scan(&stamped))
				if status := hostStatus(t, pool, h.id); status != "draining" || stamped {
					t.Fatalf("%s after the restart: status=%q disconnect stamped=%v, want draining and unstamped", h.node, status, stamped)
				}
				refused(t, h, "after the restart")
				if _, err := restarted.coord.UncordonHost(ctx, h.id); !errors.Is(err, k.uncordonTo) {
					t.Fatalf("uncordon %s: got %v, want %v", h.node, err, k.uncordonTo)
				}
				refused(t, h, "after an uncordon")
			}

			back := hosts[k.name+"/returns"]
			_, reply := typedRegister(t, restarted.agentURL, back.node, map[string]string{"node_secret": back.secret})
			if reply["type"] != "registered" || reply["host_id"] != back.id {
				t.Fatalf("the incumbent's reconnect with its own secret: %+v", reply)
			}

			gone := hosts[k.name+"/gone"]
			req, err := http.NewRequest(http.MethodDelete, restarted.api.URL+"/v1/hosts/"+gone.id, nil)
			must(t, err)
			resp, err := http.DefaultClient.Do(req)
			must(t, err)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("deleting %s: status %d, want 204", gone.node, resp.StatusCode)
			}
			_, reply = typedRegister(t, restarted.agentURL, gone.node, map[string]string{"enrollment_token": gone.bound})
			if reply["type"] != "registered" || reply["host_id"] == gone.id {
				t.Fatalf("re-adding %s after its host was deleted: %+v", gone.node, reply)
			}
		})
	}
}
