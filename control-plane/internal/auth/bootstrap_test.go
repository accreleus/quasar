package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestEnsureBootstrapAdminRejectsCommonPassword pins #513: the founding
// admin minted from BOOTSTRAP_ADMIN_PASSWORD is subject to the same policy as
// any other new password — an operator typo like "password" (padded to clear
// the length floor) must not silently provision the instance's one
// unconditionally-trusted account.
func TestEnsureBootstrapAdminRejectsCommonPassword(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	_, err := svc.EnsureBootstrapAdmin(ctx, "root@quasar.local", "root", "Password1234")
	if !errors.As(err, &ErrValidation{}) {
		t.Fatalf("common bootstrap password: want ErrValidation, got %v", err)
	}
	assertAdminCount(t, pool, 0)
}

func TestEnsureBootstrapAdminCreatesOnFreshDB(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	res, err := svc.EnsureBootstrapAdmin(ctx, "root@quasar.local", "root", "bootstrap-pw-123")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res != BootstrapCreated {
		t.Fatalf("fresh DB: want BootstrapCreated, got %v", res)
	}

	// The bootstrapped admin can log in and authenticates as role=admin.
	tok, err := svc.Login(ctx, "root@quasar.local", "bootstrap-pw-123", "")
	if err != nil {
		t.Fatalf("login bootstrap admin: %v", err)
	}
	if tok.User.Role != RoleAdmin {
		t.Fatalf("bootstrap admin role: want %q, got %q", RoleAdmin, tok.User.Role)
	}

	// Idempotent: a second boot is a no-op and never mints a second admin.
	res, err = svc.EnsureBootstrapAdmin(ctx, "root@quasar.local", "root", "bootstrap-pw-123")
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if res != BootstrapSkipped {
		t.Fatalf("second boot: want BootstrapSkipped, got %v", res)
	}
	assertAdminCount(t, pool, 1)
}

func TestEnsureBootstrapAdminSkipsWhenAdminExists(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	if _, err := svc.EnsureBootstrapAdmin(ctx, "root@quasar.local", "root", "bootstrap-pw-123"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// With an admin already present, a differently-configured bootstrap must
	// NOT create a second admin (or any account at all).
	res, err := svc.EnsureBootstrapAdmin(ctx, "other@quasar.local", "other", "bootstrap-pw-456")
	if err != nil {
		t.Fatalf("bootstrap 2: %v", err)
	}
	if res != BootstrapSkipped {
		t.Fatalf("admin exists: want BootstrapSkipped, got %v", res)
	}
	assertAdminCount(t, pool, 1)
	if _, err := svc.Login(ctx, "other@quasar.local", "bootstrap-pw-456", ""); err == nil {
		t.Fatal("second bootstrap email must not have been provisioned")
	}
}

func TestEnsureBootstrapAdminPromotesExistingUser(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	// A normal registration is role=user (register semantics are unchanged).
	u, err := svc.Register(ctx, "ada@quasar.local", "ada", "user-pw-12345")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if u.Role != RoleUser {
		t.Fatalf("fresh registration must be role=%q, got %q", RoleUser, u.Role)
	}

	// Whoever registered the email holds a live token before the promotion.
	before, err := svc.Login(ctx, "ada@quasar.local", "user-pw-12345", "")
	if err != nil {
		t.Fatalf("login before promote: %v", err)
	}

	// Bootstrapping that same email (no admin yet) promotes the account.
	res, err := svc.EnsureBootstrapAdmin(ctx, "ada@quasar.local", "ada", "bootstrap-pw-12345")
	if err != nil {
		t.Fatalf("bootstrap promote: %v", err)
	}
	if res != BootstrapPromoted {
		t.Fatalf("existing user: want BootstrapPromoted, got %v", res)
	}

	// The registrant's credentials do not become an admin's: the old password and the
	// old token stop working, and only the configured password logs in.
	if _, err := svc.Login(ctx, "ada@quasar.local", "user-pw-12345", ""); err == nil {
		t.Fatal("the registrant's password must not log in to the promoted admin")
	}
	if _, _, err := svc.Authenticate(ctx, before.Plaintext); err == nil {
		t.Fatal("a token issued before the promotion must be revoked")
	}
	tok, err := svc.Login(ctx, "ada@quasar.local", "bootstrap-pw-12345", "")
	if err != nil {
		t.Fatalf("login after promote: %v", err)
	}
	if tok.User.Role != RoleAdmin {
		t.Fatalf("promoted role: want %q, got %q", RoleAdmin, tok.User.Role)
	}
	assertAdminCount(t, pool, 1)
}

func TestEnsureBootstrapAdminUnconfiguredIsNoop(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)

	res, err := svc.EnsureBootstrapAdmin(context.Background(), "", "", "")
	if err != nil {
		t.Fatalf("unconfigured bootstrap: %v", err)
	}
	if res != BootstrapSkipped {
		t.Fatalf("unconfigured: want BootstrapSkipped, got %v", res)
	}
	assertAdminCount(t, pool, 0)
}

func assertAdminCount(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&n); err != nil {
		t.Fatalf("count admins: %v", err)
	}
	if n != want {
		t.Fatalf("admin count: want %d, got %d", want, n)
	}
}

// #485 review: a login that verified the old password while a promotion was still
// uncommitted must not mint a token that outlives it. Token issue waits on the row
// and re-checks the hash, so it is refused once the promotion commits.
func TestTokenIssueWaitsForAnInFlightPasswordChange(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	u, err := svc.Register(ctx, "ada@quasar.local", "ada", "user-pw-12345")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	creds, err := svc.store.getCredentialsByEmail(ctx, "ada@quasar.local")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}

	promotion, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer promotion.Rollback(ctx) //nolint:errcheck
	if _, err := promotion.Exec(ctx, `UPDATE users SET role = 'admin', password_hash = 'operator' WHERE id = $1::uuid`, u.ID); err != nil {
		t.Fatalf("in-flight promotion: %v", err)
	}

	_, hash, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- svc.store.createToken(ctx, u.ID, creds.passwordHash, hash, time.Now().Add(time.Hour), "", "")
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%INSERT INTO auth_tokens%')`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("token issue did not wait for the uncommitted promotion (returned %v)", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("token issue never waited on the user row")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := promotion.Commit(ctx); err != nil {
		t.Fatalf("commit promotion: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("token issue after the promotion: got %v, want ErrInvalidCredentials", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_tokens WHERE user_id = $1::uuid`, u.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d token(s) issued against the replaced password", n)
	}
}

// A password change verified against a hash that has since been replaced does not
// overwrite the new one.
func TestPasswordChangeVerifiedAgainstAReplacedHashIsRefused(t *testing.T) {
	pool := testDB(t)
	svc := testService(t, pool)
	ctx := context.Background()

	u, err := svc.Register(ctx, "ada@quasar.local", "ada", "user-pw-12345")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	creds, err := svc.store.getCredentialsByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if _, err := svc.EnsureBootstrapAdmin(ctx, "ada@quasar.local", "ada", "bootstrap-pw-12345"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := svc.store.updatePasswordHash(ctx, u.ID, creds.passwordHash, "attacker"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("stale password change: got %v, want ErrInvalidCredentials", err)
	}
	if _, err := svc.Login(ctx, "ada@quasar.local", "bootstrap-pw-12345", ""); err != nil {
		t.Fatalf("the operator's password must still log in: %v", err)
	}
}
