// token_janitor_db_test.go — SweepTokens is the pass body the auth.token_janitor
// job (internal/jobs, wired in cmd/quasar-control/app.go) now drives instead of
// the 6h-hardcoded StartTokenJanitor ticker (WP2 §8.2). Same SQL as before;
// this asserts the SQL itself and the returned count the job records as its
// run summary.
package auth

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestSweepTokensDeletesExpiredAndRevokedOnly(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	user, err := svc.Register(ctx, "sweep@t.local", "sweepuser", "password12345")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	insert := func(expiresAgo time.Duration, revoked bool) {
		t.Helper()
		revokedAt := "NULL"
		if revoked {
			revokedAt = "now()"
		}
		_, err := pool.Exec(ctx, `INSERT INTO auth_tokens (user_id, token_hash, expires_at, revoked_at)
			VALUES ($1::uuid, md5(random()::text), now() - make_interval(secs => $2), `+revokedAt+`)`,
			user.ID, expiresAgo.Seconds())
		if err != nil {
			t.Fatalf("insert token: %v", err)
		}
	}

	insert(10*24*time.Hour, false) // expired well past the 7-day grace: sweepable
	insert(1*time.Hour, false)     // expired but within the 7-day grace: NOT swept
	insert(-1*time.Hour, true)     // not yet expired but revoked: sweepable
	insert(-24*time.Hour, false)   // neither expired nor revoked: NOT swept

	n, err := svc.SweepTokens(ctx, log)
	if err != nil {
		t.Fatalf("SweepTokens: %v", err)
	}
	if n != 2 {
		t.Fatalf("SweepTokens deleted = %d, want 2 (the expired-past-grace row and the revoked row)", n)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE user_id = $1::uuid`, user.ID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 2 {
		t.Fatalf("remaining auth_tokens rows = %d, want 2", remaining)
	}

	// A second pass with nothing left to sweep is a real success with a zero
	// count, not an error — the same "swallow, don't fail, but now RECORD it"
	// contract every adopted janitor keeps.
	if n, err := svc.SweepTokens(ctx, log); err != nil || n != 0 {
		t.Fatalf("second SweepTokens = (%d, %v), want (0, nil)", n, err)
	}
}
