package session

import (
	"context"
	"errors"
	"testing"
)

// P3-04 host-offline failover: when a host's agent disconnects the reaper fails
// its sessions with state_detail = 'host_lost', frees their reservations, and
// never touches sessions on other hosts. Integration tests — need Postgres.

// TestHostLostStampedOnReap: a reaped session gets state=failed, state_detail=host_lost.
func TestHostLostStampedOnReap(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	n, err := store.ReapHost(ctx, s.hostID, "host agent connection lost")
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d sessions, want 1", n)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.State != StateFailed {
		t.Fatalf("state: got %s want failed", got.State)
	}
	if got.StateDetail == nil || *got.StateDetail != "host_lost" {
		d := "<nil>"
		if got.StateDetail != nil {
			d = *got.StateDetail
		}
		t.Fatalf("state_detail: got %q want host_lost", d)
	}
}

// TestMultiHostCorrectnessOnReap: reaped host's sessions fail with host_lost;
// the other host's sessions are completely untouched (multi-host correctness).
func TestMultiHostCorrectnessOnReap(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	setQuota(t, pool, s.userID, 10)
	ctx := context.Background()

	// Seed a second online host with the same capacity.
	h2, _ := seedSecondHost(t, pool, 16384, 4)

	// Launch two sessions (one per host via spread); use different users so the
	// per-user lock does not serialize them onto the same host.
	u2 := seedExtraUser(t, pool, 77, 5)
	sess1, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch 1: %v", err)
	}
	sess2, err := store.ScheduleAndCreate(ctx, launchAs(s, u2))
	if err != nil {
		t.Fatalf("launch 2: %v", err)
	}

	// Find which session is on host-1 and which is on host-2.
	var onH1, onH2 string
	if deref(sess1.HostID) == s.hostID {
		onH1, onH2 = sess1.ID, sess2.ID
	} else {
		onH1, onH2 = sess2.ID, sess1.ID
	}
	_ = h2 // h2 is the surviving host

	// Reap host-1 (simulate agent disconnect).
	n, err := store.ReapHost(ctx, s.hostID, "host agent connection lost")
	if err != nil {
		t.Fatalf("reap host-1: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d sessions, want 1", n)
	}

	// Host-1's session must be failed/host_lost.
	reaped, _ := store.Get(ctx, onH1)
	if reaped.State != StateFailed {
		t.Fatalf("host-1 session state: got %s want failed", reaped.State)
	}
	if reaped.StateDetail == nil || *reaped.StateDetail != "host_lost" {
		d := "<nil>"
		if reaped.StateDetail != nil {
			d = *reaped.StateDetail
		}
		t.Fatalf("host-1 session state_detail: got %q want host_lost", d)
	}

	// Host-2's session must be untouched (still assigned/starting/running).
	surviving, _ := store.Get(ctx, onH2)
	if surviving.State == StateFailed {
		t.Fatalf("host-2 session incorrectly failed after reaping host-1")
	}
	if surviving.StateDetail != nil && *surviving.StateDetail == "host_lost" {
		t.Fatalf("host-2 session incorrectly stamped host_lost")
	}
}

// TestReservationsFreedAfterReap: reaped sessions release their reservations so a
// new launch can succeed (the derived-availability model: failed = terminal).
func TestReservationsFreedAfterReap(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 1) // exactly 1 encode slot → only 1 concurrent session
	ctx := context.Background()

	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	// Second launch must fail — no capacity left.
	_, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if !errors.Is(err, ErrCapacityExhausted) && !errors.Is(err, ErrNoHostAvailable) {
		t.Fatalf("expected capacity error before reap, got: %v", err)
	}

	// Reap the host: reservations freed.
	if _, err := store.ReapHost(ctx, s.hostID, "host agent connection lost"); err != nil {
		t.Fatalf("reap: %v", err)
	}

	// Now a new launch must succeed — the slot is free again.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("launch after reap (reservation should be freed): %v", err)
	}
}

// TestRelaunachSkipsOfflineHost: after a host is reaped and flipped offline, a
// new launch goes to the surviving online host, never the dead one.
func TestRelaunachSkipsOfflineHost(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 4)
	ctx := context.Background()

	// Add a second online host.
	h2, _ := seedSecondHost(t, pool, 16384, 4)

	// Reap host-1 and flip it offline (agentws.markOffline does this on disconnect).
	if _, err := store.ReapHost(ctx, s.hostID, "host agent connection lost"); err != nil {
		t.Fatalf("reap: %v", err)
	}
	setHostStatusRaw(t, pool, s.hostID, "offline")

	// Relaunch must go to host-2 (the only online host).
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("relaunch after host-offline: %v", err)
	}
	if deref(sess.HostID) != h2 {
		t.Fatalf("relaunch landed on host %q, want surviving host %q", deref(sess.HostID), h2)
	}
}
