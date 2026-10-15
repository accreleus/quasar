package agentws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
)

// stmtGate, once armed, holds the first statement containing marker until open.
type stmtGate struct {
	marker  string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	opened  atomic.Bool
}

func newStmtGate(marker string) *stmtGate {
	return &stmtGate{marker: marker, reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *stmtGate) open() {
	if g.opened.CompareAndSwap(false, true) {
		close(g.release)
	}
}

func (g *stmtGate) awaitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatalf("no statement containing %q was reached", g.marker)
	}
}

type gateTracer []*stmtGate

func (gates gateTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	for _, g := range gates {
		if strings.Contains(d.SQL, g.marker) && g.armed.CompareAndSwap(true, false) {
			close(g.reached)
			<-g.release
		}
	}
	return ctx
}

func (gateTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// gatedHandler serves agent websockets from a handler whose pool parks on gates.
func gatedHandler(t *testing.T, pool *pgxpool.Pool, gates ...*stmtGate) (*Registry, string) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = gateTracer(gates)
	gated, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gated.Close)
	for _, g := range gates {
		t.Cleanup(g.open) // before Close, which waits for a parked connection
	}
	registry := NewRegistry(log)
	h := NewHandler(gated, log, registry, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return registry, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// wsRegister registers as node and, once registered, sends its capacity. Safe off the test goroutine.
func wsRegister(t *testing.T, url, node string, auth map[string]string) (*websocket.Conn, map[string]any) {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Error(err)
		return nil, nil
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteJSON(map[string]any{
		"type": "register", "node_name": node, "agent_version": "test", "auth": auth,
	}); err != nil {
		t.Error(err)
		return nil, nil
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var reply map[string]any
	if err := conn.ReadJSON(&reply); err != nil {
		t.Errorf("read the register reply: %v", err)
		return nil, nil
	}
	if reply["type"] == "registered" {
		if err := conn.WriteJSON(map[string]any{"type": "capacity",
			"host": map[string]any{"cpu_cores": 8, "mem_mb": 32000}, "gpus": []any{}}); err != nil {
			t.Error(err)
		}
	}
	return conn, reply
}

func holds(registry *Registry, hostID string, agent *websocket.Conn) bool {
	c, ok := registry.get(hostID)
	return ok && agent != nil && c.ws.RemoteAddr().String() == agent.LocalAddr().String()
}

func hashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// A re-enrollment that rotates the node secret between a reconnect's secret check and its
// write must refuse the reconnect: the retired secret may not replace the new agent (#618).
func TestReconnectWithASecretRotatedSinceItsLookupIsRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	beforeWrite := newStmtGate("WITH old AS")
	registry, url := gatedHandler(t, pool, beforeWrite)
	const node = "rotated-under-reconnect"

	old, first := wsRegister(t, url, node, map[string]string{"enrollment_token": testEnrollmentToken})
	hostID, _ := first["host_id"].(string)
	retired, _ := first["node_secret"].(string)
	if first["type"] != "registered" || hostID == "" || retired == "" {
		t.Fatalf("first enrollment: %+v", first)
	}
	_ = old.Close()
	waitUntil(t, "the first agent's disconnect to be recorded", func() bool {
		var gone bool
		_ = pool.QueryRow(ctx, `SELECT status = 'offline' AND agent_disconnected_at IS NOT NULL
			FROM hosts WHERE id = $1::uuid`, hostID).Scan(&gone)
		return gone && !registry.IsConnected(hostID)
	})

	// The stale reconnect passes its secret check, then parks before its write.
	beforeWrite.armed.Store(true)
	stale := make(chan map[string]any, 1)
	go func() {
		_, reply := wsRegister(t, url, node, map[string]string{"node_secret": retired})
		stale <- reply
	}()
	beforeWrite.awaitReached(t)

	current, reenrolled := wsRegister(t, url, node, map[string]string{"enrollment_token": boundEnrollmentToken(t, pool, node)})
	if reenrolled["type"] != "registered" || reenrolled["host_id"] != hostID {
		t.Fatalf("re-enrollment while the reconnect is parked: %+v", reenrolled)
	}
	waitUntil(t, "the re-enrolled agent to hold the host's connection", func() bool { return holds(registry, hostID, current) })

	beforeWrite.open()
	if reply := <-stale; reply["type"] != "error" || reply["code"] != "auth_failed" {
		t.Fatalf("reconnect with the rotated-out secret: got %v/%v, want an auth_failed error", reply["type"], reply["code"])
	}
	if !holds(registry, hostID, current) {
		t.Fatal("the stale reconnect replaced the re-enrolled agent's connection")
	}
	if got := secretHash(t, pool, hostID); got != hashOf(reenrolled["node_secret"].(string)) {
		t.Fatal("the host no longer holds the re-enrolled agent's secret")
	}
}

// A reconnect that committed while the host's previous connection was closing keeps the
// host live: that connection's markOffline waits on the reconnect's row lock and must not
// stamp the row afterwards, or a re-enrollment rotates the secret before the reconnect's
// socket is registered, and that socket then replaces the re-enrolled agent (#618).
func TestAClosingConnectionDoesNotReopenAHostAReconnectClaimed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	inTx := newStmtGate("vram_sample_agent_ms = NULL")          // holding the row lock
	committed := newStmtGate("recovery_actor_source_commit = ") // before registry.add
	registry, url := gatedHandler(t, pool, inTx, committed)
	const node = "closing-under-reconnect"

	old, first := wsRegister(t, url, node, map[string]string{"enrollment_token": testEnrollmentToken})
	hostID, _ := first["host_id"].(string)
	secret, _ := first["node_secret"].(string)
	if first["type"] != "registered" || hostID == "" || secret == "" {
		t.Fatalf("first enrollment: %+v", first)
	}
	waitUntil(t, "the first connection to be registered", func() bool { return holds(registry, hostID, old) })

	inTx.armed.Store(true)
	reconnected := make(chan *websocket.Conn, 1)
	go func() {
		conn, reply := wsRegister(t, url, node, map[string]string{"node_secret": secret})
		if reply["type"] != "registered" {
			t.Errorf("the reconnect: %+v", reply)
		}
		reconnected <- conn
	}()
	inTx.awaitReached(t)

	_ = old.Close()
	waitUntil(t, "the closing connection's markOffline to wait on the reconnect's row lock", func() bool {
		var waiting bool
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%agent_disconnected_at=now()%')`).Scan(&waiting)
		return waiting
	})
	committed.armed.Store(true)
	inTx.open()
	committed.awaitReached(t)
	waitUntil(t, "the closing connection's markOffline to finish", func() bool {
		var running bool
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid <> pg_backend_pid() AND state = 'active' AND query LIKE '%agent_disconnected_at=now()%')`).Scan(&running)
		return !running
	})

	_, reenrolled := wsRegister(t, url, node, map[string]string{"enrollment_token": boundEnrollmentToken(t, pool, node)})
	committed.open()
	current := <-reconnected
	if reenrolled["type"] != "error" || reenrolled["code"] != "auth_failed" {
		t.Errorf("re-enrollment while the reconnect's socket is being registered: got %v/%v, want an auth_failed error",
			reenrolled["type"], reenrolled["code"])
	}
	waitUntil(t, "the reconnect to hold the host's connection", func() bool { return holds(registry, hostID, current) })
	if got := secretHash(t, pool, hostID); got != hashOf(secret) {
		t.Fatal("the host's secret is not the one its connected agent authenticated with")
	}
}

// The same race with the reconnect's write already waiting on the enrollment's row lock:
// it must re-read the rotated row once the enrollment commits, not write over it.
func TestReconnectWaitingOnARotatingEnrollmentIsRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })
	const node = "rotated-while-reconnect-waits"
	first, err := st.enrollHost(ctx, node, "0.1.0", testEnrollmentToken)
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	if err := st.markOffline(ctx, first.HostID, first.RegisteredAt); err != nil {
		t.Fatal(err)
	}

	locked, release := make(chan struct{}), make(chan struct{})
	st.redeemEnrollment = func(ctx context.Context, db hostenroll.DBTX, plaintext, nodeName string) (bool, error) {
		bound, err := hostenroll.Redeem(ctx, db, plaintext, nodeName)
		if err == nil {
			err = db.QueryRow(ctx, `SELECT 1 FROM hosts WHERE node_name = $1 FOR UPDATE`, nodeName).Scan(new(int))
		}
		close(locked)
		<-release
		return bound, err
	}
	rotated := make(chan registerResult, 1)
	go func() {
		res, err := st.enrollHost(ctx, node, "0.1.0", boundEnrollmentToken(t, pool, node))
		if err != nil {
			t.Errorf("re-enrollment: %v", err)
		}
		rotated <- res
	}()
	<-locked

	stale := make(chan error, 1)
	go func() {
		_, err := st.reconnectHost(ctx, node, "0.1.0", nil, first.NodeSecret)
		stale <- err
	}()
	waitUntil(t, "the reconnect's write to wait on the enrollment's row lock", func() bool {
		var waiting bool
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%WITH old AS%')`).Scan(&waiting)
		return waiting
	})
	close(release)

	if err := <-stale; !errors.Is(err, ErrInvalidNodeSecret) {
		t.Fatalf("reconnect with the rotated-out secret: got %v, want ErrInvalidNodeSecret", err)
	}
	if got := secretHash(t, pool, first.HostID); got != hashOf((<-rotated).NodeSecret) {
		t.Fatal("the host no longer holds the re-enrolled agent's secret")
	}
	if got := hostStatus(t, pool, first.HostID); got != "online" {
		t.Fatalf("status = %q after the re-enrollment, want online", got)
	}
}
