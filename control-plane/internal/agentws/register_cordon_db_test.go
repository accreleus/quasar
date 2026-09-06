// #140: register must not lift a cordon. A `draining` row whose disconnect was
// never observed (a control-plane restart drops every socket while the column
// keeps its value) still carries the admin's — or a fleet run's — intent.
package agentws

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hostStatus reads the scheduling status column back.
func hostStatus(t *testing.T, pool *pgxpool.Pool, hostID string) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(),
		`SELECT status FROM hosts WHERE id::text = $1`, hostID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func setHostStatus(t *testing.T, pool *pgxpool.Pool, hostID, status string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE hosts SET status = $2 WHERE id::text = $1`, hostID, status); err != nil {
		t.Fatalf("set status %q: %v", status, err)
	}
}

// TestReconnectHostKeepsADrainingHostDraining: the reconnect path's business is
// offline → online, never draining → online. UncordonHost is what lifts a
// cordon, and it already handles a connected draining host.
func TestReconnectHostKeepsADrainingHostDraining(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	hostID := seedHostWithSecret(t, pool, "cordoned-reconnect-host", "secret-140")

	setHostStatus(t, pool, hostID, "draining")
	if _, err := s.reconnectHost(context.Background(), "cordoned-reconnect-host", "0.3.0", "secret-140"); err != nil {
		t.Fatalf("reconnectHost: %v", err)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("status after a cordoned host's reconnect = %q, want draining", got)
	}

	// The ordinary case is untouched: an offline host's reconnect brings it back.
	setHostStatus(t, pool, hostID, "offline")
	if _, err := s.reconnectHost(context.Background(), "cordoned-reconnect-host", "0.3.0", "secret-140"); err != nil {
		t.Fatalf("second reconnectHost: %v", err)
	}
	if got := hostStatus(t, pool, hostID); got != "online" {
		t.Fatalf("status after an offline host's reconnect = %q, want online", got)
	}
}

// TestEnrollHostKeepsADrainingHostDraining is the same rule on the enrol/upsert
// path: re-enrolling a known node_name is not an uncordon either.
func TestEnrollHostKeepsADrainingHostDraining(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}

	const token = "shared-enrollment-token-140"
	res, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.0", token, token)
	if err != nil {
		t.Fatalf("initial enroll: %v", err)
	}
	setHostStatus(t, pool, res.HostID, "draining")

	if _, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.1", token, token); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "draining" {
		t.Fatalf("status after a cordoned host's re-enrollment = %q, want draining", got)
	}

	setHostStatus(t, pool, res.HostID, "offline")
	if _, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.2", token, token); err != nil {
		t.Fatalf("third enroll: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "online" {
		t.Fatalf("status after an offline host's re-enrollment = %q, want online", got)
	}
}
