package invites

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
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
	if _, err := pool.Exec(ctx, `TRUNCATE invites, users CASCADE`); err != nil {
		pool.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func seedAdmin(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ('admin@x.io','admin','x','admin') RETURNING id::text`).Scan(&id); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return id
}

func TestMintStoresHashedNotPlaintext(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	inv, code, err := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if code == "" {
		t.Fatal("empty plaintext code")
	}
	// The plaintext must NOT be stored — the DB holds only its hash.
	var stored string
	if err := pool.QueryRow(ctx, `SELECT code_hash FROM invites WHERE id = $1::uuid`, inv.ID).Scan(&stored); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if stored == code {
		t.Fatal("plaintext code stored at rest — must be hashed")
	}
	if stored != hashCode(code) {
		t.Fatal("stored hash does not match sha256(code)")
	}
	if inv.CodePrefix != stored[:8] {
		t.Fatalf("code_prefix mismatch: %q vs %q", inv.CodePrefix, stored[:8])
	}
}

func TestRedeemHappyThenExhausted(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	_, code, err := s.Mint(ctx, MintParams{CreatedBy: admin, Role: "admin"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	role, err := Redeem(ctx, pool, code)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if role != "admin" {
		t.Fatalf("role: got %q want admin (rides the invite)", role)
	}
	// Single-use: the second redemption of the same code is invalid.
	if _, err := Redeem(ctx, pool, code); err != ErrInvalidInvite {
		t.Fatalf("second redeem: got %v want ErrInvalidInvite", err)
	}
}

func TestRedeemBoundedMultiUse(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	_, code, _ := s.Mint(ctx, MintParams{CreatedBy: admin, MaxUses: 2})
	for i := 0; i < 2; i++ {
		if _, err := Redeem(ctx, pool, code); err != nil {
			t.Fatalf("redeem %d: %v", i, err)
		}
	}
	if _, err := Redeem(ctx, pool, code); err != ErrInvalidInvite {
		t.Fatalf("third redeem: got %v want ErrInvalidInvite", err)
	}
}

func TestRedeemExpiredRevokedUnknownAllInvalid(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	past := time.Now().Add(-time.Hour)
	_, expCode, _ := s.Mint(ctx, MintParams{CreatedBy: admin, ExpiresAt: &past})
	if _, err := Redeem(ctx, pool, expCode); err != ErrInvalidInvite {
		t.Fatalf("expired: got %v want ErrInvalidInvite", err)
	}

	inv, revCode, _ := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err := s.Revoke(ctx, inv.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := Redeem(ctx, pool, revCode); err != ErrInvalidInvite {
		t.Fatalf("revoked: got %v want ErrInvalidInvite", err)
	}

	if _, err := Redeem(ctx, pool, "totally-made-up-code"); err != ErrInvalidInvite {
		t.Fatalf("unknown: got %v want ErrInvalidInvite", err)
	}
	if _, err := Redeem(ctx, pool, ""); err != ErrInvalidInvite {
		t.Fatalf("empty: got %v want ErrInvalidInvite", err)
	}
}

// TestRedeemRollsBackWithTx proves the consume is atomic with its transaction: a redeem
// inside a rolled-back tx does NOT burn the invite (decision D5).
func TestRedeemRollsBackWithTx(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	inv, code, _ := s.Mint(ctx, MintParams{CreatedBy: admin})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := Redeem(ctx, tx, code); err != nil {
		t.Fatalf("redeem in tx: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var used int
	if err := pool.QueryRow(ctx, `SELECT used_count FROM invites WHERE id = $1::uuid`, inv.ID).Scan(&used); err != nil {
		t.Fatalf("read used_count: %v", err)
	}
	if used != 0 {
		t.Fatalf("used_count after rollback: got %d want 0 (invite must not be burned)", used)
	}
	// And it remains redeemable afterwards.
	if _, err := Redeem(ctx, pool, code); err != nil {
		t.Fatalf("post-rollback redeem: %v", err)
	}
}

func TestListNeverLeaksPlaintext(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()

	_, code, _ := s.Mint(ctx, MintParams{CreatedBy: admin, Note: "for Bob"})
	list, err := s.List(ctx, StateAll)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("list len: got %d want 1", len(list))
	}
	if list[0].CodePrefix == code || len(list[0].CodePrefix) != 8 {
		t.Fatalf("list must expose only an 8-char prefix, not the code: %q", list[0].CodePrefix)
	}
	if list[0].Note == nil || *list[0].Note != "for Bob" {
		t.Fatalf("note not round-tripped: %v", list[0].Note)
	}
}

func TestRevokeIdempotent(t *testing.T) {
	pool := testDB(t)
	admin := seedAdmin(t, pool)
	s := NewStore(pool)
	ctx := context.Background()
	inv, _, _ := s.Mint(ctx, MintParams{CreatedBy: admin})
	if err := s.Revoke(ctx, inv.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// A second revoke is a no-op, not an error.
	if err := s.Revoke(ctx, inv.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}
