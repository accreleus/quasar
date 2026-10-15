package agentws

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The one production call of reconnectHost: handleRegister must hand it the commit the
// agent reported. Branch builds all report version "dev", so the commit is the only
// thing that tells an update from a blip there.
func TestRegisterReconnectOnAChangedCommitIsARestart(t *testing.T) {
	pool := testPool(t)
	withRestartGap(t, time.Hour) // the gap alone never classifies here
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	register := func(commit string, auth map[string]string) RegisteredMsg {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.WriteJSON(map[string]any{
			"type": "register", "node_name": "build-restart-host", "agent_version": "dev",
			"source_commit": commit, "auth": auth,
		}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var reg RegisteredMsg
		if err := conn.ReadJSON(&reg); err != nil || reg.HostID == "" {
			t.Fatalf("register: %v %+v", err, reg)
		}
		return reg
	}
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	reg := register(a, map[string]string{"enrollment_token": testEnrollmentToken})
	reconnect := map[string]string{"node_secret": reg.NodeSecret}
	enrolled := readRestartFields(t, pool, reg.HostID)

	for _, step := range []struct {
		name      string
		commit    string
		wantCount int32
		wantMoved bool
	}{
		{"same build", a, 0, false},
		{"replaced by build B", b, 1, true},
		{"same process again on B", b, 1, false},
	} {
		before := readRestartFields(t, pool, reg.HostID)
		register(step.commit, reconnect)
		after := readRestartFields(t, pool, reg.HostID)
		if after.restartCount != step.wantCount {
			t.Errorf("%s: agent_restart_count = %d, want %d", step.name, after.restartCount, step.wantCount)
		}
		if moved := !after.processStarted.Equal(*before.processStarted); moved != step.wantMoved {
			t.Errorf("%s: agent_process_started_at moved = %v, want %v", step.name, moved, step.wantMoved)
		}
	}
	if !readRestartFields(t, pool, reg.HostID).processStarted.After(*enrolled.processStarted) {
		t.Error("agent_process_started_at did not move past the enrollment's after the replacement")
	}
}
