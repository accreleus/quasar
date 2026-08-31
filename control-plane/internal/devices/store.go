// Package devices implements the user_devices store (P4-08 / schema.md § user_devices).
// One row per (user, device_key): a login-time probe of the client's codec/decode +
// network capability. Phase 4 writes it; no consumer reads it (that is a later phase).
package devices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a device row does not exist.
var ErrNotFound = errors.New("device not found")

// Store wraps the pool for user_devices reads and writes.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Device is one row from user_devices, returned by Upsert.
type Device struct {
	ID          string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// UpsertParams carries the inputs for the owner-scoped upsert.
type UpsertParams struct {
	// UserID must come from the bearer-token identity (auth.UserFromContext),
	// never the request body.
	UserID    string
	DeviceKey string
	UserAgent string
	// Capabilities is raw client JSON; measured_at is server-stamped before
	// storage and unknown keys pass through (the column is schema-free).
	Capabilities json.RawMessage
}

// Upsert inserts a new (user_id, device_key) row or updates capabilities,
// last_seen_at, and user_agent on a repeat call (same device_key for the same
// user). measured_at inside capabilities is always server-stamped — any
// client-supplied value is overwritten. Returns the id + timestamps of the row.
func (s *Store) Upsert(ctx context.Context, p UpsertParams) (Device, error) {
	clean, err := sanitizeCapabilities(p.Capabilities)
	if err != nil {
		return Device{}, fmt.Errorf("sanitize capabilities: %w", err)
	}

	// Server-stamp measured_at so a client cannot backdate it.
	stamped, err := stampMeasuredAt(clean)
	if err != nil {
		return Device{}, fmt.Errorf("stamp measured_at: %w", err)
	}

	var d Device
	var ua *string
	if p.UserAgent != "" {
		ua = &p.UserAgent
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO user_devices (user_id, device_key, user_agent, capabilities)
		VALUES ($1::uuid, $2, $3, $4)
		ON CONFLICT (user_id, device_key) DO UPDATE
		    SET capabilities  = EXCLUDED.capabilities,
		        last_seen_at  = now(),
		        user_agent    = EXCLUDED.user_agent
		RETURNING id::text, first_seen_at, last_seen_at
	`, p.UserID, p.DeviceKey, ua, []byte(stamped)).
		Scan(&d.ID, &d.FirstSeenAt, &d.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, fmt.Errorf("upsert device: %w", err)
	}
	return d, nil
}

// ErrForbidden is returned by owner-scoped device operations when the id does not belong
// to the caller (or does not exist). The handler maps it to 403 — never 404 — so the
// endpoint leaks no information about other users' device ids (LP-SEC-01 §B.6).
var ErrForbidden = errors.New("device not owned by caller")

// Item is one row of GET /v1/me/devices (LP-SEC-01 §B.6). Capabilities is the stored
// (sanitized, measured_at-stamped) JSON blob returned verbatim. Current marks the device
// the bearer token is bound to; ActiveSessionID is the device's live session, if any.
type Item struct {
	ID              string          `json:"id"`
	DeviceKey       string          `json:"device_key"`
	Name            *string         `json:"name"`
	Trusted         bool            `json:"trusted"`
	FirstSeenAt     time.Time       `json:"first_seen_at"`
	LastSeenAt      time.Time       `json:"last_seen_at"`
	Current         bool            `json:"current"`
	ActiveSessionID *string         `json:"active_session_id"`
	Capabilities    json.RawMessage `json:"capabilities"`
}

// activeSessionStates are the non-terminal session states whose row still represents a
// live session for the account UI (mirrors sessions.state minus 'stopped'/'failed').
const activeSessionStates = `('pending','assigned','starting','running','stopping')`

// List returns all of the caller's devices, newest-first by last_seen_at (LP-SEC-01 §B.6).
// currentDeviceID (the bearer token's bound device, "" when unbound) flags the current
// row. Owner-scoped: userID MUST be the bearer identity.
func (s *Store) List(ctx context.Context, userID, currentDeviceID string) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT d.id::text, d.device_key, d.name, d.trusted, d.first_seen_at, d.last_seen_at,
		       d.capabilities,
		       (SELECT sess.id::text FROM sessions sess
		         WHERE sess.device_id = d.id
		           AND sess.state IN `+activeSessionStates+`
		         ORDER BY sess.created_at DESC LIMIT 1) AS active_session_id
		FROM user_devices d
		WHERE d.user_id = $1::uuid
		ORDER BY d.last_seen_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	defer rows.Close()

	out := make([]Item, 0)
	for rows.Next() {
		var it Item
		var rawCaps []byte
		if err := rows.Scan(&it.ID, &it.DeviceKey, &it.Name, &it.Trusted,
			&it.FirstSeenAt, &it.LastSeenAt, &rawCaps, &it.ActiveSessionID); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		if len(rawCaps) == 0 {
			rawCaps = []byte("{}")
		}
		it.Capabilities = json.RawMessage(rawCaps)
		it.Current = it.ID != "" && it.ID == currentDeviceID
		out = append(out, it)
	}
	return out, rows.Err()
}

// getOne loads a single owner-scoped device row (for the PATCH response), computing
// current/active_session_id like List. Returns ErrForbidden when the id is not the
// caller's. currentDeviceID flags the current row.
func (s *Store) getOne(ctx context.Context, userID, deviceID, currentDeviceID string) (Item, error) {
	var it Item
	var rawCaps []byte
	err := s.pool.QueryRow(ctx, `
		SELECT d.id::text, d.device_key, d.name, d.trusted, d.first_seen_at, d.last_seen_at,
		       d.capabilities,
		       (SELECT sess.id::text FROM sessions sess
		         WHERE sess.device_id = d.id
		           AND sess.state IN `+activeSessionStates+`
		         ORDER BY sess.created_at DESC LIMIT 1) AS active_session_id
		FROM user_devices d
		WHERE d.id = $1::uuid AND d.user_id = $2::uuid
	`, deviceID, userID).Scan(&it.ID, &it.DeviceKey, &it.Name, &it.Trusted,
		&it.FirstSeenAt, &it.LastSeenAt, &rawCaps, &it.ActiveSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrForbidden
	}
	if err != nil {
		return Item{}, fmt.Errorf("get device: %w", err)
	}
	if len(rawCaps) == 0 {
		rawCaps = []byte("{}")
	}
	it.Capabilities = json.RawMessage(rawCaps)
	it.Current = it.ID == currentDeviceID
	return it, nil
}

// UpdateNameTrust applies an owner-scoped rename / trust change (LP-SEC-01 §B.6). Only
// non-nil fields change. The WHERE clause is owner-scoped so a mismatched (or unknown) id
// affects zero rows ⇒ ErrForbidden (403, no existence leak). Returns the updated row.
func (s *Store) UpdateNameTrust(ctx context.Context, userID, deviceID, currentDeviceID string, name *string, trusted *bool) (Item, error) {
	if name == nil && trusted == nil {
		// Nothing to change: return current (still owner-scoped).
		return s.getOne(ctx, userID, deviceID, currentDeviceID)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE user_devices
		SET name    = COALESCE($3, name),
		    trusted = COALESCE($4, trusted)
		WHERE id = $1::uuid AND user_id = $2::uuid
	`, deviceID, userID, name, trusted)
	if err != nil {
		return Item{}, fmt.Errorf("update device: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Item{}, ErrForbidden
	}
	return s.getOne(ctx, userID, deviceID, currentDeviceID)
}

// Revoke is the load-bearing device-revocation (LP-SEC-01 §B.6). Owner-scoped and atomic:
//  1. verify the device is the caller's (else ErrForbidden — 403, no leak);
//  2. collect the ids of the device's live sessions (so the handler can end them);
//  3. REVOKE every auth_token bound to the device (revoked_at = now()) — this is the real
//     revocation: that device's next request 401s, not merely a display row removed;
//  4. delete the user_devices row (ON DELETE SET NULL protects the just-revoked tokens
//     and the sessions from a cascade; a re-login with the same device_key gets a fresh
//     row + fresh bindable token — it never reclaims the revoked one).
//
// Returns the live session ids so the handler can tear them down via the existing session
// teardown (no agent-wire change). Token revocation happens BEFORE the row delete.
func (s *Store) Revoke(ctx context.Context, userID, deviceID string) ([]string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin revoke tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op after commit

	var exists bool
	err = tx.QueryRow(ctx, `
		SELECT true FROM user_devices WHERE id = $1::uuid AND user_id = $2::uuid
	`, deviceID, userID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrForbidden
	}
	if err != nil {
		return nil, fmt.Errorf("verify device owner: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT id::text FROM sessions
		WHERE device_id = $1::uuid AND state IN `+activeSessionStates+`
	`, deviceID)
	if err != nil {
		return nil, fmt.Errorf("collect device sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan session id: %w", err)
		}
		sessionIDs = append(sessionIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE auth_tokens SET revoked_at = now()
		WHERE device_id = $1::uuid AND revoked_at IS NULL
	`, deviceID); err != nil {
		return nil, fmt.Errorf("revoke device tokens: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		DELETE FROM user_devices WHERE id = $1::uuid AND user_id = $2::uuid
	`, deviceID, userID); err != nil {
		return nil, fmt.Errorf("delete device row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit revoke tx: %w", err)
	}
	return sessionIDs, nil
}

// stampMeasuredAt returns a copy of raw with "measured_at" set to the current
// server time (RFC3339). If raw is nil/empty/invalid JSON it is treated as an
// empty object. The server value always wins — any client-supplied measured_at
// is overwritten (control-api.md: "measured_at is server-stamped at upsert").
func stampMeasuredAt(raw json.RawMessage) (json.RawMessage, error) {
	m := make(map[string]json.RawMessage)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			// Treat unparseable JSON as an empty object — the handler validates
			// the shape before calling us, so this is a defensive fallback.
			m = make(map[string]json.RawMessage)
		}
	}
	ts, err := json.Marshal(time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	m["measured_at"] = ts
	return json.Marshal(m)
}
