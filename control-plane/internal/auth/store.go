package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// pgUniqueViolation is the SQLSTATE for a unique-constraint violation.
const pgUniqueViolation = "23505"

// ErrConflict is returned when a unique constraint (email/username) is violated.
var ErrConflict = errors.New("email or username already in use")

// ErrUserNotFound is returned by getUserByEmail when no row matches.
var ErrUserNotFound = errors.New("user not found")

// ErrLastAdmin is returned when demoting the only remaining admin would leave
// no admin in the system. The caller should surface this as a 409.
var ErrLastAdmin = errors.New("cannot demote the last admin")

// ErrUserHasActiveSessions is returned when deleting a user who still has a
// non-terminal session. Stop the session first; only terminal history cascades.
var ErrUserHasActiveSessions = errors.New("user has active sessions")

// ErrSetupAlreadyComplete: the first-run claim found an admin already exists
// (409 setup_already_complete). Claim-side twin of ensureBootstrapAdmin's
// BootstrapSkipped — both decide under the same advisory lock, so the two
// provisioning paths can never both create an admin (wizard spec, gate 1).
var ErrSetupAlreadyComplete = errors.New("setup already complete")

// store is the auth data-access layer over the pgx pool.
type store struct {
	pool *pgxpool.Pool
}

// User is the domain view of an account (no password hash).
type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// createUser inserts a new account and returns it. Returns ErrConflict if the
// email or username is already taken.
func (s *store) createUser(ctx context.Context, email, username, passwordHash string) (User, error) {
	u := User{Email: email, Username: username, Role: "user"}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id::text, role, created_at
	`, email, username, passwordHash).Scan(&u.ID, &u.Role, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("insert user: %w", err)
	}
	return u, nil
}

// credentials is the minimal row login needs. email/username are populated
// only by getCredentialsByID (the CP-01 password-identity check, #513);
// getCredentialsByEmail leaves them zero.
type credentials struct {
	userID       string
	passwordHash string
	disabled     bool
	email        string
	username     string
}

// getCredentialsByEmail looks up a user by case-insensitive email. Returns
// ErrUserNotFound if absent (callers must treat this identically to a bad
// password to avoid user enumeration).
func (s *store) getCredentialsByEmail(ctx context.Context, email string) (credentials, error) {
	var c credentials
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, password_hash, (disabled_at IS NOT NULL)
		FROM users
		WHERE lower(email) = lower($1)
	`, email).Scan(&c.userID, &c.passwordHash, &c.disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentials{}, ErrUserNotFound
	}
	if err != nil {
		return credentials{}, fmt.Errorf("select credentials: %w", err)
	}
	return c, nil
}

// getCredentialsByID looks up a user by id. Returns ErrUserNotFound if absent.
// Used by self-service change-password (CP-01) to verify the current password.
func (s *store) getCredentialsByID(ctx context.Context, id string) (credentials, error) {
	var c credentials
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, password_hash, (disabled_at IS NOT NULL), email, username
		FROM users
		WHERE id::text = $1
	`, id).Scan(&c.userID, &c.passwordHash, &c.disabled, &c.email, &c.username)
	if errors.Is(err, pgx.ErrNoRows) {
		return credentials{}, ErrUserNotFound
	}
	if err != nil {
		return credentials{}, fmt.Errorf("select credentials by id: %w", err)
	}
	return c, nil
}

// updatePasswordHash rotates a user's stored password hash (CP-01). Existing
// bearer tokens are intentionally left intact — the session survives the change.
// Returns ErrUserNotFound if no row matches.
func (s *store) updatePasswordHash(ctx context.Context, id, passwordHash string) error {
	ct, err := s.pool.Exec(ctx, `
		UPDATE users SET password_hash = $2, updated_at = now()
		WHERE id::text = $1
	`, id, passwordHash)
	if err != nil {
		return fmt.Errorf("update password hash: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// createToken stores the hash of a freshly minted bearer token. deviceID, when
// non-empty, is the user_devices id this token is bound to (LP-SEC-01 §B.5) — the
// binding that makes per-device revocation real; empty ⇒ NULL (not device-revocable).
func (s *store) createToken(ctx context.Context, userID, tokenHash string, expiresAt time.Time, userAgent, deviceID string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO auth_tokens (user_id, token_hash, expires_at, user_agent, device_id)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')::uuid)
	`, userID, tokenHash, expiresAt, userAgent, deviceID)
	if err != nil {
		return fmt.Errorf("insert token: %w", err)
	}
	return nil
}

// upsertLoginDevice records the caller's device at login (LP-SEC-01 §B.5) and returns its
// id so the minted token can be bound to it. Inserts on first sight of (user_id,
// device_key); on a repeat it touches last_seen_at and refreshes user_agent. This is the
// minimal login-time upsert — the fuller capability probe still flows through
// POST /v1/me/devices separately. userID MUST be the authenticated user id.
func (s *store) upsertLoginDevice(ctx context.Context, userID, deviceKey, userAgent string) (string, error) {
	var ua *string
	if userAgent != "" {
		ua = &userAgent
	}
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO user_devices (user_id, device_key, user_agent)
		VALUES ($1::uuid, $2, $3)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET last_seen_at = now(),
		        user_agent   = COALESCE(EXCLUDED.user_agent, user_devices.user_agent)
		RETURNING id::text
	`, userID, deviceKey, ua).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("upsert login device: %w", err)
	}
	return id, nil
}

// begin opens a transaction on the pool. Used by the invite-redemption register path so
// the invite consume and the account insert commit atomically (decision D5).
func (s *store) begin(ctx context.Context) (pgx.Tx, error) { return s.pool.Begin(ctx) }

// createUserTx inserts a new account inside an existing transaction with an explicit role
// (the invite's role — LP-SEC-01: role rides the admin-minted invite, never the register
// wire). Returns ErrConflict on a duplicate email/username so the caller can roll the tx
// back (which un-consumes the invite).
func (s *store) createUserTx(ctx context.Context, tx pgx.Tx, email, username, passwordHash, role string) (User, error) {
	u := User{Email: email, Username: username, Role: role}
	err := tx.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ($1, $2, $3, $4)
		RETURNING id::text, role, created_at
	`, email, username, passwordHash, role).Scan(&u.ID, &u.Role, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, ErrConflict
		}
		return User{}, fmt.Errorf("insert user (invite): %w", err)
	}
	return u, nil
}

// lastUsedAtThrottle bounds how often authenticate touches last_used_at, which
// is advisory (hygiene UI, never an auth input). Throttling kills the
// per-request WAL write that was the #1 DB time consumer (#421: 13.95ms mean,
// ~70x the next-largest query).
const lastUsedAtThrottle = 60 * time.Second

// authenticate validates a token hash and returns the owning user plus the
// token's bound device id ("" when unbound — pre-0020 or no device_key). Every
// invalid case (missing, revoked, expired, user disabled) is ErrUserNotFound;
// the caller maps it to 401 without distinction. last_used_at is touched only
// when NULL or older than lastUsedAtThrottle (#421), so the common case is one
// round trip.
func (s *store) authenticate(ctx context.Context, tokenHash string) (User, string, error) {
	var u User
	var deviceID *string
	var lastUsedAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT u.id::text, u.email, u.username, u.role, u.created_at,
		       t.device_id::text, t.last_used_at
		FROM auth_tokens t
		JOIN users u ON t.user_id = u.id
		WHERE t.token_hash = $1
		  AND t.revoked_at IS NULL
		  AND t.expires_at > now()
		  AND u.disabled_at IS NULL
	`, tokenHash).Scan(&u.ID, &u.Email, &u.Username, &u.Role, &u.CreatedAt, &deviceID, &lastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrUserNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("authenticate token: %w", err)
	}

	if lastUsedAt == nil || time.Since(*lastUsedAt) >= lastUsedAtThrottle {
		if _, err := s.pool.Exec(ctx, `
			UPDATE auth_tokens SET last_used_at = now() WHERE token_hash = $1
		`, tokenHash); err != nil {
			return User{}, "", fmt.Errorf("touch token last_used_at: %w", err)
		}
	}

	dev := ""
	if deviceID != nil {
		dev = *deviceID
	}
	return u, dev, nil
}

// bootstrapAdvisoryLock is a fixed key for pg_advisory_xact_lock so that two
// control-plane instances booting against the same fresh DB serialize their
// first-admin decision instead of both creating one (the control plane is
// horizontally scalable — schema.md).
const bootstrapAdvisoryLock int64 = 0x5175_61_5341 // "Qua" + 'SA' mnemonic

// adminDemoteAdvisoryLock serializes admin→user demotions so the last-admin
// guard's check+update is atomic across concurrent requests (#148).
const adminDemoteAdvisoryLock int64 = 0x5175_61_4445 // "Qua" + 'DE' mnemonic

// ensureBootstrapAdmin provisions the operator-configured first admin when no
// admin exists yet. It runs inside one transaction holding an advisory lock so
// the decision is race-free across concurrently-booting instances:
//   - if any admin already exists → BootstrapSkipped (no-op);
//   - else if an account with this email exists → promote it (BootstrapPromoted);
//   - else insert a fresh admin account (BootstrapCreated).
//
// The password hash is used only on the insert path; promotion never resets a
// password. A username collision on insert surfaces as ErrConflict.
func (s *store) ensureBootstrapAdmin(ctx context.Context, email, username, passwordHash string) (BootstrapResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BootstrapSkipped, fmt.Errorf("begin bootstrap tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapAdvisoryLock); err != nil {
		return BootstrapSkipped, fmt.Errorf("bootstrap lock: %w", err)
	}

	var adminExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&adminExists); err != nil {
		return BootstrapSkipped, fmt.Errorf("check admin exists: %w", err)
	}
	if adminExists {
		if err := tx.Commit(ctx); err != nil {
			return BootstrapSkipped, fmt.Errorf("commit bootstrap: %w", err)
		}
		return BootstrapSkipped, nil
	}

	// No admin yet: promote a matching account if one already registered.
	ct, err := tx.Exec(ctx, `
		UPDATE users SET role = 'admin', updated_at = now()
		WHERE lower(email) = lower($1)
	`, email)
	if err != nil {
		return BootstrapSkipped, fmt.Errorf("promote bootstrap admin: %w", err)
	}
	if ct.RowsAffected() > 0 {
		if err := tx.Commit(ctx); err != nil {
			return BootstrapSkipped, fmt.Errorf("commit bootstrap: %w", err)
		}
		return BootstrapPromoted, nil
	}

	// Otherwise create the admin account outright.
	if _, err := tx.Exec(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ($1, $2, $3, 'admin')
	`, email, username, passwordHash); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return BootstrapSkipped, ErrConflict
		}
		return BootstrapSkipped, fmt.Errorf("insert bootstrap admin: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrapSkipped, fmt.Errorf("commit bootstrap: %w", err)
	}
	return BootstrapCreated, nil
}

// anyAdminExists reports whether any account with role='admin' exists. It backs
// GET /v1/setup/status's admin_exists boolean — a plain unauthenticated read, no
// lock needed (the value is advisory routing information, and the authoritative
// race-free check happens inside claimFirstAdmin under the advisory lock).
func (s *store) anyAdminExists(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check admin exists: %w", err)
	}
	return exists, nil
}

// claimFirstAdmin creates the first admin and mints its token atomically under
// the same bootstrapAdvisoryLock ensureBootstrapAdmin takes: exactly one caller
// can observe "no admin" and insert; every other gets ErrSetupAlreadyComplete
// (wizard spec, gate 1). The token lives in the same tx as the user, so a
// committed claim is "admin + working session" and a rollback leaves neither.
// passwordHash is computed by the caller outside the tx so the expensive KDF
// never holds the lock. deviceKey, when non-empty, device-binds the token in
// the same tx (LP-SEC-01 §B.5) — otherwise the highest-value token on the
// instance would be unkillable if stolen; empty means device_id NULL.
func (s *store) claimFirstAdmin(ctx context.Context, email, username, passwordHash string, tokenTTL time.Duration, userAgent, deviceKey string) (Token, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("begin claim tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapAdvisoryLock); err != nil {
		return Token{}, fmt.Errorf("claim lock: %w", err)
	}

	var adminExists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE role = 'admin')`).Scan(&adminExists); err != nil {
		return Token{}, fmt.Errorf("check admin exists: %w", err)
	}
	if adminExists {
		return Token{}, ErrSetupAlreadyComplete
	}

	u := User{Email: email, Username: username}
	err = tx.QueryRow(ctx, `
		INSERT INTO users (email, username, password_hash, role)
		VALUES ($1, $2, $3, 'admin')
		RETURNING id::text, email, username, role, created_at
	`, email, username, passwordHash).Scan(&u.ID, &u.Email, &u.Username, &u.Role, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return Token{}, ErrConflict
		}
		return Token{}, fmt.Errorf("insert first admin: %w", err)
	}

	// Bind the token to the caller's device when one was declared — same
	// owner-scoped upsert LoginWithDevice performs, but inside this tx so the
	// device row, the admin, and the token commit (or roll back) together.
	deviceID := ""
	if deviceKey != "" {
		err = tx.QueryRow(ctx, `
			INSERT INTO user_devices (user_id, device_key, user_agent)
			VALUES ($1::uuid, $2, NULLIF($3, ''))
			ON CONFLICT (user_id, device_key) DO UPDATE
			    SET last_seen_at = now(),
			        user_agent   = COALESCE(EXCLUDED.user_agent, user_devices.user_agent)
			RETURNING id::text
		`, u.ID, deviceKey, userAgent).Scan(&deviceID)
		if err != nil {
			return Token{}, fmt.Errorf("upsert claim device: %w", err)
		}
	}

	plaintext, hash, err := generateToken()
	if err != nil {
		return Token{}, err
	}
	expiresAt := time.Now().Add(tokenTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO auth_tokens (user_id, token_hash, expires_at, user_agent, device_id)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')::uuid)
	`, u.ID, hash, expiresAt, userAgent, deviceID); err != nil {
		return Token{}, fmt.Errorf("insert claim token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Token{}, fmt.Errorf("commit claim: %w", err)
	}
	return Token{Plaintext: plaintext, ExpiresAt: expiresAt, User: u}, nil
}

// revokeToken marks a token revoked. Idempotent: revoking an absent or
// already-revoked token is not an error (returns nil).
func (s *store) revokeToken(ctx context.Context, tokenHash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_tokens SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
	`, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke token: %w", err)
	}
	return nil
}

// revokeAllUserTokens revokes every active token for a user — used on password
// change (CP-01) to force re-authentication on all devices. Idempotent.
func (s *store) revokeAllUserTokens(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE auth_tokens SET revoked_at = now()
		WHERE user_id::text = $1 AND revoked_at IS NULL
	`, userID)
	if err != nil {
		return fmt.Errorf("revoke all user tokens: %w", err)
	}
	return nil
}

// AdminUser is the full user view exposed only to admin callers.
type AdminUser struct {
	ID                    string     `json:"id"`
	Email                 string     `json:"email"`
	Username              string     `json:"username"`
	Role                  string     `json:"role"`
	DisabledAt            *time.Time `json:"disabled_at"`
	MaxConcurrentSessions int32      `json:"max_concurrent_sessions"`
	CreatedAt             time.Time  `json:"created_at"`
	// LastSeenAt is max(user_devices.last_seen_at); nil with no devices. DEVICE
	// activity (a login or a capability probe), not "last request" — there is no
	// per-request write on the hot path and adding one for a dashboard would be
	// the wrong trade.
	LastSeenAt *time.Time `json:"last_seen_at"`
	// ActiveSessionCount is the user's non-terminal sessions. One state wider
	// than the quota gate (which excludes `stopping`, so a relaunch is not blocked
	// by a teardown): read it as "sessions open", not "quota consumed".
	// semantics: control-api.md §UI v3 console
	ActiveSessionCount int32 `json:"active_session_count"`
}

// activeSessionPredicateSQL must stay IDENTICAL to migration 0070's
// sessions_user_active_idx predicate. Postgres uses a partial index only when it
// can prove the query's WHERE implies the index's; an identical predicate is what
// guarantees that, and is what earns the index-ONLY scan (verified by EXPLAIN).
// Widen this list without widening the index and the count silently degrades to a
// scan of all session history — slower forever, failing nothing.
const activeSessionPredicateSQL = `state IN ('pending','assigned','starting','running','stopping')`

// adminUserSelectSQL is the single projection behind every AdminUser read. Both
// derived fields are correlated subqueries, computed with the row: a users page
// must never cost one extra query per user. Each reads one index — 0004's
// user_devices_user_id_idx and 0070's partial sessions_user_active_idx.
const adminUserSelectSQL = `
	SELECT u.id::text, u.email, u.username, u.role, u.disabled_at,
	       u.max_concurrent_sessions, u.created_at,
	       (SELECT max(d.last_seen_at) FROM user_devices d WHERE d.user_id = u.id),
	       (SELECT count(*) FROM sessions
	         WHERE user_id = u.id
	           AND ` + activeSessionPredicateSQL + `)::int
	FROM users u`

// scanAdminUser reads one adminUserSelectSQL row.
func scanAdminUser(row pgx.Row) (AdminUser, error) {
	var u AdminUser
	err := row.Scan(&u.ID, &u.Email, &u.Username, &u.Role, &u.DisabledAt,
		&u.MaxConcurrentSessions, &u.CreatedAt, &u.LastSeenAt, &u.ActiveSessionCount)
	return u, err
}

// readAdminUser fetches one user in the full admin shape.
func (s *store) readAdminUser(ctx context.Context, id string) (AdminUser, error) {
	u, err := scanAdminUser(s.pool.QueryRow(ctx, adminUserSelectSQL+` WHERE u.id::text = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrUserNotFound
	}
	return u, err
}

// listUsers returns all users with pagination, newest first. Admin use only.
func (s *store) listUsers(ctx context.Context, cursor string, limit int32) ([]AdminUser, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 1000 {
		limit = 1000
	}
	var offset int64
	fmt.Sscanf(cursor, "%d", &offset)

	rows, err := s.pool.Query(ctx, adminUserSelectSQL+`
		ORDER BY u.created_at DESC
		LIMIT $1 OFFSET $2
	`, limit+1, offset)
	if err != nil {
		return nil, "", fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	var out []AdminUser
	for rows.Next() {
		u, err := scanAdminUser(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if int32(len(out)) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+int64(limit))
	}
	return out, next, nil
}

// updateUser applies partial updates to a user (admin only). Only non-nil fields
// are changed. Returns ErrUserNotFound if the user doesn't exist.
func (s *store) updateUser(ctx context.Context, id string, role *string, disabled *bool, maxSessions *int32) (AdminUser, error) {
	// Last-admin guard. Check and UPDATE must be one critical section — two
	// concurrent demotions can each observe admin_count=2 and demote both
	// admins (#148). Advisory lock: cheaper than SERIALIZABLE, contention nil.
	if role != nil && *role == RoleUser {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return AdminUser{}, fmt.Errorf("begin demote tx: %w", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, adminDemoteAdvisoryLock); err != nil {
			return AdminUser{}, fmt.Errorf("demote advisory lock: %w", err)
		}
		var currentRole string
		var adminCount int
		err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id::text = $1`, id).Scan(&currentRole)
		if errors.Is(err, pgx.ErrNoRows) {
			return AdminUser{}, ErrUserNotFound
		}
		if err != nil {
			return AdminUser{}, fmt.Errorf("check user role: %w", err)
		}
		if currentRole == RoleAdmin {
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&adminCount); err != nil {
				return AdminUser{}, fmt.Errorf("count admins: %w", err)
			}
			if adminCount <= 1 {
				return AdminUser{}, ErrLastAdmin
			}
			// Demote inside the same locked tx; the generic UPDATE below then
			// only applies the remaining fields (role already final here when
			// it is the sole change).
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET role = $2 WHERE id::text = $1`, id, *role); err != nil {
			return AdminUser{}, fmt.Errorf("demote user: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return AdminUser{}, fmt.Errorf("commit demote: %w", err)
		}
		role = nil // applied; don't re-apply in the generic UPDATE
	}

	var sets []string
	var args []any
	i := 1
	if role != nil {
		sets = append(sets, fmt.Sprintf("role = $%d", i))
		args = append(args, *role)
		i++
	}
	if disabled != nil {
		if *disabled {
			sets = append(sets, fmt.Sprintf("disabled_at = CASE WHEN disabled_at IS NULL THEN now() ELSE disabled_at END"))
		} else {
			sets = append(sets, "disabled_at = NULL")
		}
	}
	if maxSessions != nil {
		sets = append(sets, fmt.Sprintf("max_concurrent_sessions = $%d", i))
		args = append(args, *maxSessions)
		i++
	}
	if len(sets) == 0 {
		return s.readAdminUser(ctx, id) // no-op: fetch current
	}

	query := "UPDATE users SET "
	for j, s := range sets {
		if j > 0 {
			query += ", "
		}
		query += s
	}
	// RETURNING only the id: the two derived AdminUser fields are aggregates over
	// other tables, so the shape is read back through the one projection every
	// other AdminUser read uses rather than duplicated into this UPDATE.
	query += fmt.Sprintf(` WHERE id::text = $%d RETURNING id::text`, i)
	args = append(args, id)

	var updated string
	err := s.pool.QueryRow(ctx, query, args...).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrUserNotFound
	}
	if err != nil {
		return AdminUser{}, fmt.Errorf("update user: %w", err)
	}
	return s.readAdminUser(ctx, updated)
}

// deleteUser hard-deletes an account. Session history and tokens/devices
// cascade (migration 0007 / 0001 / 0004). Guards, all inside one advisory-
// locked transaction (shares the demote lock — both mutate the admin set):
//   - the user must exist                        → ErrUserNotFound
//   - the last admin cannot be deleted           → ErrLastAdmin
//   - no non-terminal sessions may remain        → ErrUserHasActiveSessions
//
// Self-deletion is refused at the handler (it knows the caller identity).
func (s *store) deleteUser(ctx context.Context, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, adminDemoteAdvisoryLock); err != nil {
		return fmt.Errorf("delete advisory lock: %w", err)
	}

	var role string
	err = tx.QueryRow(ctx, `SELECT role FROM users WHERE id::text = $1`, id).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if role == RoleAdmin {
		var admins int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
			return fmt.Errorf("count admins: %w", err)
		}
		if admins <= 1 {
			return ErrLastAdmin
		}
	}

	var active int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE user_id::text = $1 AND state NOT IN ('stopped','failed')
	`, id).Scan(&active); err != nil {
		return fmt.Errorf("count active sessions: %w", err)
	}
	if active > 0 {
		return ErrUserHasActiveSessions
	}

	// Tombstone all of the user's homes before deleting the user row (P5-05).
	// The FK ON DELETE SET NULL then orphans those rows (user_id → NULL) so the
	// GC janitor can reap the backing stores after the 24h grace period.
	if _, err := tx.Exec(ctx,
		`UPDATE user_homes SET gc_after = now() WHERE user_id = $1::uuid AND gc_after IS NULL`, id); err != nil {
		return fmt.Errorf("tombstone user homes: %w", err)
	}

	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id::text = $1`, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return tx.Commit(ctx)
}
