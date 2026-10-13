package session

// #477: a console relaunch refused because the previous session's home hold
// is settling must follow the agent's cleanup proof on the same socket, with
// no further capacity report or connector event.

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/console"
	"github.com/accreleus/quasar/control-plane/internal/storage"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConsoleRelaunchFollowsTheHomeProofOnTheSameSocket(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	logs := &lockedLog{}
	log := slog.New(slog.NewTextHandler(logs, nil))
	const token = "console-relaunch-477"
	seedEnrollmentToken(t, pool, token)
	var userID, appID string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('relaunch477@test.local','relaunch477','x') RETURNING id::text`).Scan(&userID))
	must(t, pool.QueryRow(ctx, `INSERT INTO apps
		(name, kind, default_vram_mb, default_encode_slots, default_width, default_height,
		 default_fps, default_bitrate_kbps, runtime_spec, managed_home, home_container_path)
		VALUES ('relaunch-desktop', 'desktop', 512, 1, 1280, 720, 60, 2000,
		 '{"direct_display":true}', true, '/home/quasar') RETURNING id::text`).Scan(&appID))
	entitleAll(t, pool, appID)
	// The relaunch takes its own hold; the next test's truncate refuses one.
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `UPDATE managed_home_claims SET
			pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid`, userID); err != nil {
			t.Errorf("clear test hold: %v", err)
		}
	})

	registry := agentws.NewRegistry(log)
	coord := newTestCoordinator(t, NewStore(pool), registry, log,
		WithHomeProvider(storage.NewLocal(pool, testHomeRoot)))
	consoles := console.NewStore(pool)
	h := agentws.NewHandler(pool, log, registry, coord, nil, nil, consoles)
	coord.ConsoleReeval = h.ConsoleSessionTerminated
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	must(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(15 * time.Second))

	must(t, ws.WriteJSON(map[string]any{
		"type": "register", "node_name": "console-relaunch-477", "agent_version": "test",
		"auth": map[string]string{"enrollment_token": token}, "terminal_home_cleanup_v1": true,
	}))
	var registered map[string]any
	must(t, ws.ReadJSON(&registered))
	hostID, _ := registered["host_id"].(string)
	if hostID == "" {
		t.Fatalf("registered without host id: %+v", registered)
	}

	// The previous console launch was reaped on a disconnect; its home hold
	// still awaits the agent's qualified terminal.
	seedHome(t, pool, userID, appID, hostID)
	var holder string
	must(t, pool.QueryRow(ctx, `INSERT INTO sessions
		(user_id,app_id,host_id,state,state_detail,width,height,fps,bitrate_kbps,ended_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'failed','host_lost',1920,1080,60,2000,now())
		RETURNING id::text`, userID, appID, hostID).Scan(&holder))
	_, err = pool.Exec(ctx, `INSERT INTO managed_home_claims
		(user_id,canonical_app_id,host_id,state,pending_home_session_id,pending_home_token,pending_home_started_at)
		VALUES ($1::uuid,$2::uuid,$3::uuid,'reserved',$4::uuid,gen_random_uuid(),now())`, userID, appID, hostID, holder)
	must(t, err)
	must(t, consoles.Upsert(ctx, hostID, map[string]any{
		"enabled": true, "auto_start_on_display": true, "default_app": appID, "default_user": userID,
	}, nil))

	must(t, ws.WriteJSON(map[string]any{
		"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000},
		"gpus":                 []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 2}},
		"console_capabilities": map[string]any{"connectors": []string{"DP-1"}, "input_devices": []any{}},
	}))

	// The control plane asks the agent to prove the held session's cleanup.
	for {
		msg := readAgentCommand(t, ws)
		if msg["type"] == "session_stop" && msg["session_id"] == holder {
			break
		}
	}
	if !logs.contains(ErrConsoleHomeSettling.Error()) {
		t.Fatal("the handshake capacity's auto-start was not refused as a settling hold")
	}
	if n := otherSessions(t, pool, userID, holder); n != 0 {
		t.Fatalf("%d session(s) launched over the held home", n)
	}

	// The qualified terminal is the only thing the agent sends next.
	must(t, ws.WriteJSON(map[string]any{"type": "session_state", "session_id": holder, "state": "stopped"}))
	var relaunched string
	for relaunched == "" {
		msg := readAgentCommand(t, ws)
		switch msg["type"] {
		case "session_assign":
			must(t, ws.WriteJSON(map[string]any{"type": "ack", "id": msg["id"], "ok": true}))
		case "session_start":
			must(t, ws.WriteJSON(map[string]any{"type": "ack", "id": msg["id"], "ok": true}))
			relaunched, _ = msg["session_id"].(string)
		}
	}
	if relaunched == holder || otherSessions(t, pool, userID, holder) != 1 {
		t.Fatalf("relaunch = %s, want one new console session", relaunched)
	}

	must(t, ws.Close())
	waitFor(t, func() bool {
		var state string
		err := pool.QueryRow(ctx, `SELECT state FROM sessions WHERE id=$1::uuid`, relaunched).Scan(&state)
		return err == nil && State(state).IsTerminal()
	})
}

func readAgentCommand(t *testing.T, ws *websocket.Conn) map[string]any {
	t.Helper()
	var msg map[string]any
	must(t, ws.ReadJSON(&msg))
	return msg
}

func otherSessions(t *testing.T, pool *pgxpool.Pool, userID, except string) int {
	t.Helper()
	var n int
	must(t, pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM sessions
		WHERE user_id=$1::uuid AND id<>$2::uuid`, userID, except).Scan(&n))
	return n
}
