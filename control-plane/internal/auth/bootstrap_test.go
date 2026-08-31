package auth

import (
	"context"
	"errors"
	"testing"

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

	// Bootstrapping that same email (no admin yet) promotes the account.
	res, err := svc.EnsureBootstrapAdmin(ctx, "ada@quasar.local", "ada", "ignored-pw-12345")
	if err != nil {
		t.Fatalf("bootstrap promote: %v", err)
	}
	if res != BootstrapPromoted {
		t.Fatalf("existing user: want BootstrapPromoted, got %v", res)
	}

	// Promotion does not reset the password — the original still logs in.
	tok, err := svc.Login(ctx, "ada@quasar.local", "user-pw-12345", "")
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
