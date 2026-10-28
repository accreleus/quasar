package session

// #271: admission's recheck and the agent's capacity write both lock a host row
// and its GPU rows. Opposite orders would deadlock, and a launch that lost would
// be a 500. Both take the host row first; these tests hold them to it.

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Generous: a passing check returns at once, and the gates share the machine.
const lockOrderWait = 15 * time.Second

// How recheckQuery starts; no other admission statement starts this way.
const recheckHead = "SELECT h.status = 'online'"

// tupleWaitedOn waits for a backend running a statement that contains stmt to
// block on a row lock, and returns the table whose row it is queued on. stmt
// must sit in the statement's first 1024 bytes: pg_stat_activity keeps no more.
func tupleWaitedOn(t *testing.T, pool *pgxpool.Pool, stmt string) string {
	t.Helper()
	deadline := time.Now().Add(lockOrderWait)
	for time.Now().Before(deadline) {
		var rel string
		err := pool.QueryRow(context.Background(), `
			SELECT l.relation::regclass::text
			FROM pg_stat_activity a JOIN pg_locks l ON l.pid = a.pid
			WHERE a.datname = current_database() AND a.wait_event_type = 'Lock' AND a.pid <> pg_backend_pid()
			  AND position($1 in a.query) > 0
			  AND l.locktype = 'tuple' AND l.granted`, stmt).Scan(&rel)
		if err == nil {
			return rel
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no backend running %q blocked on a row lock", stmt)
	return ""
}

// rowLocked reports whether some transaction holds the row against FOR UPDATE.
func rowLocked(t *testing.T, pool *pgxpool.Pool, table, id string) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	must(t, err)
	defer tx.Rollback(ctx) //nolint:errcheck
	_, err = tx.Exec(ctx, `SELECT 1 FROM `+table+` WHERE id=$1::uuid FOR UPDATE NOWAIT`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	must(t, err)
	return false
}

// TestRecheckLocksHostBeforeGPU pins the recheck's row-lock order. Postgres
// locks in the order of the FOR UPDATE OF list, so `OF h, g` is host first;
// `OF g, h`, or no list at all, would be GPU first and invert it against every
// other writer of the two tables.
func TestRecheckLocksHostBeforeGPU(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
	defer cancel()
	var pgVersion string
	must(t, pool.QueryRow(ctx, `SHOW server_version`).Scan(&pgVersion))

	for _, tc := range []struct {
		held, heldID   string // the row another transaction holds
		other, otherID string // the recheck's other row
		otherLocked    bool   // whether the recheck holds it while it waits
	}{
		{"hosts", s.hostID, "gpus", s.gpuID, false},
		{"gpus", s.gpuID, "hosts", s.hostID, true},
	} {
		blocker, err := pool.Begin(ctx)
		must(t, err)
		t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
		_, err = blocker.Exec(ctx, `SELECT 1 FROM `+tc.held+` WHERE id=$1::uuid FOR UPDATE`, tc.heldID)
		must(t, err)

		launched := make(chan error, 1)
		go func() {
			_, err := store.ScheduleAndCreate(ctx, launchParams(s))
			launched <- err
		}()
		rel := tupleWaitedOn(t, pool, recheckHead)
		got := rowLocked(t, pool, tc.other, tc.otherID)
		t.Logf("PostgreSQL %s, %s row held: the recheck is queued on a %s row, its %s row locked = %v",
			pgVersion, tc.held, rel, tc.other, got)
		if rel != tc.held || got != tc.otherLocked {
			t.Fatalf("%s row held: want the recheck queued on it with its %s row locked = %v (host is locked first)",
				tc.held, tc.other, tc.otherLocked)
		}
		must(t, blocker.Rollback(ctx))
		if err := <-launched; err != nil {
			t.Fatalf("%s row held: launch after release: %v", tc.held, err)
		}
	}
}

// stmtGate parks its pool's connection just before the first statement that
// contains marker once armed, until open is called.
type stmtGate struct {
	marker  string
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
	open    func()
}

func (g *stmtGate) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, g.marker) && g.armed.CompareAndSwap(true, false) {
		close(g.reached)
		<-g.release
	}
	return ctx
}

func (*stmtGate) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (g *stmtGate) awaitReached(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(lockOrderWait):
		t.Fatalf("no statement containing %q was reached", g.marker)
	}
}

// A deadlockTimeout applies to every connection of the pool. Setting it needs a
// superuser, which the test database's role is.
func gatedPool(t *testing.T, marker string, deadlockTimeout ...string) (*pgxpool.Pool, *stmtGate) {
	t.Helper()
	g := &stmtGate{marker: marker, reached: make(chan struct{}), release: make(chan struct{})}
	g.open = sync.OnceFunc(func() { close(g.release) })
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	must(t, err)
	cfg.ConnConfig.Tracer = g
	for _, d := range deadlockTimeout {
		cfg.ConnConfig.RuntimeParams["deadlock_timeout"] = d
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	must(t, err)
	t.Cleanup(pool.Close)
	t.Cleanup(g.open) // before Close, which waits for a parked connection
	return pool, g
}

// TestLaunchAndCapacityWriteDoNotDeadlock runs a real launch against a real
// capacity message for the same host, each parked in turn holding the row the
// other asks for first. Opposite lock orders would end one of them with 40P01.
func TestLaunchAndCapacityWriteDoNotDeadlock(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*lockOrderWait)
	defer cancel()
	log := testLogger()

	// The capacity write parks after its host UPDATE, before it reaches gpus;
	// the launch parks after its recheck, holding both rows.
	agentPool, capacityGate := gatedPool(t, "UPDATE gpus SET reported = false")
	launchPool, launchGate := gatedPool(t, "INSERT INTO sessions (")

	const name = "lock-order-271"
	seedEnrollmentToken(t, pool, name)
	registry := agentws.NewRegistry(log)
	h := agentws.NewHandler(agentPool, log, registry, newTestCoordinator(t, NewStore(pool), registry, log), nil, nil, nil)
	t.Cleanup(h.Close)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	must(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(lockOrderWait))
	must(t, ws.WriteJSON(map[string]any{
		"type": "register", "node_name": name, "agent_version": "test",
		"auth": map[string]string{"enrollment_token": name},
	}))
	var registered map[string]any
	must(t, ws.ReadJSON(&registered))
	hostID, _ := registered["host_id"].(string)
	if hostID == "" {
		t.Fatalf("registered without host id: %+v", registered)
	}
	sendCapacity := func(cores int) {
		must(t, ws.WriteJSON(map[string]any{
			"type": "capacity", "host": map[string]any{"cpu_cores": cores, "mem_mb": 32000},
			"gpus": []map[string]any{{"index": 0, "vendor": "nvidia", "model": "test", "vram_mb_total": 16384, "encode_slots_total": 8}},
		}))
	}
	stored := func(cores int) bool {
		var got int
		err := pool.QueryRow(ctx, `SELECT cpu_cores FROM hosts WHERE id=$1::uuid`, hostID).Scan(&got)
		return err == nil && got == cores
	}
	awaitStored := func(cores int) {
		t.Helper()
		for deadline := time.Now().Add(lockOrderWait); !stored(cores); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("capacity report %d was never stored", cores)
			}
		}
	}
	sendCapacity(1)
	awaitStored(1)
	var gpuID string
	must(t, pool.QueryRow(ctx, `SELECT id::text FROM gpus WHERE host_id=$1::uuid`, hostID).Scan(&gpuID))

	s := seed(t, pool, 8)
	setQuota(t, pool, s.userID, 20)
	p := launchParams(s)
	p.PinHostID = hostID // the registered host, not seed's
	launched := make(chan error, 1)
	launch := func() {
		_, err := NewStore(launchPool).ScheduleAndCreate(ctx, p)
		launched <- err
	}

	// The capacity write holds the host row; the launch reaches its recheck.
	capacityGate.armed.Store(true)
	sendCapacity(2)
	capacityGate.awaitReached(t)
	go launch()
	if rel := tupleWaitedOn(t, pool, recheckHead); rel != "hosts" {
		t.Fatalf("capacity write holds the host row: the recheck waits on a %s row", rel)
	}
	if rowLocked(t, pool, "gpus", gpuID) {
		t.Fatal("the recheck holds the GPU row while it waits for the host row: the capacity write deadlocks on it")
	}
	capacityGate.open()
	if err := <-launched; err != nil {
		t.Fatalf("launch against a capacity write holding the host row: %v", err)
	}
	if !stored(2) {
		t.Fatal("the capacity write the launch waited for did not commit")
	}

	// The launch holds the host and GPU rows; the capacity write arrives.
	launchGate.armed.Store(true)
	go launch()
	launchGate.awaitReached(t)
	sendCapacity(3)
	if rel := tupleWaitedOn(t, pool, "UPDATE hosts SET cpu_cores"); rel != "hosts" {
		t.Fatalf("launch holds both rows: the capacity write waits on a %s row", rel)
	}
	launchGate.open()
	if err := <-launched; err != nil {
		t.Fatalf("launch with a capacity write queued behind it: %v", err)
	}
	awaitStored(3)
}
