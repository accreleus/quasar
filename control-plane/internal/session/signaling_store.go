package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrTokenInvalid is returned when the signaling token is not found, has
	// expired, or has already been consumed. Mapped to WS close 4401.
	ErrTokenInvalid = errors.New("signaling token invalid, expired, or already used")
	// ErrSessionTerminal is returned when the session is already stopped or failed.
	// Mapped to WS close 4404.
	ErrSessionTerminal = errors.New("session is terminal")
	// ErrSessionNotReady is returned when the session has no host assigned yet
	// (state pending). The client should retry shortly. Mapped to WS close 4409.
	ErrSessionNotReady = errors.New("session not yet assigned to a host")
)

// ConsumeSignalingToken hashes plaintext, validates TTL and single-use,
// atomically stamps consumed_at, and returns the session. FOR UPDATE ensures
// two concurrent WS connects with the same token cannot both succeed.
func (s *Store) ConsumeSignalingToken(ctx context.Context, plaintext string) (Session, error) {
	h := sha256.Sum256([]byte(plaintext))
	hash := hex.EncodeToString(h[:])

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Session{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var state State
	var hostID *string
	var expired, consumed bool
	var sessionID string
	err = tx.QueryRow(ctx, `
		SELECT s.id::text, s.state, s.host_id::text,
		       t.expires_at < now(), t.consumed_at IS NOT NULL
		FROM session_tokens t
		JOIN sessions s ON s.id = t.session_id
		WHERE t.token_hash = $1
		FOR UPDATE OF t
	`, hash).Scan(&sessionID, &state, &hostID, &expired, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrTokenInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup token: %w", err)
	}

	if expired || consumed {
		return Session{}, ErrTokenInvalid
	}
	if state.IsTerminal() {
		return Session{}, ErrSessionTerminal
	}
	if hostID == nil {
		return Session{}, ErrSessionNotReady
	}

	_, err = tx.Exec(ctx, `
		UPDATE session_tokens SET consumed_at = now()
		WHERE token_hash = $1
	`, hash)
	if err != nil {
		return Session{}, fmt.Errorf("consume token: %w", err)
	}

	sess, err := scanSession(tx.QueryRow(ctx, selectSessionSQL+` WHERE id = $1::uuid`, sessionID))
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, fmt.Errorf("commit: %w", err)
	}
	return sess, nil
}

// MintSignalingToken creates another independently single-use token for a live
// session. Ownership is checked by the HTTP handler before this method is called.
func (s *Store) MintSignalingToken(ctx context.Context, sessionID string) (signalingToken, error) {
	if !isValidUUID(sessionID) {
		return signalingToken{}, ErrSessionTerminal
	}
	token, err := newSignalingToken(time.Now())
	if err != nil {
		return signalingToken{}, err
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO session_tokens (session_id, token_hash, expires_at)
		SELECT id, $2, $3 FROM sessions
		WHERE id = $1::uuid AND state IN ('assigned', 'starting', 'running')
	`, sessionID, token.Hash, token.ExpiresAt)
	if err != nil {
		return signalingToken{}, fmt.Errorf("insert signaling token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return signalingToken{}, ErrSessionTerminal
	}
	return token, nil
}
