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

// parkReconnectUpdate holds the first statement containing marker until release closes.
type parkReconnectUpdate struct {
	marker   string
	armed    atomic.Bool
	reached  chan struct{}
	release  chan struct{}
	released atomic.Bool
}

func (p *parkReconnectUpdate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, p.marker) && p.armed.CompareAndSwap(true, false) {
		close(p.reached)
		<-p.release
	}
	return ctx
}

func (*parkReconnectUpdate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (p *parkReconnectUpdate) open() {
	if p.released.CompareAndSwap(false, true) {
		close(p.release)
	}
}

// A re-enrollment that rotates the node secret between a reconnect's secret check and its
// write must refuse the reconnect: the retired secret may not replace the new agent (#618).
func TestReconnectWithASecretRotatedSinceItsLookupIsRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	park := &parkReconnectUpdate{marker: "WITH old AS", reached: make(chan struct{}), release: make(chan struct{})}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = park
	gated, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gated.Close)
	t.Cleanup(park.open) // before Close, which waits for the parked connection

	registry := NewRegistry(log)
	h := NewHandler(gated, log, registry, nil, nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	const node = "rotated-under-reconnect"
	register := func(auth map[string]string) (*websocket.Conn, map[string]any) {
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

	old, first := register(map[string]string{"enrollment_token": testEnrollmentToken})
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
	park.armed.Store(true)
	stale := make(chan map[string]any, 1)
	go func() {
		_, reply := register(map[string]string{"node_secret": retired})
		stale <- reply
	}()
	select {
	case <-park.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the reconnect never reached its write")
	}

	current, reenrolled := register(map[string]string{"enrollment_token": boundEnrollmentToken(t, pool, node)})
	if reenrolled["type"] != "registered" || reenrolled["host_id"] != hostID {
		t.Fatalf("re-enrollment while the reconnect is parked: %+v", reenrolled)
	}
	waitUntil(t, "the re-enrolled agent to hold the host's connection", func() bool {
		c, ok := registry.get(hostID)
		return ok && c.ws.RemoteAddr().String() == current.LocalAddr().String()
	})

	park.open()
	if reply := <-stale; reply["type"] != "error" || reply["code"] != "auth_failed" {
		t.Fatalf("reconnect with the rotated-out secret: got %v/%v, want an auth_failed error", reply["type"], reply["code"])
	}
	if c, ok := registry.get(hostID); !ok || c.ws.RemoteAddr().String() != current.LocalAddr().String() {
		t.Fatal("the stale reconnect replaced the re-enrolled agent's connection")
	}
	sum := sha256.Sum256([]byte(reenrolled["node_secret"].(string)))
	if got := secretHash(t, pool, hostID); got != hex.EncodeToString(sum[:]) {
		t.Fatal("the host no longer holds the re-enrolled agent's secret")
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
	if err := st.markOffline(ctx, first.HostID); err != nil {
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
	res := <-rotated
	sum := sha256.Sum256([]byte(res.NodeSecret))
	if got := secretHash(t, pool, first.HostID); got != hex.EncodeToString(sum[:]) {
		t.Fatal("the host no longer holds the re-enrolled agent's secret")
	}
	if got := hostStatus(t, pool, first.HostID); got != "online" {
		t.Fatalf("status = %q after the re-enrollment, want online", got)
	}
}
