package agentws

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/admission"
	"github.com/accreleus/quasar/control-plane/internal/hostenroll"
)

func secretHash(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var hash string
	if err := pool.QueryRow(context.Background(),
		`SELECT node_secret_hash FROM hosts WHERE id::text = $1`, hostID).Scan(&hash); err != nil {
		t.Fatalf("read node_secret_hash: %v", err)
	}
	return hash
}

// A cordoned host is still connected. A second replica has no registry entry for it,
// so the row is all that can refuse a re-enrollment there.
func TestEnrollmentOnASecondReplicaCannotTakeOverAConnectedDrainingHost(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	replica := func() string {
		h := NewHandler(pool, log, NewRegistry(log), nil, nil, nil, nil)
		t.Cleanup(h.Close)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return "ws" + strings.TrimPrefix(srv.URL, "http")
	}
	register := func(url, token string) map[string]any {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if err := conn.WriteJSON(map[string]any{
			"type": "register", "node_name": "drained-on-a", "agent_version": "test",
			"auth": map[string]string{"enrollment_token": token},
		}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var reply map[string]any
		if err := conn.ReadJSON(&reply); err != nil {
			t.Fatalf("read the register reply: %v", err)
		}
		if reply["type"] == "registered" {
			if err := conn.WriteJSON(map[string]any{"type": "capacity",
				"host": map[string]any{"cpu_cores": 8, "mem_mb": 32000}, "gpus": []any{}}); err != nil {
				t.Fatal(err)
			}
		}
		return reply
	}

	incumbent := register(replica(), testEnrollmentToken)
	hostID, _ := incumbent["host_id"].(string)
	if incumbent["type"] != "registered" || hostID == "" {
		t.Fatalf("incumbent enrollment: %+v", incumbent)
	}
	if _, err := admission.NewStore(pool).AcquireOnline(ctx, hostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	var status string
	var stamped bool
	if err := pool.QueryRow(ctx, `SELECT status, agent_disconnected_at IS NOT NULL FROM hosts WHERE id=$1::uuid`, hostID).
		Scan(&status, &stamped); err != nil {
		t.Fatal(err)
	}
	if status != "draining" || stamped {
		t.Fatalf("connected cordoned host: status=%q disconnect stamped=%v, want draining and unstamped", status, stamped)
	}
	before := secretHash(t, pool, hostID)

	reply := register(replica(), boundEnrollmentToken(t, pool, "drained-on-a"))
	if reply["type"] != "error" || reply["code"] != "auth_failed" {
		t.Fatalf("re-enrollment on the second replica: got %v/%v, want an auth_failed error", reply["type"], reply["code"])
	}
	if after := secretHash(t, pool, hostID); after != before {
		t.Fatal("the connected host's node secret was rotated")
	}
}

// Two enrollments of a name that does not exist yet both pass the takeover guard, which
// finds no row. The loser reaches the row only at the upsert, and must not replace the
// secret the winner was just given.
func TestConcurrentFirstEnrollmentDoesNotReplaceTheWinnersSecret(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })
	token, bound, err := hostenroll.NewStore(pool).Mint(ctx, hostenroll.MintParams{CreatedBy: seedAdmin(t, pool), NodeName: "raced-first-enroll"})
	if err != nil {
		t.Fatalf("mint a single-use token bound to the name: %v", err)
	}

	// The winner, held uncommitted: the loser's guard cannot see it, its upsert waits on it.
	winner, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Rollback(ctx) //nolint:errcheck
	var hostID string
	if err := winner.QueryRow(ctx, `INSERT INTO hosts (node_name, status, node_secret_hash)
		VALUES ('raced-first-enroll', 'online', 'the-winners-hash') RETURNING id::text`).Scan(&hostID); err != nil {
		t.Fatal(err)
	}

	lost := make(chan error, 1)
	go func() {
		_, err := st.enrollHost(ctx, "raced-first-enroll", "0.1.0", bound)
		lost <- err
	}()
	waitUntil(t, "the losing enrollment to block on the winner's row", func() bool {
		var waiting bool
		_ = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%INSERT INTO hosts (node_name, agent_version%')`).Scan(&waiting)
		return waiting
	})
	if err := winner.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-lost; !errors.Is(err, ErrHostAgentConnected) {
		t.Fatalf("the losing first enrollment: got %v, want ErrHostAgentConnected", err)
	}
	if got := secretHash(t, pool, hostID); got != "the-winners-hash" {
		t.Fatalf("node_secret_hash = %q: the loser replaced the winner's secret", got)
	}
	if n := usedCount(t, pool, token.ID); n != 0 {
		t.Fatalf("the refused loser spent its single-use token: used_count = %d, want 0", n)
	}
}

// The recovery actor's dump import sets every imported host offline (quasar-recovery
// OLD_HOSTS_OFFLINE). A drain held as a restriction must come back when the host is re-added.
func TestReEnrollingAnOfflineHostWithAHeldDrainComesBackDraining(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	st := storeWithMintedTokens(pool, func(string) bool { return false })

	res, err := st.enrollHost(ctx, "imported-drained", "0.1.0", testEnrollmentToken)
	if err != nil {
		t.Fatalf("initial enrollment: %v", err)
	}
	if _, err := admission.NewStore(pool).AcquireOnline(ctx, res.HostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatalf("cordon: %v", err)
	}
	setHostStatus(t, pool, res.HostID, "offline")

	if _, err := st.enrollHost(ctx, "imported-drained", "0.1.0", boundEnrollmentToken(t, pool, "imported-drained")); err != nil {
		t.Fatalf("re-adding the imported host: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "draining" {
		t.Fatalf("status after the re-add = %q, want draining: the drain is still held", got)
	}
}
