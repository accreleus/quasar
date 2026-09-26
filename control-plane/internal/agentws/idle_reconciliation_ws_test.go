package agentws

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
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
