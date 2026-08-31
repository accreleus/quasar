package session

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setQuota overrides a user's max_concurrent_sessions for test isolation.
func setQuota(t *testing.T, pool *pgxpool.Pool, userID string, limit int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE users SET max_concurrent_sessions = $2 WHERE id::text = $1`,
		userID, limit); err != nil {
		t.Fatalf("set quota: %v", err)
	}
}

// TestQuotaDenyAtLimit: a user whose active-session count reaches max_concurrent_sessions
// is rejected with ErrSessionQuotaExceeded; stopping one session allows the next launch.
func TestQuotaDenyAtLimit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	// Large encode-slot count so GPU capacity is never the binding constraint.
	s := seed(t, pool, 10)
	ctx := context.Background()

	// Set limit to 1 so the second launch is refused immediately.
	setQuota(t, pool, s.userID, 1)

	// First launch — under quota.
	first, err := store.ScheduleAndCreate(ctx, launchParams(s))
	if err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if first.State != StateAssigned {
		t.Fatalf("first state: got %s want assigned", first.State)
	}

	// Second launch — at limit; must be rejected with no row persisted.
	before := countSessions(t, pool)
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrSessionQuotaExceeded) {
		t.Fatalf("second launch: got %v want ErrSessionQuotaExceeded", err)
	}
	if after := countSessions(t, pool); after != before {
		t.Fatalf("quota_exceeded persisted a row: %d → %d", before, after)
	}

	// Drive first session terminal → quota slot released.
	if _, err := store.Transition(ctx, first.ID, StateStopped, nil, nil); err != nil {
		t.Fatalf("stop first: %v", err)
	}

	// Third launch — now under quota again.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("third launch after release: %v", err)
	}
}

// TestQuotaAllowBelowLimit: a user with limit=2 can hold two active sessions.
func TestQuotaAllowBelowLimit(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	s := seed(t, pool, 10)
	ctx := context.Background()

	setQuota(t, pool, s.userID, 2)

	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); err != nil {
		t.Fatalf("second launch (at limit−1): %v", err)
	}
	// Third launch must be refused.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(s)); !errors.Is(err, ErrSessionQuotaExceeded) {
		t.Fatalf("third launch: got %v want ErrSessionQuotaExceeded", err)
	}
}

// TestQuotaPerUserIsolation: user A exhausting their quota must not block user B.
func TestQuotaPerUserIsolation(t *testing.T) {
	pool := testDB(t)
	store := NewStore(pool)
	ctx := context.Background()

	// Seed user A with quota=1.
	a := seed(t, pool, 10)
	setQuota(t, pool, a.userID, 1)

	// Seed user B separately (shares the same app+host+GPU).
	var bUserID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (email, username, password_hash)
		VALUES ('b@test.local','b','x') RETURNING id::text`).Scan(&bUserID); err != nil {
		t.Fatalf("seed user B: %v", err)
	}
	setQuota(t, pool, bUserID, 2)

	bParams := launchParams(a)
	bParams.UserID = bUserID

	// A fills their quota.
	if _, err := store.ScheduleAndCreate(ctx, launchParams(a)); err != nil {
		t.Fatalf("user A first launch: %v", err)
	}
	if _, err := store.ScheduleAndCreate(ctx, launchParams(a)); !errors.Is(err, ErrSessionQuotaExceeded) {
		t.Fatalf("user A second launch: got %v want ErrSessionQuotaExceeded", err)
	}

	// B still launches fine.
	if _, err := store.ScheduleAndCreate(ctx, bParams); err != nil {
		t.Fatalf("user B launch: %v", err)
	}
}
