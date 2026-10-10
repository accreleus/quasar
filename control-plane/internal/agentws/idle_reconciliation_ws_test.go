package agentws

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/hostcfg"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

func TestFailedJournalBeginReleasesAuthenticatedConnection(t *testing.T) {
	pool := testPool(t)
	// A missing boot row deterministically fails BeginJournalReconciliation
	// after registration but before the capacity handshake.
	var priorBoot string
	var priorStarted time.Time
	err := pool.QueryRow(context.Background(), `SELECT incarnation::text,started_at FROM rh05_control_boot WHERE id=true`).
		Scan(&priorBoot, &priorStarted)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	if priorBoot != "" {
		t.Cleanup(func() {
			if _, err := pool.Exec(context.Background(), `INSERT INTO rh05_control_boot(id,incarnation,started_at)
				VALUES(true,$1::uuid,$2) ON CONFLICT(id) DO UPDATE SET incarnation=excluded.incarnation,started_at=excluded.started_at`, priorBoot, priorStarted); err != nil {
				t.Error(err)
			}
		})
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM rh05_control_boot WHERE id=true`); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, hostcfg.NewStore(pool), nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err := ws.WriteJSON(map[string]any{
		"type": "register", "node_name": "idle-begin-failure", "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": "test-token"},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1},
		"config_policy_groups":   []string{},
	}); err != nil {
		t.Fatal(err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var registered map[string]any
	if err := ws.ReadJSON(&registered); err != nil || registered["type"] != "registered" {
		t.Fatalf("registration did not reach the failing reconciliation seam: %v %+v", err, registered)
	}
	hostID, _ := registered["host_id"].(string)
	if hostID == "" {
		t.Fatal("registered without host id")
	}
	if err := ws.ReadJSON(&registered); err == nil {
		t.Fatal("failed reconciliation left authenticated websocket open")
	}
	if registry.IsConnected(hostID) {
		t.Fatal("failed reconciliation retained a schedulable current connection")
	}
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM hosts WHERE id=$1::uuid`, hostID).Scan(&status); err != nil || status != "offline" {
		t.Fatalf("failed reconciliation left host available: %q %v", status, err)
	}
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), s)
}

// #515: a boot fence written by a process that never serves the agents is
// released on the connection the serving control plane already holds.
func TestForeignBootFenceReconcilesOnLiveConnection(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := hostcfg.NewStore(pool)
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, store, nil, boot)
	t.Cleanup(h.Close)
	agent, echo := connectTypedAgent(t, pool, h, hostcfg.NextSessionPolicyGroups(), nil)

	type gate struct {
		status, state, boot string
		owner               *string
		held                bool
	}
	read := func() gate {
		var g gate
		if err := pool.QueryRow(ctx, `SELECT h.status,j.state,j.boot_incarnation::text,j.connection_incarnation::text,
			EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=h.id)
			FROM hosts h JOIN host_journal_reconciliation j ON j.host_id=h.id WHERE h.id=$1::uuid`, agent.hostID).
			Scan(&g.status, &g.state, &g.boot, &g.owner, &g.held); err != nil {
			t.Fatal(err)
		}
		return g
	}
	released := func() bool {
		g := read()
		return g.status == "online" && g.state == "complete" && !g.held && g.owner != nil && g.boot == boot
	}
	waitUntil(t, "the first reconciliation", released)
	connection := *read().owner
	heartbeat := map[string]any{"type": "heartbeat", "running_sessions": []string{}, "ts_unix_ms": time.Now().UnixMilli()}
	// Frames are handled in order: a stored heartbeat means every frame before it was.
	settle := func() {
		t.Helper()
		agent.send(t, heartbeat)
		waitUntil(t, "the heartbeat to be stored", func() bool {
			var stored bool
			err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_idle_inventory WHERE host_id=$1::uuid)`, agent.hostID).Scan(&stored)
			return err == nil && stored
		})
	}
	settle()

	// Another process starts against the same database and serves nobody.
	foreign, err := hostcfg.NewStore(pool).StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g := read(); g.status != "draining" || g.state != "pending" || g.owner != nil || !g.held || g.boot != foreign {
		t.Fatalf("the foreign boot did not fence the host; the test proves nothing: %+v", g)
	}

	capacity := func(cores int) map[string]any {
		return map[string]any{
			"type": "capacity", "host": map[string]any{"cpu_cores": cores, "mem_mb": 32000},
			"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2, "codecs": []string{"h264", "h265", "av1"}}},
			"config_policy_accepted_groups": echo, "deployment_settings": policyBaseline(),
		}
	}
	const refused = "capacity re-report failed"

	// The capacity is refused before the heartbeat heals.
	agent.send(t, capacity(9))
	agent.send(t, heartbeat)
	request := agent.readUntil(t, "config_policy_journal_inventory_request")
	if request["boot_incarnation"] != boot || request["connection_incarnation"] != connection {
		t.Fatalf("inventory request names %v/%v, want this control plane's %s/%s",
			request["boot_incarnation"], request["connection_incarnation"], boot, connection)
	}
	if logs.count(refused) != 1 {
		t.Fatalf("the orphaned journal refused %d capacity reports, want 1", logs.count(refused))
	}
	snapshots := map[string]any{}
	for _, group := range echo {
		snapshots[group] = map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}
	}
	agent.send(t, map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request["inventory_id"],
		"snapshot_id": "00000000-0000-4000-8000-000000000515", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "active_snapshots": snapshots, "entries": []any{},
	})
	waitUntil(t, "the fence to release on the live connection", released)
	if got := *read().owner; got != connection {
		t.Fatalf("journal owner = %s, want the connection that never dropped, %s", got, connection)
	}
	var current string
	if err := pool.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&current); err != nil || current != boot {
		t.Fatalf("database boot = %s (%v), want the serving control plane's %s", current, err, boot)
	}

	agent.send(t, capacity(10))
	settle()
	if logs.count(refused) != 1 {
		t.Fatal("capacity reports are still refused after the journal reconciled")
	}
	if !registry.IsConnected(agent.hostID) || !released() {
		t.Fatalf("the host did not stay connected and unrestricted: %+v", read())
	}
}
