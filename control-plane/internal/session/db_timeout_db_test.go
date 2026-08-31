package session

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/db"
)

// #416 — before this fix, a Store built over internal/db.Open carried no
// statement_timeout / lock_timeout, so a FOR UPDATE holder (a lock pile-up,
// odds raised by #414's Seq Scans) blocked Transition's lock-read forever —
// on context.Background(), with nothing to cancel it. This test holds the
// row lock from a second connection and asserts Transition (called with
// context.Background(), exactly as the agent read loop's per-message DB work
// does — #416) returns an error within 5s instead of hanging.
func TestTransitionBoundedByLockTimeout(t *testing.T) {
	pool := testDB(t)
	s := seed(t, pool, 4)
	ctx := context.Background()

	store := NewStore(pool)
	sess, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Hold the row's lock from a second, independent connection — mirrors a
	// concurrent FOR UPDATE holder (another in-flight Transition, or a stuck
	// long-running statement) rather than reusing the pool under test.
	dbURL := os.Getenv("TEST_DATABASE_URL")
	holderPool, err := db.Open(ctx, dbURL, 0, 0) // no timeout — this connection deliberately holds
	if err != nil {
		t.Fatalf("open holder pool: %v", err)
	}
	defer holderPool.Close()

	tx, err := holderPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder tx: %v", err)
	}
	var cur string
	if err := tx.QueryRow(ctx, `SELECT state FROM sessions WHERE id = $1::uuid FOR UPDATE`, sess.ID).Scan(&cur); err != nil {
		t.Fatalf("acquire holder lock: %v", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// A Store built with the #416 short lock_timeout — same shape as the
	// deliberate agentws bg-context calls: no caller-supplied deadline at all.
	shortPool, err := db.Open(ctx, dbURL, 2*time.Second, 2*time.Second)
	if err != nil {
		t.Fatalf("open short-timeout pool: %v", err)
	}
	defer shortPool.Close()
	shortStore := NewStore(shortPool)

	done := make(chan error, 1)
	go func() {
		_, err := shortStore.Transition(context.Background(), sess.ID, StateStarting, nil, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected Transition to fail while the row lock is held, got nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Transition did not return within 5s while the row lock was held — lock_timeout not applied")
	}
}
