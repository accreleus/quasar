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
	"github.com/jackc/pgx/v5/pgxpool"
)

type signalingFixture struct {
	h    *Handler
	pool *pgxpool.Pool
	url  string
}

func newSignalingFixture(t *testing.T) *signalingFixture {
	t.Helper()
	pool := testPool(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &signalingFixture{h: h, pool: pool, url: "ws" + strings.TrimPrefix(srv.URL, "http")}
}

// connect registers an agent for a new host and returns its socket and host id.
func (f *signalingFixture) connect(t *testing.T, node string) (*websocket.Conn, string) {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(f.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	a := typedAgent{ws: ws}
	a.send(t, map[string]any{"type": "register", "node_name": node, "agent_version": "test",
		"auth": map[string]string{"enrollment_token": testEnrollmentToken}})
	hostID, _ := a.readUntil(t, "registered")["host_id"].(string)
	a.send(t, map[string]any{"type": "capacity", "host": map[string]any{"cpu_cores": 8, "mem_mb": 32000}, "gpus": []any{}})
	return ws, hostID
}

func (f *signalingFixture) seedSession(t *testing.T, hostID string) string {
	t.Helper()
	ctx := context.Background()
	var userID, sessionID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO users(email,username,password_hash)
		VALUES (gen_random_uuid()::text || '@test', gen_random_uuid()::text, 'x') RETURNING id::text`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var appID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO apps(name) VALUES (gen_random_uuid()::text) RETURNING id::text`).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO sessions(user_id,app_id,host_id,state,width,height,fps,bitrate_kbps)
		VALUES ($1,$2,$3,'assigned',1280,720,60,6000) RETURNING id::text`, userID, appID, hostID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func signal(t *testing.T, ws *websocket.Conn, sessionID, msg string) {
	t.Helper()
	if err := ws.WriteJSON(map[string]any{"type": "signaling", "session_id": sessionID,
		"msg": map[string]string{"type": "offer", "sdp": msg}}); err != nil {
		t.Fatal(err)
	}
}

func recv(ch <-chan []byte) (string, bool) {
	select {
	case b := <-ch:
		return string(b), true
	case <-time.After(5 * time.Second):
		return "", false
	}
}

func silent(t *testing.T, ch <-chan []byte) {
	t.Helper()
	select {
	case b := <-ch:
		t.Fatalf("a browser received a frame from an agent that does not own its session: %s", b)
	case <-time.After(200 * time.Millisecond):
	}
}

// #1116: host A's agent names a session placed on host B. B's browser gets nothing, A's own
// session still relays, and B's agent relays its own.
func TestAgentSignalingForAnotherHostsSessionIsNotRelayed(t *testing.T) {
	f := newSignalingFixture(t)
	wsA, hostA := f.connect(t, "sig-owner-a")
	wsB, hostB := f.connect(t, "sig-owner-b")
	sessA, sessB := f.seedSession(t, hostA), f.seedSession(t, hostB)
	chA, chB := make(chan []byte, 8), make(chan []byte, 8)
	f.h.relay.Register(sessA, chA)
	f.h.relay.Register(sessB, chB)

	signal(t, wsA, sessB, "forged")
	signal(t, wsA, sessA, "own") // same read loop: once this lands, the forged frame was handled
	if got, ok := recv(chA); !ok || !strings.Contains(got, "own") {
		t.Fatalf("host A's own session frame = %q, %v", got, ok)
	}
	silent(t, chB)

	signal(t, wsB, sessB, "owner")
	if got, ok := recv(chB); !ok || !strings.Contains(got, "owner") {
		t.Fatalf("host B's own session frame = %q, %v", got, ok)
	}
}

// The coordinator inserts the placed row, then sends session_assign, so the owner's first frames
// can arrive after its connection was long established and after a foreign frame for the same
// id was refused. Placement is read per frame, not snapshotted at connect.
func TestSignalingFromTheOwnerAfterAnEarlierRefusalFlows(t *testing.T) {
	f := newSignalingFixture(t)
	wsA, hostA := f.connect(t, "sig-late-a")
	wsB, hostB := f.connect(t, "sig-late-b")
	sessA := f.seedSession(t, hostA)
	chB := make(chan []byte, 8)
	// B's session id exists only after a frame for it was refused to A.
	sessB := f.seedSession(t, hostB)
	f.h.relay.Register(sessB, chB)
	signal(t, wsA, sessB, "forged")
	signal(t, wsA, sessA, "marker")
	silent(t, chB)

	signal(t, wsB, sessB, "first-offer")
	if got, ok := recv(chB); !ok || !strings.Contains(got, "first-offer") {
		t.Fatalf("owner's first frame after the refusal = %q, %v", got, ok)
	}
}

func TestSignalingOwnersRefusalExpiresAndOwnerIsCached(t *testing.T) {
	f := newSignalingFixture(t)
	_, hostA := f.connect(t, "sig-ttl-a")
	_, hostB := f.connect(t, "sig-ttl-b")
	sess := f.seedSession(t, hostB)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := time.Now()
	o := newSignalingOwners(f.h.store, log, hostA)
	o.now = func() time.Time { return clock }
	ctx := context.Background()

	if o.allows(ctx, sess) {
		t.Fatal("a foreign session was allowed")
	}
	// Placement cannot change, so the refusal stands inside the window without a query.
	if _, err := f.pool.Exec(ctx, `UPDATE sessions SET host_id=$1::uuid WHERE id=$2::uuid`, hostA, sess); err != nil {
		t.Fatal(err)
	}
	if o.allows(ctx, sess) {
		t.Fatal("a refusal was not remembered inside its window")
	}
	clock = clock.Add(foreignTTL + time.Second)
	if !o.allows(ctx, sess) {
		t.Fatal("a refusal outlived its window")
	}
	f.pool.Exec(ctx, `DELETE FROM sessions WHERE id=$1::uuid`, sess)
	if !o.allows(ctx, sess) {
		t.Fatal("an owned session was not cached")
	}
	if o.allows(ctx, "not-a-uuid") {
		t.Fatal("a malformed id was allowed")
	}
}
