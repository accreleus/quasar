package agentws

import (
	"bytes"
	"context"
	"encoding/json"
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

// #515: an adoption that binds the journal but cannot queue its inventory
// request is retried at the next heartbeat, never left holding the host.
func TestAdoptedJournalWithUnsentRequestIsRetried(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := hostcfg.NewStore(pool)
	hostID := seedHost(t, pool)
	const connection, delivery = "00000000-0000-4000-8000-000000000516", "00000000-0000-4000-8000-000000000517"
	groups := []string{"idle_timeout_secs"}
	if _, err := store.BeginPolicyConnection(ctx, hostID, connection,
		map[string]int{"typed_settings": 2, "execution_journal": 1}, groups, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConfirmPolicyGroups(ctx, hostID, connection, groups); err != nil {
		t.Fatal(err)
	}
	boot, err := store.StartRH05Boot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginJournalReconciliation(ctx, hostID, connection); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.SetInitialDelivery(ctx, hostID, connection, delivery); err != nil || !ok {
		t.Fatalf("set initial map delivery: ok=%v err=%v", ok, err)
	}
	if ok, err := store.AcknowledgeInitialDelivery(ctx, hostID, connection, delivery); err != nil || !ok {
		t.Fatalf("ack initial map: ok=%v err=%v", ok, err)
	}
	if err := store.CompleteJournalReconciliation(ctx, hostID, connection, nil); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry := NewRegistry(log)
	h := NewHandler(pool, log, registry, nil, nil, store, nil, boot)
	t.Cleanup(h.Close)
	c := newConn(hostID, nil)
	c.policyTyped = true
	c.policyAccepted = groups
	c.policyAcknowledged.Store(true)
	c.policyInventoryDone.Store(true)
	c.policyDeliveryID = delivery
	c.bootIncarnation = boot
	c.connectionIncarnation = connection
	registry.add(c)
	t.Cleanup(func() { registry.remove(c) })
	gate := func() (state string, owner *string, held bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT j.state,j.connection_incarnation::text,
			EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=j.host_id)
			FROM host_journal_reconciliation j WHERE j.host_id=$1::uuid`, hostID).Scan(&state, &owner, &held); err != nil {
			t.Fatal(err)
		}
		return
	}
	if state, _, held := gate(); state != "complete" || held {
		t.Fatalf("fixture journal is %s held=%v, want complete and free", state, held)
	}

	if _, err := hostcfg.NewStore(pool).StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	// The outbound queue is full when the stale heartbeat adopts the journal.
	for len(c.out) < cap(c.out) {
		c.out <- nil
	}
	if err := h.adoptOrphanedJournal(ctx, c, true); err == nil {
		t.Fatal("a request the queue refused was reported as sent")
	}
	if state, owner, held := gate(); state != "pending" || owner == nil || *owner != connection || !held {
		t.Fatalf("the adoption did not bind the journal; the test proves nothing: %s %v %v", state, owner, held)
	}
	for len(c.out) > 0 {
		<-c.out
	}
	// The next heartbeat is not stale: this connection owns the journal now.
	if err := h.adoptOrphanedJournal(ctx, c, false); err != nil {
		t.Fatal(err)
	}
	var request ConfigPolicyInventoryRequest
	select {
	case raw := <-c.out:
		if err := json.Unmarshal(raw, &request); err != nil || request.Type != "config_policy_journal_inventory_request" {
			t.Fatalf("retry queued %s (%v), want an inventory request", raw, err)
		}
	default:
		t.Fatal("a bound journal was never asked for after its request failed")
	}
	page, err := json.Marshal(map[string]any{
		"type": "config_policy_journal_inventory_page", "inventory_id": request.InventoryID,
		"snapshot_id": "00000000-0000-4000-8000-000000000518", "cursor": nil, "next_cursor": nil,
		"revision_high_water": map[string]string{}, "entries": []any{},
		"active_snapshots": map[string]any{"idle_timeout_secs": map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.acceptPolicyInventoryPage(ctx, c, page); err != nil {
		t.Fatal(err)
	}
	if state, owner, held := gate(); state != "complete" || owner == nil || *owner != connection || held {
		t.Fatalf("after the retried inventory the journal is %s owner=%v held=%v", state, owner, held)
	}
	// One heartbeat more must not ask again.
	if err := h.adoptOrphanedJournal(ctx, c, false); err != nil || len(c.out) != 0 {
		t.Fatalf("a settled adoption asked again: err=%v queued=%d", err, len(c.out))
	}
}

// #515: the answer to an inventory the adoption replaced is dropped, and only
// that one: the same id a second time is a violation again.
func TestSupersededInventoryPageIsDroppedOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := hostcfg.NewStore(pool)
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
	groups := hostcfg.NextSessionPolicyGroups()
	const node = "rh05-515-superseded"
	agent.send(t, map[string]any{
		"type": "register", "node_name": node, "agent_version": "test",
		"auth":                   map[string]string{"enrollment_token": boundEnrollmentToken(t, pool, node)},
		"config_policy_versions": map[string]int{"typed_settings": 2, "execution_journal": 1, "deployment_baseline": 1},
		"config_policy_groups":   groups,
	})
	agent.hostID, _ = agent.readUntil(t, "registered")["host_id"].(string)
	capacity := map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                          []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2, "codecs": []string{"h264", "h265", "av1"}}},
		"config_policy_accepted_groups": groups, "deployment_settings": policyBaseline(),
	}
	agent.send(t, capacity)
	first := agent.readUntil(t, "config_policy_journal_inventory_request")

	// Another process boots before the agent has answered.
	if _, err := hostcfg.NewStore(pool).StartRH05Boot(ctx); err != nil {
		t.Fatal(err)
	}
	agent.send(t, map[string]any{"type": "heartbeat", "running_sessions": []string{}, "ts_unix_ms": time.Now().UnixMilli()})
	second := agent.readUntil(t, "config_policy_journal_inventory_request")
	if second["inventory_id"] == first["inventory_id"] {
		t.Fatal("the adoption did not open a new inventory; the test proves nothing")
	}
	snapshots := map[string]any{}
	for _, group := range groups {
		snapshots[group] = map[string]string{"kind": "seeded", "digest": strings.Repeat("a", 64)}
	}
	page := func(id any) map[string]any {
		return map[string]any{
			"type": "config_policy_journal_inventory_page", "inventory_id": id,
			"snapshot_id": "00000000-0000-4000-8000-000000000519", "cursor": nil, "next_cursor": nil,
			"revision_high_water": map[string]string{}, "active_snapshots": snapshots, "entries": []any{},
		}
	}
	agent.send(t, page(first["inventory_id"]))
	agent.send(t, page(second["inventory_id"]))
	delivery := agent.readUntil(t, "config_update")
	for delivery["settings_delivery_id"] == nil {
		delivery = agent.readUntil(t, "config_update")
	}
	capacity["config_policy_legacy_map_applied_id"] = delivery["settings_delivery_id"]
	agent.send(t, capacity)
	waitUntil(t, "the replacement inventory to release the host", func() bool {
		var status, state string
		var held bool
		err := pool.QueryRow(ctx, `SELECT h.status,j.state,EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=h.id)
			FROM hosts h JOIN host_journal_reconciliation j ON j.host_id=h.id WHERE h.id=$1::uuid`, agent.hostID).Scan(&status, &state, &held)
		return err == nil && status == "online" && state == "complete" && !held
	})
	if !registry.IsConnected(agent.hostID) {
		t.Fatal("the late answer to a replaced inventory closed the connection")
	}

	agent.send(t, page(first["inventory_id"]))
	waitUntil(t, "an unknown inventory id to close the connection", func() bool { return !registry.IsConnected(agent.hostID) })
}
