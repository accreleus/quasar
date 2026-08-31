package auth_test

// External test package (auth_test) so it can import invites + settings — both of which
// import auth — without a package cycle. This exercises the real registration gate
// end-to-end (LP-SEC-01 SEC-03), including the D5 atomicity guarantee.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/invites"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/internal/settings"
	"github.com/accreleus/quasar/control-plane/migrations"
)

// gate mirrors the production registrationGate (cmd/quasar-control/app.go): settings for
// the mode, invites.Redeem for atomic consumption inside the register tx.
type gate struct{ s *settings.Store }

func (g gate) Mode(ctx context.Context) (string, error) { return g.s.RegistrationMode(ctx) }
func (g gate) RedeemInvite(ctx context.Context, tx pgx.Tx, code string) (string, bool, error) {
	role, err := invites.Redeem(ctx, tx, code)
	if errors.Is(err, invites.ErrInvalidInvite) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

func regTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := migrate.Run(migrations.FS, dbURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE users, invites, instance_settings, auth_tokens CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// harness wires a gated auth.Service + an invites store + a seeded admin (for created_by),
// then sets registration_mode.
func harness(t *testing.T, mode string) (*auth.Service, *invites.Store, *pgxpool.Pool, string) {
	t.Helper()
	pool := regTestDB(t)
	ss := settings.NewStore(pool)
	ctx := context.Background()
	if err := ss.Seed(ctx, settings.RegistrationClosed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var admin string
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('root@x.io','root','x','admin') RETURNING id::text`).Scan(&admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := ss.UpdateRegistrationMode(ctx, mode, admin); err != nil {
		t.Fatalf("set mode: %v", err)
	}
	svc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour, auth.WithRegistration(gate{s: ss}))
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	return svc, invites.NewStore(pool), pool, admin
}

func TestRegisterClosedRefused(t *testing.T) {
	svc, _, _, _ := harness(t, settings.RegistrationClosed)
	_, err := svc.RegisterWithInvite(context.Background(), "a@b.io", "ada", "quasar-fixture-pw-08", "anything")
	if !errors.Is(err, auth.ErrRegistrationClosed) {
		t.Fatalf("closed register: got %v want ErrRegistrationClosed", err)
	}
}

func TestRegisterOpenIgnoresInvite(t *testing.T) {
	svc, _, _, _ := harness(t, settings.RegistrationOpen)
	u, err := svc.RegisterWithInvite(context.Background(), "open@b.io", "opener", "quasar-fixture-pw-08", "")
	if err != nil {
		t.Fatalf("open register: %v", err)
	}
	if u.Role != "user" {
		t.Fatalf("open register role: got %q want user", u.Role)
	}
}

func TestRegisterInviteOnlyRequiresValidInvite(t *testing.T) {
	svc, _, _, _ := harness(t, settings.RegistrationInviteOnly)
	ctx := context.Background()
	// No code → invalid.
	if _, err := svc.RegisterWithInvite(ctx, "x@b.io", "x", "quasar-fixture-pw-08", ""); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Fatalf("no code: got %v want ErrInvalidInvite", err)
	}
	// Bad code → invalid (same generic error — no oracle).
	if _, err := svc.RegisterWithInvite(ctx, "x@b.io", "x", "quasar-fixture-pw-08", "bogus"); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Fatalf("bad code: got %v want ErrInvalidInvite", err)
	}
}

func TestRegisterRoleRidesInvite(t *testing.T) {
	svc, inv, _, admin := harness(t, settings.RegistrationInviteOnly)
	ctx := context.Background()
	_, code, err := inv.Mint(ctx, invites.MintParams{CreatedBy: admin, Role: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	u, err := svc.RegisterWithInvite(ctx, "new@b.io", "newadmin", "quasar-fixture-pw-08", code)
	if err != nil {
		t.Fatalf("register with admin invite: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("role must ride the invite: got %q want admin", u.Role)
	}
}

// TestRegisterConflictDoesNotBurnInvite is the D5 guarantee: a duplicate email 409 rolls
// the tx back, leaving the single-use invite unspent and reusable.
func TestRegisterConflictDoesNotBurnInvite(t *testing.T) {
	svc, inv, pool, admin := harness(t, settings.RegistrationInviteOnly)
	ctx := context.Background()

	// A user already owns this email.
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('dupe@b.io','dupe','x','user')`); err != nil {
		t.Fatalf("seed dupe: %v", err)
	}

	invRow, code, err := inv.Mint(ctx, invites.MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	// Register with the same email → 409 conflict.
	if _, err := svc.RegisterWithInvite(ctx, "dupe@b.io", "dupe2", "quasar-fixture-pw-08", code); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("dup register: got %v want ErrConflict", err)
	}

	// The invite must NOT be consumed.
	var used int
	if err := pool.QueryRow(ctx, `SELECT used_count FROM invites WHERE id = $1::uuid`, invRow.ID).Scan(&used); err != nil {
		t.Fatalf("read used_count: %v", err)
	}
	if used != 0 {
		t.Fatalf("invite burned by a 409: used_count=%d want 0", used)
	}

	// And it still works for a fresh account.
	if _, err := svc.RegisterWithInvite(ctx, "fresh@b.io", "fresh", "quasar-fixture-pw-08", code); err != nil {
		t.Fatalf("reuse after conflict: %v", err)
	}
}
