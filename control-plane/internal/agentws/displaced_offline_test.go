package agentws

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A reconnect displaces the old socket; that socket's teardown, arriving after the
// new registration, must not project the live host offline.
func TestDisplacedConnectionTeardownKeepsTheReconnectedHostOnline(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	register := func(auth map[string]string) (*websocket.Conn, RegisteredMsg) {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.WriteJSON(map[string]any{
			"type": "register", "node_name": "displaced-teardown", "agent_version": "test", "auth": auth,
		}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var reg RegisteredMsg
		if err := conn.ReadJSON(&reg); err != nil || reg.HostID == "" {
			t.Fatalf("register: %v %+v", err, reg)
		}
		if err := conn.WriteJSON(map[string]any{"type": "capacity",
			"host": map[string]any{"cpu_cores": 8, "mem_mb": 32000}, "gpus": []any{}}); err != nil {
			t.Fatal(err)
		}
		return conn, reg
	}

	first, reg := register(map[string]string{"enrollment_token": "test-token"})
	if reg.NodeSecret == "" {
		t.Fatal("enrollment returned no node secret")
	}
	_, again := register(map[string]string{"node_secret": reg.NodeSecret})
	if again.HostID != reg.HostID {
		t.Fatalf("reconnect registered host %s, want %s", again.HostID, reg.HostID)
	}

	// The old agent process is gone; the control plane learns so only when its
	// read of the displaced socket fails, after the reconnect registered.
	_ = first.Close()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		var status string
		var disconnected bool
		if err := pool.QueryRow(context.Background(),
			`SELECT status, agent_disconnected_at IS NOT NULL FROM hosts WHERE id=$1::uuid`, reg.HostID).
			Scan(&status, &disconnected); err != nil {
			t.Fatal(err)
		}
		if status != "online" || disconnected {
			t.Fatalf("after the displaced socket closed: status=%q disconnected=%v, want online and connected", status, disconnected)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
