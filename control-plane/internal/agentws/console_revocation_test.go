package agentws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/console"
	"github.com/gorilla/websocket"
)

// #498: a console revocation that meets a full send queue closes the agent's
// connection, and the reconnect's register snapshot delivers it.
func TestConsoleRevocationOnFullQueueReconnectsAndDelivers(t *testing.T) {
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	consoleStore := console.NewStore(pool)
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, consoleStore)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ctx := context.Background()

	register := func(auth map[string]string) (*websocket.Conn, RegisteredMsg) {
		t.Helper()
		ws, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ws.Close() })
		if err := ws.WriteJSON(map[string]any{"type": "register", "node_name": "console-revocation", "agent_version": "test", "auth": auth}); err != nil {
			t.Fatal(err)
		}
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		var reg RegisteredMsg
		if err := ws.ReadJSON(&reg); err != nil || reg.HostID == "" {
			t.Fatalf("register: %v %+v", err, reg)
		}
		return ws, reg
	}

	first, reg := register(map[string]string{"enrollment_token": "test-token"})
	if err := consoleStore.Upsert(ctx, reg.HostID, map[string]any{"enabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	// The test agent stops reading, so the writer blocks once the socket
	// buffers fill and the 32-slot queue backs up behind it.
	pad := strings.Repeat("x", 1<<20)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := h.registry.Send(reg.HostID, map[string]string{"type": "test_padding", "pad": pad})
		if errors.Is(err, ErrSendQueueFull) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("send queue never filled")
		}
	}

	mux := http.NewServeMux()
	console.NewHandler(consoleStore, h.registry).Register(mux, func(next http.Handler) http.Handler { return next })
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/v1/admin/hosts/"+reg.HostID+"/console-config", bytes.NewReader([]byte(`{"enabled":false}`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body.String())
	}

	// The control plane closes the socket: the agent drains the backlog and
	// then sees the close, not its own read deadline.
	_ = first.SetReadDeadline(time.Now().Add(8 * time.Second))
	for {
		_, _, err := first.ReadMessage()
		if err == nil {
			continue
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatal("connection still open after a revocation met a full send queue")
		}
		break
	}

	second, _ := register(map[string]string{"node_secret": reg.NodeSecret})
	for {
		var msg struct {
			Type          string                 `json:"type"`
			ConsoleConfig *console.ConsoleConfig `json:"console_config"`
		}
		if err := second.ReadJSON(&msg); err != nil {
			t.Fatalf("waiting for the console config: %v", err)
		}
		if msg.Type == "config_update" && msg.ConsoleConfig != nil {
			if msg.ConsoleConfig.Enabled {
				t.Fatal("reconnect delivered console enabled; the revocation was lost")
			}
			return
		}
	}
}
