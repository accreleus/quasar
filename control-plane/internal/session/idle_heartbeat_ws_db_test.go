package session

// #477: a heartbeat the idle inventory refuses must not end the agent's
// connection, so a launch still starting on that host is never host_lost.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Contains(l.buf.String(), s)
}

func TestRejectedIdleHeartbeatKeepsSocketAndStartingSession(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := hostcfg.NewStore(pool)
	boot, err := cfg.StartRH05Boot(ctx)
	must(t, err)
	const token = "idle-heartbeat-477"
	seedEnrollmentToken(t, pool, token)

	registry := agentws.NewRegistry(log)
	coord := newTestCoordinator(t, NewStore(pool), registry, log)
	h := agentws.NewHandler(pool, log, registry, coord, nil, cfg, nil, boot)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	must(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))

	// A typed-policy agent's whole handshake: register, capacity, the journal
	// inventory page, then the initial settings map acknowledgement.
	must(t, ws.WriteJSON(map[string]any{
		"type": "register", "node_name": "idle-heartbeat-477", "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": token},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1},
		"config_policy_groups":   []string{},
	}))
	var registered map[string]any
	must(t, ws.ReadJSON(&registered))
	hostID, _ := registered["host_id"].(string)
	if hostID == "" {
		t.Fatalf("registered without host id: %+v", registered)
	}
	gpus := []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}}
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus": gpus, "config_policy_accepted_groups": []string{},
	}
	must(t, ws.WriteJSON(capacity))
	var request map[string]any
	for request["type"] != "config_policy_journal_inventory_request" {
		must(t, ws.ReadJSON(&request))
	}
	must(t, ws.WriteJSON(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000477", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "active_snapshots": map[string]any{}, "entries": []any{},
	}))
	var delivery map[string]any
	for delivery["type"] != "config_update" || delivery["settings_delivery_id"] == nil {
		must(t, ws.ReadJSON(&delivery))
	}
	capacity["config_policy_legacy_map_applied_id"] = delivery["settings_delivery_id"]
	capacity["host"] = map[string]any{"cpu_cores": 9, "mem_mb": 32000}
	must(t, ws.WriteJSON(capacity))
	waitForCores(t, pool, hostID, 9)

	// A launch is still starting on this host when the journal moves to a
	// connection this socket does not own, as a reconnect elsewhere does.
	var userID, appID, sessionID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('idle477@test.local','idle477','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps (name) VALUES ('idle-477') RETURNING id::text`).Scan(&appID))
	must(t, pool.QueryRow(ctx, `INSERT INTO sessions (user_id,app_id,host_id,state,width,height,fps,bitrate_kbps)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'starting',1280,720,60,5000) RETURNING id::text`,
		userID, appID, hostID).Scan(&sessionID))
	var journalConn string
	must(t, pool.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation
		WHERE host_id=$1::uuid`, hostID).Scan(&journalConn))
	_, err = pool.Exec(ctx, `UPDATE host_journal_reconciliation SET connection_incarnation=gen_random_uuid()
		WHERE host_id=$1::uuid`, hostID)
	must(t, err)

	must(t, ws.WriteJSON(map[string]any{"type": "heartbeat", "running_sessions": []string{}, "ts_unix_ms": time.Now().UnixMilli()}))
	// Messages are read in order: once this capacity lands, the heartbeat
	// before it was handled and the socket outlived it.
	must(t, ws.WriteJSON(map[string]any{"type": "capacity", "host": map[string]any{"cpu_cores": 10, "mem_mb": 32000}, "gpus": gpus}))
	waitForCores(t, pool, hostID, 10)
	if !logs.contains("idle inventory heartbeat from a superseded connection ignored") {
		t.Fatal("the idle inventory did not refuse the heartbeat; the test proves nothing")
	}
	if logs.contains("RH05 idle inventory heartbeat rejected") {
		t.Fatal("a stale-connection heartbeat was reported as a rejection")
	}
	if !registry.IsConnected(hostID) {
		t.Fatal("a refused idle heartbeat dropped the agent connection")
	}

	// The current socket sends a list the inventory cannot store: refused at
	// Warn, and the socket still carries the next message.
	_, err = pool.Exec(ctx, `UPDATE host_journal_reconciliation SET connection_incarnation=$2::uuid
		WHERE host_id=$1::uuid`, hostID, journalConn)
	must(t, err)
	must(t, ws.WriteJSON(map[string]any{"type": "heartbeat", "running_sessions": []string{strings.Repeat("x", 65)}, "ts_unix_ms": time.Now().UnixMilli()}))
	must(t, ws.WriteJSON(map[string]any{"type": "capacity", "host": map[string]any{"cpu_cores": 11, "mem_mb": 32000}, "gpus": gpus}))
	waitForCores(t, pool, hostID, 11)
	if !logs.contains(hostcfg.ErrIdleInventoryInvalid.Error()) {
		t.Fatal("an invalid session list was not refused")
	}
	if !registry.IsConnected(hostID) {
		t.Fatal("a refused idle heartbeat dropped the agent connection")
	}

	var state string
	var detail *string
	must(t, pool.QueryRow(ctx, `SELECT state,state_detail FROM sessions WHERE id=$1::uuid`, sessionID).Scan(&state, &detail))
	if state != "starting" || detail != nil {
		t.Fatalf("starting session after a refused idle heartbeat = %s/%v, want starting with no detail", state, detail)
	}

	// Control: a real disconnect does reap the in-flight launch, so the check
	// above would have seen one.
	must(t, ws.Close())
	waitFor(t, func() bool {
		var d *string
		err := pool.QueryRow(ctx, `SELECT state,state_detail FROM sessions WHERE id=$1::uuid`, sessionID).Scan(&state, &d)
		return err == nil && state == "failed" && d != nil && *d == "host_lost"
	})
}

// seedEnrollmentToken mints an unbound enrollment token so a test agent can
// register through the real handler.
func seedEnrollmentToken(t *testing.T, pool *pgxpool.Pool, token string) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	_, err := pool.Exec(context.Background(), `INSERT INTO host_enrollments (token_hash, created_by, node_name, max_uses, expires_at, note)
		VALUES ($1, NULL, NULL, 10, NULL, 'test fixture')
		ON CONFLICT (token_hash) DO UPDATE SET used_count=0, revoked_at=NULL, expires_at=NULL`, hex.EncodeToString(sum[:]))
	must(t, err)
}

func waitForCores(t *testing.T, pool *pgxpool.Pool, hostID string, want int) {
	t.Helper()
	waitFor(t, func() bool {
		var cores int
		err := pool.QueryRow(context.Background(), `SELECT cpu_cores FROM hosts WHERE id=$1::uuid`, hostID).Scan(&cores)
		return err == nil && cores == want
	})
}
