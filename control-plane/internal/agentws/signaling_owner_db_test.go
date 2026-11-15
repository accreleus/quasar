package agentws

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
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

// A ::uuid cast accepts upper case, no hyphens and misplaced hyphens, so each spelling of an
// owned id passes the placement query. The relay keys its buffers on the raw string, and Forget
// evicts only the canonical one, so none may reach it.
func TestNonCanonicalSpellingsOfAnOwnedSessionAreRefused(t *testing.T) {
	f := newSignalingFixture(t)
	wsA, hostA := f.connect(t, "sig-spell-a")
	sess := f.seedSession(t, hostA)
	ch := make(chan []byte, 8)
	f.h.relay.Register(sess, ch)
	moved := []byte(sess)
	moved[7], moved[8] = moved[8], moved[7]
	for _, spelling := range []string{strings.ToUpper(sess), strings.ReplaceAll(sess, "-", ""), string(moved)} {
		signal(t, wsA, spelling, "variant")
	}
	signal(t, wsA, sess, "canonical")
	if got, ok := recv(ch); !ok || !strings.Contains(got, "canonical") {
		t.Fatalf("canonical frame = %q, %v", got, ok)
	}
	silent(t, ch)
	f.h.relay.mu.Lock()
	defer f.h.relay.mu.Unlock()
	if len(f.h.relay.pending) != 0 || len(f.h.relay.browsers) != 1 {
		t.Fatalf("a variant spelling reached the relay: pending=%d browsers=%d", len(f.h.relay.pending), len(f.h.relay.browsers))
	}
}

const canonicalSession = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func countingOwners(placed bool, err error, log *slog.Logger) (*signalingOwners, *int, *time.Time) {
	calls, clock := 0, time.Now()
	o := newSignalingOwners(nil, log, "host")
	o.lookup = func(context.Context, string) (bool, error) { calls++; return placed, err }
	o.now = func() time.Time { return clock }
	return o, &calls, &clock
}

func TestSignalingOwnersCacheHitAndRefusalWindow(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, calls, clock := countingOwners(true, nil, log)
	for i := 0; i < 3; i++ {
		if !o.allows(context.Background(), canonicalSession) {
			t.Fatal("owned session refused")
		}
	}
	if *calls != 1 {
		t.Fatalf("an owned session cost %d lookups, want 1", *calls)
	}

	o, calls, clock = countingOwners(false, nil, log)
	o.allows(context.Background(), canonicalSession)
	o.allows(context.Background(), canonicalSession)
	if *calls != 1 {
		t.Fatalf("a refusal cost %d lookups inside its window, want 1", *calls)
	}
	*clock = clock.Add(foreignTTL + time.Second)
	o.allows(context.Background(), canonicalSession)
	if *calls != 2 {
		t.Fatalf("a refusal outlived its window: %d lookups", *calls)
	}
}

// A non-canonical id is refused before any lookup: the placement query would accept it.
func TestSignalingOwnersRefuseNonCanonicalIDsWithoutALookup(t *testing.T) {
	o, calls, _ := countingOwners(true, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, id := range []string{"", "not-a-uuid", strings.ToUpper(canonicalSession), strings.ReplaceAll(canonicalSession, "-", "")} {
		if o.allows(context.Background(), id) {
			t.Fatalf("%q was allowed", id)
		}
	}
	if *calls != 0 {
		t.Fatalf("%d lookups for ids that cannot be canonical", *calls)
	}
}

type failureEvents struct {
	noopEvents
	mu       sync.Mutex
	failures int
}

func (e *failureEvents) AgentSignalingFailure(context.Context, string, string, bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
}

// A transient DB error loses a frame and never fails the session. A burst during the outage costs
// one lookup (the read loop blocks once per window, not once per frame), the next window looks up
// again, and the log is throttled.
func TestSignalingLookupFailureDropsTheBurstWithOneLookup(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ev := &failureEvents{}
	h := NewHandler(nil, log, nil, ev, nil, nil, nil)
	t.Cleanup(h.Close)
	o, calls, clock := countingOwners(false, errors.New("db down"), log)
	ch := make(chan []byte, 64)
	h.relay.Register(canonicalSession, ch)
	send := func(n int) {
		for i := 0; i < n; i++ {
			h.relayAgentSignaling(context.Background(), o, "host", SignalingEnvelope{SessionID: canonicalSession, Msg: []byte(`{"type":"bye"}`)})
		}
	}

	const burst = 50
	send(burst)
	if *calls != 1 {
		t.Fatalf("a burst of %d frames during a failing lookup cost %d lookups, want 1", burst, *calls)
	}
	if ev.failures != 0 {
		t.Fatalf("a lookup error failed the session %d times", ev.failures)
	}
	silent(t, ch)
	*clock = clock.Add(unknownTTL + time.Second)
	send(1)
	if *calls != 2 {
		t.Fatalf("lookups after the window = %d, want 2", *calls)
	}
	*clock = clock.Add(unknownTTL + time.Second)
	send(burst)
	if n := strings.Count(buf.String(), "placement lookup failed"); n != 1 {
		t.Fatalf("lookup error logged %d times in under %s, want 1", n, ownerLogEvery)
	}
}
