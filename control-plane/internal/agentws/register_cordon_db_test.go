// #140: register must not lift a cordon. A `draining` row whose disconnect was
// never observed (a control-plane restart drops every socket while the column
// keeps its value) still carries the admin's — or a fleet run's — intent.
package agentws

import (
	"context"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/admission"
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

func TestReconnectPreservesOwnedRestrictionAfterOfflineProjection(t *testing.T) {
	pool := testPool(t)
	s := storeWithMintedTokens(pool, nil)
	hostID := seedHostWithSecret(t, pool, "owned-reconnect-host", "secret-rh05")
	holds := admission.NewStore(pool)
	ctx := context.Background()
	if _, err := holds.Acquire(ctx, hostID, admission.Owner{Kind: admission.Platform,
		ID: "00000000-0000-0000-0000-000000000123"}, "Platform apply"); err != nil {
		t.Fatal(err)
	}
	if err := s.markOffline(ctx, hostID); err != nil {
		t.Fatal(err)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("disconnect status = %q, want draining while owner holds", got)
	}
	if _, err := s.reconnectHost(ctx, "owned-reconnect-host", "0.3.0", "secret-rh05"); err != nil {
		t.Fatal(err)
	}
	if got := hostStatus(t, pool, hostID); got != "draining" {
		t.Fatalf("reconnect status = %q, want draining while owner holds", got)
	}
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
	s := storeWithMintedTokens(pool, nil)
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

// recordRemoval writes the remove route's audit record for a host, `ago` in the past.
func recordRemoval(t *testing.T, pool *pgxpool.Pool, hostID string, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO admin_activity (action, target_type, target_id, details, created_at)
		VALUES ('platform.remove.host', 'host', $1, '{"node_name":"n","force":false}', now() - $2::interval)`, hostID, ago); err != nil {
		t.Fatalf("record removal: %v", err)
	}
}

func setDrainCreatedAt(t *testing.T, pool *pgxpool.Pool, hostID string, ago string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE host_admission_restrictions SET created_at = now() - $2::interval
		WHERE host_id = $1::uuid AND owner_kind = 'manual'`, hostID, ago); err != nil {
		t.Fatalf("set drain created_at: %v", err)
	}
}

// #366: a console removal cordons the host with the operator-drain owner when no drain is
// held. Adding the host back onto its row (a new enrollment) lifts that drain, and only
// that one: an operator's own drain, older than the removal, stays.
func TestReEnrollmentAfterAConsoleRemovalLiftsOnlyTheRemovalsDrain(t *testing.T) {
	pool := testPool(t)
	s := &agentStore{pool: pool}
	holds := admission.NewStore(pool)
	ctx := context.Background()
	const token = "shared-enrollment-token-366"

	// The removal's own drain: taken seconds before the removal was audited.
	res, err := s.enrollHost(ctx, "removed-host", "0.3.0", token, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holds.Acquire(ctx, res.HostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatal(err)
	}
	setDrainCreatedAt(t, pool, res.HostID, "2 hours")
	recordRemoval(t, pool, res.HostID, "7196 seconds")
	if err := s.markOffline(ctx, res.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enrollHost(ctx, "removed-host", "0.3.1", token, token); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "online" {
		t.Fatalf("status after the re-add = %q, want online (the removal's drain lifted)", got)
	}
	if r, _ := holds.List(ctx, res.HostID); len(r) != 0 {
		t.Fatalf("holds after the re-add = %+v, want none", r)
	}

	// An operator drain held before the removal: the route took none of its own.
	res2, err := s.enrollHost(ctx, "drained-then-removed", "0.3.0", token, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holds.Acquire(ctx, res2.HostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatal(err)
	}
	setDrainCreatedAt(t, pool, res2.HostID, "3 hours")
	recordRemoval(t, pool, res2.HostID, "1 hour")
	if err := s.markOffline(ctx, res2.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enrollHost(ctx, "drained-then-removed", "0.3.1", token, token); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := hostStatus(t, pool, res2.HostID); got != "draining" {
		t.Fatalf("status = %q, want draining: the operator's own drain stays", got)
	}

	// A platform owner's hold is never touched, even beside a removal's drain.
	res3, err := s.enrollHost(ctx, "removed-mid-run", "0.3.0", token, token)
	if err != nil {
		t.Fatal(err)
	}
	run := admission.Owner{Kind: admission.Platform, ID: "00000000-0000-0000-0000-000000000366"}
	if _, err := holds.Acquire(ctx, res3.HostID, run, admission.ReasonPlatformApply); err != nil {
		t.Fatal(err)
	}
	if _, err := holds.Acquire(ctx, res3.HostID, admission.ManualOwner, admission.ReasonManualDrain); err != nil {
		t.Fatal(err)
	}
	recordRemoval(t, pool, res3.HostID, "0 seconds")
	if err := s.markOffline(ctx, res3.HostID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enrollHost(ctx, "removed-mid-run", "0.3.1", token, token); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	r, _ := holds.List(ctx, res3.HostID)
	if len(r) != 1 || r[0].OwnerKind != admission.Platform {
		t.Fatalf("holds = %+v, want only the platform owner's", r)
	}
	if got := hostStatus(t, pool, res3.HostID); got != "draining" {
		t.Fatalf("status = %q, want draining while the platform owner holds", got)
	}
}

// TestEnrollHostKeepsADrainingHostDraining is the same rule on the enrol/upsert
// path: re-enrolling a known node_name is not an uncordon either.
func TestEnrollHostKeepsADrainingHostDraining(t *testing.T) {
	pool := testPool(t)
	s := storeWithMintedTokens(pool, nil)

	res, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.0", testEnrollmentToken)
	if err != nil {
		t.Fatalf("initial enroll: %v", err)
	}
	setHostStatus(t, pool, res.HostID, "draining")

	if _, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.1", testEnrollmentToken); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "draining" {
		t.Fatalf("status after a cordoned host's re-enrollment = %q, want draining", got)
	}

	setHostStatus(t, pool, res.HostID, "offline")
	if _, err := s.enrollHost(context.Background(), "cordoned-enroll-host", "0.3.2", testEnrollmentToken); err != nil {
		t.Fatalf("third enroll: %v", err)
	}
	if got := hostStatus(t, pool, res.HostID); got != "online" {
		t.Fatalf("status after an offline host's re-enrollment = %q, want online", got)
	}
}
