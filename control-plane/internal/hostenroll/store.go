// Package hostenroll owns per-host enrollment tokens (#12/#96): admin-minted, hashed at
// rest, single-use by default, expiring, and optionally bound to one node_name.
//
// Why this exists rather than the single static ENROLLMENT_TOKEN: that value is shared by
// the whole fleet, cannot be rotated without a control-plane restart, and — because
// enrollment upserts on node_name — carries the authority to BECOME an already-enrolled
// host, not merely to add a new one. A token minted for one machine, good once, expiring,
// is the credential the operator thinks they are handing out.
//
// The redemption model is deliberately the same one `invites` uses (single
// UPDATE ... RETURNING under the row lock, no oracle in the error): one way to redeem a
// hashed code in this codebase, not two.
package hostenroll

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

// tokenBytes is the entropy behind a minted token. 32 bytes: this is a bearer credential
// that authorises joining a fleet, and it is copied by hand exactly once.
const tokenBytes = 32

// DefaultTTL bounds a mint that names no expiry. An enrollment token is used within
// minutes of being minted — the operator is standing at the machine — so a short default
// is the honest one, and it is the difference between a leaked token being a problem for
// an hour and forever.
const DefaultTTL = time.Hour

// ErrInvalidToken is returned for every unusable token: unknown, expired, exhausted,
// revoked, or bound to a different node_name. One error for all of them on purpose — a
// caller probing the agent websocket must not learn WHICH.
var ErrInvalidToken = errors.New("invalid enrollment token")

// DBTX is the subset of pgx used here, so Redeem can run inside the caller's transaction
// and be rolled back with it if enrollment fails after the consume.
type DBTX interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Enrollment is the persisted row. The token itself is never in here — only its hash, and
// TokenPrefix (the first bytes of that hash) so the admin UI can tell two rows apart.
type Enrollment struct {
	ID          string     `json:"id"`
	TokenPrefix string     `json:"token_prefix"`
	NodeName    *string    `json:"node_name"`
	MaxUses     int        `json:"max_uses"`
	UsedCount   int        `json:"used_count"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Note        *string    `json:"note"`
	CreatedAt   time.Time  `json:"created_at"`
	// Provenance, resolved at read time; nullable on the wire for the same reason
	// Invite.created_by_username is.
	CreatedByUserID   *string `json:"created_by_user_id"`
	CreatedByUsername *string `json:"created_by_username"`
}

// pendingSQL is "this token would still redeem right now". Must stay in step with
// Redeem's predicate — the admin list must show exactly the rows that would work.
const pendingSQL = `revoked_at IS NULL AND used_count < max_uses
	AND (expires_at IS NULL OR expires_at > now())`

// hashToken is sha256 hex, matching invites/auth_tokens. The token is high-entropy
// random, so a fast hash is correct: there is nothing to brute-force.
func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func generateToken() (plaintext, hash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate enrollment token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(b)
	return plaintext, hashToken(plaintext), nil
}

// Store is the admin-side CRUD. Redemption is a package function, not a method, so it can
// take the agent handler's transaction.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// MintParams: every field optional except CreatedBy. NodeName binds the token to one
// machine; MaxUses defaults to 1; ExpiresAt defaults to now()+DefaultTTL.
type MintParams struct {
	CreatedBy string
	NodeName  string
	MaxUses   int
	ExpiresAt *time.Time
	Note      string
}

// Mint inserts a token and returns the row plus the plaintext, which is shown to the admin
// exactly once and never persisted.
func (s *Store) Mint(ctx context.Context, p MintParams) (Enrollment, string, error) {
	maxUses := p.MaxUses
	if maxUses <= 0 {
		maxUses = 1
	}
	expires := p.ExpiresAt
	if expires == nil {
		t := time.Now().Add(DefaultTTL)
		expires = &t
	}
	var nodeName, note *string
	if p.NodeName != "" {
		nodeName = &p.NodeName
	}
	if p.Note != "" {
		note = &p.Note
	}

	plaintext, hash, err := generateToken()
	if err != nil {
		return Enrollment{}, "", err
	}

	var e Enrollment
	err = s.pool.QueryRow(ctx, `
		INSERT INTO host_enrollments (token_hash, created_by, node_name, max_uses, expires_at, note)
		VALUES ($1, $2::uuid, $3, $4, $5, $6)
		RETURNING id::text, left(token_hash, 8), node_name, max_uses, used_count,
		          expires_at, revoked_at, last_used_at, note, created_at
	`, hash, p.CreatedBy, nodeName, maxUses, expires, note).
		Scan(&e.ID, &e.TokenPrefix, &e.NodeName, &e.MaxUses, &e.UsedCount,
			&e.ExpiresAt, &e.RevokedAt, &e.LastUsedAt, &e.Note, &e.CreatedAt)
	if err != nil {
		return Enrollment{}, "", fmt.Errorf("insert host enrollment: %w", err)
	}
	return e, plaintext, nil
}

// List returns every minted token, newest first, with the minter resolved for
// provenance — the same instance-wide admin view /v1/admin/invites gives. pendingOnly
// narrows to tokens that would still redeem.
func (s *Store) List(ctx context.Context, pendingOnly bool) ([]Enrollment, error) {
	q := `SELECT e.id::text, left(e.token_hash, 8), e.node_name, e.max_uses, e.used_count,
	             e.expires_at, e.revoked_at, e.last_used_at, e.note, e.created_at,
	             e.created_by::text, u.username
	      FROM host_enrollments e
	      LEFT JOIN users u ON u.id = e.created_by`
	if pendingOnly {
		q += ` WHERE ` + pendingSQLQualified
	}
	q += ` ORDER BY e.created_at DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list host enrollments: %w", err)
	}
	defer rows.Close()

	out := []Enrollment{}
	for rows.Next() {
		var e Enrollment
		if err := rows.Scan(&e.ID, &e.TokenPrefix, &e.NodeName, &e.MaxUses, &e.UsedCount,
			&e.ExpiresAt, &e.RevokedAt, &e.LastUsedAt, &e.Note, &e.CreatedAt,
			&e.CreatedByUserID, &e.CreatedByUsername); err != nil {
			return nil, fmt.Errorf("scan host enrollment: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// pendingSQLQualified is pendingSQL with the alias the List join needs.
const pendingSQLQualified = `e.revoked_at IS NULL AND e.used_count < e.max_uses
	AND (e.expires_at IS NULL OR e.expires_at > now())`

// Revoke makes a token unusable. Idempotent: revoking twice keeps the first timestamp.
func (s *Store) Revoke(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE host_enrollments SET revoked_at = COALESCE(revoked_at, now())
		WHERE id = $1::uuid
	`, id)
	if err != nil {
		return fmt.Errorf("revoke host enrollment: %w", err)
	}
	return nil
}

// Redeem atomically consumes one use of the token matching plaintext, for nodeName.
//
// Runs against the caller's DBTX so the consume is atomic with the host upsert: if
// enrollment fails afterwards the use is given back with the rollback. The single
// UPDATE ... RETURNING is the race-safe consume — two concurrent redemptions of a
// single-use token cannot both match, because used_count < max_uses is evaluated under
// the row lock.
//
// A token bound to a node_name only redeems for that node_name; an unbound token redeems
// for any. Zero rows ⇒ ErrInvalidToken for every reason, with no oracle.
func Redeem(ctx context.Context, db DBTX, plaintext, nodeName string) error {
	if plaintext == "" {
		return ErrInvalidToken
	}
	var id string
	err := db.QueryRow(ctx, `
		UPDATE host_enrollments
		SET used_count = used_count + 1, last_used_at = now()
		WHERE token_hash = $1
		  AND (node_name IS NULL OR node_name = $2)
		  AND `+pendingSQL+`
		RETURNING id::text
	`, hashToken(plaintext), nodeName).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidToken
	}
	if err != nil {
		return fmt.Errorf("redeem host enrollment: %w", err)
	}
	return nil
}
