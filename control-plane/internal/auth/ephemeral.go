package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// MintEphemeral creates a throwaway identity (a real users row with
// ephemeral_expires_at) and a real bearer token clamped to it — a token can
// never outlive its user. The internal/devauth (#399) seam lives here because
// password hashing and token issuance must have exactly one implementation;
// duplicating either is how a weaker credential enters the system. Enforcement
// is unchanged: RequireAuth → RequireAdmin sees an ordinary user, and the
// random password is never returned or used.
func (s *Service) MintEphemeral(ctx context.Context, role string, ttl time.Duration) (Token, error) {
	if role != RoleUser && role != RoleAdmin {
		return Token{}, ErrValidation{Msg: fmt.Sprintf("role must be %q or %q", RoleUser, RoleAdmin)}
	}
	if ttl <= 0 {
		return Token{}, ErrValidation{Msg: "ttl must be positive"}
	}

	id, err := randomUUIDv4()
	if err != nil {
		return Token{}, err
	}
	suffix, err := randomHex(4)
	if err != nil {
		return Token{}, err
	}
	email := "agent-" + id + "@dev.invalid"
	username := "agent-" + id[:8] + "-" + suffix

	// A random password nobody holds, hashed by the one hashing path: the row
	// stays shaped like any account (password_hash NOT NULL) and no login can
	// ever guess it.
	pw, err := randomPassword()
	if err != nil {
		return Token{}, err
	}
	hash, err := HashPassword(pw, s.params)
	if err != nil {
		return Token{}, fmt.Errorf("hash ephemeral password: %w", err)
	}

	expiresAt := time.Now().Add(ttl)
	user, err := s.store.createEphemeralUser(ctx, email, username, hash, role, expiresAt)
	if err != nil {
		return Token{}, err
	}

	plaintext, tokenHash, err := generateToken()
	if err != nil {
		return Token{}, err
	}
	if err := s.store.createToken(ctx, user.ID, tokenHash, expiresAt, "quasar-dev-agent", ""); err != nil {
		return Token{}, err
	}
	return Token{Plaintext: plaintext, ExpiresAt: expiresAt, User: user}, nil
}

// ReapReport is one sweep's outcome.
type ReapReport struct {
	// Deleted is how many identities went this sweep.
	Deleted int
	// InSession is how many were expired but still holding a non-terminal
	// session. Those are LEFT ALONE and retried next sweep — deleting them would
	// orphan a live session on a node agent (see ReapEphemeral).
	InSession int
	// Failed is how many hit some other error. The sweep continues past each one.
	Failed int
	// HostsNudged is how many distinct hosts were asked to reap the homes those
	// identities left behind (#92).
	HostsNudged int
}

// ReapEphemeral deletes expired throwaway identities one row at a time through
// store.deleteUser — the DELETE /v1/users/{id} path — so its guards hold here:
// a user with a non-terminal session is refused (migration 0007's cascade is
// only safe because active sessions are refused first; a bulk DELETE would
// orphan a running session and its GPU reservation), user_homes are tombstoned
// so the janitor reclaims the store (bulk DELETE leaks the volume forever),
// and the last admin is refused. One row's failure never aborts the sweep.
//
// Each host that held one of those homes is then nudged to run its home-GC pass
// (#92): the home is orphaned the moment its owner's row goes, so waiting out a
// grace window or a 6h tick only keeps multi-GB payloads on disk.
func (s *Service) ReapEphemeral(ctx context.Context) (ReapReport, error) {
	ids, err := s.store.expiredEphemeralUserIDs(ctx)
	if err != nil {
		return ReapReport{}, err
	}

	var rep ReapReport
	var errs []error
	seen := map[string]bool{}
	var hosts []string
	for _, id := range ids {
		switch hostIDs, err := s.store.deleteUser(ctx, id); {
		case err == nil:
			rep.Deleted++
			for _, h := range hostIDs {
				if !seen[h] {
					seen[h] = true
					hosts = append(hosts, h)
				}
			}
		case errors.Is(err, ErrUserHasActiveSessions):
			// Expected and correct: try again next sweep.
			rep.InSession++
		case errors.Is(err, ErrUserNotFound):
			// Raced with another control-plane instance's sweep. Not a failure.
		default:
			rep.Failed++
			errs = append(errs, fmt.Errorf("reap ephemeral user %s: %w", id, err))
		}
	}
	// One nudge per host for the whole batch, not per identity: a harness that
	// mints an identity per login expires them in clumps, and Enqueue coalesces
	// onto the one open run anyway.
	rep.HostsNudged = len(hosts)
	s.reapHomesOn(ctx, hosts)
	return rep, errors.Join(errs...)
}

// createEphemeralUser inserts a throwaway account with an explicit role and expiry.
// Deliberately separate from createUser/createUserTx: those two are the production
// signup paths and must never grow an "expires" parameter that a bug could set.
func (s *store) createEphemeralUser(ctx context.Context, email, username, passwordHash, role string, expiresAt time.Time) (User, error) {
	u := User{Email: email, Username: username, Role: role}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role, ephemeral_expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, role, created_at
	`, email, username, passwordHash, role, expiresAt).Scan(&u.ID, &u.Role, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("insert ephemeral user: %w", err)
	}
	return u, nil
}

// reapBatch bounds one sweep. The reaper runs every minute, so a backlog drains
// quickly; the bound just stops a pathological backlog from holding one long
// transaction-heavy loop.
const reapBatch = 500

// expiredEphemeralUserIDs lists the throwaway identities whose TTL has lapsed,
// oldest first. The WHERE clause is exactly the partial index's predicate
// (migration 0051), so it never scans real accounts.
func (s *store) expiredEphemeralUserIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM users
		WHERE ephemeral_expires_at IS NOT NULL
		  AND ephemeral_expires_at < now()
		ORDER BY ephemeral_expires_at
		LIMIT $1
	`, reapBatch)
	if err != nil {
		return nil, fmt.Errorf("list expired ephemeral users: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired ephemeral user: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// randomUUIDv4 returns a RFC 4122 v4 UUID string. Hand-rolled rather than adding a
// dependency for one call site — the control plane has no uuid module today.
func randomUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random hex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// randomPassword returns 32 bytes of entropy as a password the caller never sees.
func randomPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate ephemeral password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
