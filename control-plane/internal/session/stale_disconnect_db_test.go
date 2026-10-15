package session

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
)

// An agent that reconnected to another control-plane process stays placeable when its
// old connection's teardown records the disconnect afterwards: that markOffline carries
// the old registration, so it must not stamp the row (#626, #644).
func TestAStaleConnectionsDisconnectLeavesTheReconnectedHostPlaceable(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	log := testLogger()
	const node = "stale-disconnect-626"
	seedEnrollmentToken(t, pool, node)

	process := func(db *pgxpool.Pool) string {
		registry := agentws.NewRegistry(log)
		h := agentws.NewHandler(db, log, registry, newTestCoordinator(t, NewStore(pool), registry, log), nil, nil, nil)
		t.Cleanup(h.Close)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return "ws" + strings.TrimPrefix(srv.URL, "http")
	}
	connect := func(url string, auth map[string]string, cores int) (*websocket.Conn, map[string]any) {
		ws, _, err := websocket.DefaultDialer.Dial(url, nil)
		must(t, err)
		t.Cleanup(func() { _ = ws.Close() })
		_ = ws.SetReadDeadline(time.Now().Add(lockOrderWait))
		must(t, ws.WriteJSON(map[string]any{"type": "register", "node_name": node, "agent_version": "test", "auth": auth}))
		var registered map[string]any
		must(t, ws.ReadJSON(&registered))
		if registered["type"] != "registered" {
			t.Fatalf("register: %+v", registered)
		}
		must(t, ws.WriteJSON(map[string]any{
			"type": "capacity", "host": map[string]any{"cpu_cores": cores, "mem_mb": 32000},
			"gpus": []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 8}},
		}))
		waitForCores(t, pool, registered["host_id"].(string), cores)
		return ws, registered
	}

	oldPool, teardown := gatedPool(t, "agent_disconnected_at=now()")
	old, first := connect(process(oldPool), map[string]string{"enrollment_token": node}, 1)
	hostID, _ := first["host_id"].(string)
	secret, _ := first["node_secret"].(string)
	connect(process(pool), map[string]string{"node_secret": secret}, 2)

	teardown.armed.Store(true)
	_ = old.Close()
	teardown.awaitReached(t)
	teardown.open()
	oldPool.Close() // waits for the old connection's markOffline to return

	var stamped bool
	must(t, pool.QueryRow(ctx, `SELECT agent_disconnected_at IS NOT NULL FROM hosts WHERE id=$1::uuid`, hostID).Scan(&stamped))
	if status := hostStatus(t, pool, hostID); status != "online" || stamped {
		t.Fatalf("after the old connection closed: status=%q disconnect stamped=%v, want online and unstamped", status, stamped)
	}
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	p := launchParams(s)
	p.PinHostID = hostID
	if _, err := NewStore(pool).ScheduleAndCreate(ctx, p); err != nil {
		t.Fatalf("launch on the reconnected host: %v", err)
	}
}
