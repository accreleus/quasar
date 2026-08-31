package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestClaimSetupCreatesAdminAndAuthenticates is the core first-run guarantee:
// on an empty DB, ClaimSetup mints the first admin and the returned token both
// exists and resolves back to that admin (role=admin).
func TestClaimSetupCreatesAdminAndAuthenticates(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	exists, err := svc.AdminExists(ctx)
	if err != nil {
		t.Fatalf("AdminExists: %v", err)
	}
	if exists {
		t.Fatal("admin_exists true on a freshly-truncated DB")
	}

	tok, err := svc.ClaimSetup(ctx, "Owner@Example.com", "owner", "correct-horse-battery", "test-agent", "")
	if err != nil {
		t.Fatalf("ClaimSetup: %v", err)
	}
	if tok.Plaintext == "" {
		t.Fatal("claim returned an empty access token")
	}
	if tok.User.Role != RoleAdmin {
		t.Fatalf("new user role = %q, want admin", tok.User.Role)
	}
	if tok.User.Email != "owner@example.com" {
		t.Fatalf("email = %q, want lower-cased owner@example.com", tok.User.Email)
	}

	// The token must authenticate and resolve to the same admin.
	u, _, err := svc.Authenticate(ctx, tok.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate(claim token): %v", err)
	}
	if u.ID != tok.User.ID || u.Role != RoleAdmin {
		t.Fatalf("authenticated user = %+v, want the claimed admin %+v", u, tok.User)
	}

	if exists, err = svc.AdminExists(ctx); err != nil || !exists {
		t.Fatalf("AdminExists after claim = (%v, %v), want (true, nil)", exists, err)
	}
}

// TestClaimSetupRefusedWhenAdminExists pins gate 1: once any admin exists the
// claim returns ErrSetupAlreadyComplete and creates no second admin.
func TestClaimSetupRefusedWhenAdminExists(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	if _, err := svc.ClaimSetup(ctx, "first@example.com", "first", "correct-horse-battery", "t", ""); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	_, err := svc.ClaimSetup(ctx, "second@example.com", "second", "correct-horse-battery", "t", "")
	if !errors.Is(err, ErrSetupAlreadyComplete) {
		t.Fatalf("second claim err = %v, want ErrSetupAlreadyComplete", err)
	}

	if n := adminCount(t, pool); n != 1 {
		t.Fatalf("admin count = %d, want 1", n)
	}
}

// TestClaimSetupWeakPassword pins gate 3: the shared registration strength rule
// rejects a weak password with ErrValidation and creates nothing.
func TestClaimSetupWeakPassword(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	_, err := svc.ClaimSetup(ctx, "owner@example.com", "owner", "short", "t", "")
	if !errors.As(err, &ErrValidation{}) {
		t.Fatalf("weak-password err = %v, want ErrValidation", err)
	}
	if n := adminCount(t, pool); n != 0 {
		t.Fatalf("admin count = %d after a rejected weak-password claim, want 0", n)
	}
}

// TestClaimSetupCommonPassword pins the #513 hardening: the founding admin —
// the highest-value account on the instance — cannot be claimed with a common
// password even when it clears the length floor.
func TestClaimSetupCommonPassword(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	_, err := svc.ClaimSetup(ctx, "owner@example.com", "owner", "Password1234", "t", "")
	if !errors.As(err, &ErrValidation{}) {
		t.Fatalf("common-password err = %v, want ErrValidation", err)
	}
	if n := adminCount(t, pool); n != 0 {
		t.Fatalf("admin count = %d after a rejected common-password claim, want 0", n)
	}
}

// TestClaimSetupPasswordContainsIdentity pins the #513 identity-containment
// rule on the founding-admin path: a password built from the claimed
// username or email is rejected even though it is long and not on the
// common-password list.
func TestClaimSetupPasswordContainsIdentity(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	_, err := svc.ClaimSetup(ctx, "foundingadmin@example.com", "foundingadmin", "foundingadmin-2026!", "t", "")
	if !errors.As(err, &ErrValidation{}) {
		t.Fatalf("identity-containing password err = %v, want ErrValidation", err)
	}
	if n := adminCount(t, pool); n != 0 {
		t.Fatalf("admin count = %d after a rejected identity-containing claim, want 0", n)
	}
}

// TestClaimSetupRacesToExactlyOneAdmin is the headline concurrency guarantee:
// N goroutines racing ClaimSetup (each with a DISTINCT email, so ONLY the
// advisory-locked admin-exists gate — not a unique constraint — can serialize
// them) plus a concurrent env-bootstrap MUST produce exactly one admin. Exactly
// one claim succeeds; every other returns ErrSetupAlreadyComplete.
func TestClaimSetupRacesToExactlyOneAdmin(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	const claimers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, alreadyComplete, others int

	record := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSetupAlreadyComplete):
			alreadyComplete++
		default:
			others++
			t.Errorf("unexpected claim error: %v", err)
		}
	}

	start := make(chan struct{})
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := svc.ClaimSetup(ctx,
				fmt.Sprintf("owner%d@example.com", i),
				fmt.Sprintf("owner%d", i),
				"correct-horse-battery", "race", "")
			record(err)
		}(i)
	}

	// A concurrent env-bootstrap racing the claims against the SAME lock. Its
	// outcome (created/skipped) does not matter to the assertion — only that the
	// final admin count is exactly one.
	wg.Add(1)
	var bootErr error
	go func() {
		defer wg.Done()
		<-start
		_, bootErr = svc.EnsureBootstrapAdmin(ctx, "bootstrap@example.com", "bootstrap", "correct-horse-battery")
	}()

	close(start)
	wg.Wait()

	if bootErr != nil && !errors.Is(bootErr, ErrConflict) {
		t.Fatalf("bootstrap err = %v", bootErr)
	}
	if others != 0 {
		t.Fatalf("%d claims failed with an unexpected error", others)
	}
	// Exactly one provisioner won overall. The bootstrap may be the winner, in
	// which case zero claims succeed; or a claim won and bootstrap skipped. Either
	// way the DB must hold exactly one admin.
	if successes > 1 {
		t.Fatalf("%d claims succeeded, want at most 1 (advisory lock failed)", successes)
	}
	if n := adminCount(t, pool); n != 1 {
		t.Fatalf("FINAL ADMIN COUNT = %d, want exactly 1 — the claim/bootstrap race created duplicates", n)
	}
}

// TestClaimSetupBindsTokenToDevice pins the device binding: a claim carrying a
// device_key upserts user_devices and stamps the minted token's device_id, so
// the founding admin's token is visible to per-device revocation. A claim
// WITHOUT a device_key keeps device_id NULL (covered by the other tests, which
// pass "").
func TestClaimSetupBindsTokenToDevice(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	tok, err := svc.ClaimSetup(ctx, "owner@example.com", "owner", "correct-horse-battery", "test-agent", "dev-key-abc")
	if err != nil {
		t.Fatalf("ClaimSetup: %v", err)
	}

	var deviceKey string
	err = pool.QueryRow(ctx, `
		SELECT d.device_key
		FROM auth_tokens tk
		JOIN user_devices d ON d.id = tk.device_id
		WHERE tk.user_id::text = $1
	`, tok.User.ID).Scan(&deviceKey)
	if err != nil {
		t.Fatalf("claim token has no resolvable device binding: %v", err)
	}
	if deviceKey != "dev-key-abc" {
		t.Fatalf("bound device_key = %q, want dev-key-abc", deviceKey)
	}
}

func adminCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	return n
}
