// Package invites implements the invite data model (LP-SEC-01 §A.1): admin-minted,
// single-or-bounded-multi-use registration codes stored HASHED (sha256 hex, same custody
// model as auth_tokens). Plaintext is shown to the admin exactly once at mint and never
// stored. Redemption is atomic single-use and runs inside the caller's transaction so it
// is consistent with account creation (SEC-03 / decision D5).
package invites

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// codeBytes is the entropy of an opaque invite code. 32 bytes = 256 bits (>= the
// contract's 128-bit floor), matching the bearer-token generator.
const codeBytes = 32

// ErrInvalidInvite is the single non-enumerating redemption failure (decision D2): it
// covers missing / unknown / expired / exhausted / revoked, indistinguishably.
var ErrInvalidInvite = errors.New("invalid invite")

// DBTX is the subset of pgx used by Redeem so it can run against either the pool or an
// open transaction (SEC-03 redeems inside the register tx).
type DBTX interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is the invites data-access layer over the pgx pool.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Invite is a minted invite row. The plaintext code is NEVER a field here — it exists
// only in the Mint return value, exactly once.
type Invite struct {
	ID         string     `json:"id"`
	CodePrefix string     `json:"code_prefix"` // first 8 hex of code_hash — a stable non-secret handle
	Role       string     `json:"role"`
	MaxUses    int        `json:"max_uses"`
	UsedCount  int        `json:"used_count"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Note       *string    `json:"note"`
	CreatedAt  time.Time  `json:"created_at"`
	// Provenance (UI v3 amendment). The column (invites.created_by, migration
	// 0020) has always been written; it was never served. Nullable on the wire
	// defensively — the column is NOT NULL and cascades, so an invite never
	// outlives its minter, which is a security property: deleting an admin
	// invalidates the codes they minted.
	CreatedByUserID *string `json:"created_by_user_id"`
	// CreatedByUsername is LEFT-joined at read time; null when that row is gone.
	CreatedByUsername *string `json:"created_by_username"`
}

// StateFilter narrows List. StateAll is the default and filters nothing.
type StateFilter string

const (
	StateAll     StateFilter = "all"
	StatePending StateFilter = "pending"
)

// ParseStateFilter maps the ?state= vocabulary; "" is `all`. An unrecognized
// value is refused rather than widened.
func ParseStateFilter(v string) (StateFilter, bool) {
	switch StateFilter(v) {
	case "":
		return StateAll, true
	case StateAll, StatePending:
		return StateFilter(v), true
	}
	return "", false
}

// pendingSQL is "this code would still redeem right now": the three exclusions
// are one operator-facing fact, and the client cannot compute the third for rows
// it has not loaded. Must stay in step with Redeem's predicate.
const pendingSQL = `revoked_at IS NULL AND used_count < max_uses
	AND (expires_at IS NULL OR expires_at > now())`

// hashCode returns the sha256 hex digest used as the invites lookup key. An invite code
// is high-entropy random, so a fast hash (not a KDF) is correct — nothing to brute-force.
func hashCode(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// generateCode returns a new opaque code (URL-safe base64, no padding) and its sha256
// hex digest. Only the digest is persisted; the plaintext is returned once.
func generateCode() (plaintext, hash string, err error) {
	b := make([]byte, codeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate invite code: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b)
	return plaintext, hashCode(plaintext), nil
}

// MintParams carries the (all-optional) inputs for a mint. role defaults 'user';
// maxUses defaults 1. createdBy is the acting admin's user id (bearer identity).
type MintParams struct {
	CreatedBy string
	Role      string // "user" | "admin"; "" -> "user"
	MaxUses   int    // <=0 -> 1
	ExpiresAt *time.Time
	Note      string
}

// Mint inserts a new invite and returns the persisted row plus the plaintext code
// (shown to the admin exactly once; only its hash is stored).
func (s *Store) Mint(ctx context.Context, p MintParams) (Invite, string, error) {
	role := p.Role
	if role == "" {
		role = "user"
	}
	if role != "user" && role != "admin" {
		return Invite{}, "", fmt.Errorf("invalid invite role %q", role)
	}
	maxUses := p.MaxUses
	if maxUses <= 0 {
		maxUses = 1
	}
	var note *string
	if p.Note != "" {
		note = &p.Note
	}

	plaintext, hash, err := generateCode()
	if err != nil {
		return Invite{}, "", err
	}

	inv := Invite{CodePrefix: hash[:8], Role: role, MaxUses: maxUses, UsedCount: 0, Note: note}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO invites (code_hash, created_by, role, max_uses, expires_at, note)
		VALUES ($1, $2::uuid, $3, $4, $5, $6)
		RETURNING id::text, role, max_uses, used_count, expires_at, revoked_at, note, created_at
	`, hash, p.CreatedBy, role, maxUses, p.ExpiresAt, note).
		Scan(&inv.ID, &inv.Role, &inv.MaxUses, &inv.UsedCount, &inv.ExpiresAt, &inv.RevokedAt, &inv.Note, &inv.CreatedAt)
	if err != nil {
		return Invite{}, "", fmt.Errorf("insert invite: %w", err)
	}
	return inv, plaintext, nil
}

// List returns all minted invites, newest first (admin-global management surface). The
// plaintext code is never returned — only the non-secret code_prefix handle.
func (s *Store) List(ctx context.Context, filter StateFilter) ([]Invite, error) {
	// LEFT JOIN, not inner: an invite must survive a repaired or restored actor
	// row as "minted by someone we can no longer name", never vanish from the list.
	where := ""
	if filter == StatePending {
		where = "WHERE " + pendingSQL
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id::text, left(i.code_hash, 8), i.role, i.max_uses, i.used_count,
		       i.expires_at, i.revoked_at, i.note, i.created_at,
		       i.created_by::text, u.username
		FROM invites i
		LEFT JOIN users u ON u.id = i.created_by
		`+where+`
		ORDER BY i.created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query invites: %w", err)
	}
	defer rows.Close()

	out := make([]Invite, 0)
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.ID, &inv.CodePrefix, &inv.Role, &inv.MaxUses, &inv.UsedCount,
			&inv.ExpiresAt, &inv.RevokedAt, &inv.Note, &inv.CreatedAt,
			&inv.CreatedByUserID, &inv.CreatedByUsername); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// Revoke marks an invite revoked (revoked_at = now()). Idempotent: revoking an absent or
// already-revoked invite is not an error — the invite simply stops redeeming immediately.
func (s *Store) Revoke(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE invites SET revoked_at = now()
		WHERE id = $1::uuid AND revoked_at IS NULL
	`, id)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	return nil
}

// Redeem atomically consumes one use of the invite matching plaintextCode, returning the
// role the redeemed account must be created with. It runs against the supplied DBTX so
// the consume is atomic with account creation (SEC-03): if the surrounding transaction
// rolls back (e.g. a duplicate-email 409), the used_count bump is undone with it.
//
// The single UPDATE...RETURNING is the race-safe consume: two concurrent redemptions of a
// single-use code cannot both match (the used_count < max_uses predicate is evaluated
// under the row lock). Zero rows returned ⇒ ErrInvalidInvite for ALL of
// missing/unknown/expired/exhausted/revoked — no oracle (decision D2).
func Redeem(ctx context.Context, db DBTX, plaintextCode string) (role string, err error) {
	if plaintextCode == "" {
		return "", ErrInvalidInvite
	}
	// pendingSQL, not a hand copy: "this code still redeems" is ONE definition,
	// and ?state=pending must list exactly the invites this UPDATE would accept.
	err = db.QueryRow(ctx, `
		UPDATE invites
		SET used_count = used_count + 1
		WHERE code_hash = $1
		  AND `+pendingSQL+`
		RETURNING role
	`, hashCode(plaintextCode)).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrInvalidInvite
	}
	if err != nil {
		return "", fmt.Errorf("redeem invite: %w", err)
	}
	return role, nil
}
