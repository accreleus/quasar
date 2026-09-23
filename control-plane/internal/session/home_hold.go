package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/jackc/pgx/v5"
)

// HomeHoldDecision is private dispatch correlation. It is never serialized,
// logged or sent to the agent. Only Created holds may be released for a proven
// pre-handoff failure or an exact-epoch negative ack.
type HomeHoldDecision struct {
	Token     string
	Created   bool
	UserID    string
	AppID     string
	SessionID string
}

// ClearQualifiedHomeHolds consumes authenticated cleanup proof from the
// current cleanup-capable reporting connection. A missing historical session
// row is allowed; a present row must still belong to the reporting host.
// Claim rows are locked in canonical order and NULL-host claims stay held.
func (s *Store) ClearQualifiedHomeHolds(ctx context.Context, hostID, sessionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin home cleanup proof: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var owner *string
	err = tx.QueryRow(ctx, `SELECT host_id::text FROM sessions WHERE id=$1::uuid FOR UPDATE`, sessionID).Scan(&owner)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock cleanup session: %w", err)
	}
	if err == nil && (owner == nil || *owner != hostID) {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT user_id::text,canonical_app_id::text
		FROM managed_home_claims WHERE pending_home_session_id=$1::uuid
		ORDER BY user_id,canonical_app_id FOR UPDATE`, sessionID)
	if err != nil {
		return fmt.Errorf("lock pending home claims: %w", err)
	}
	type key struct{ user, app string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.user, &k.app); err != nil {
			rows.Close()
			return fmt.Errorf("read pending home claim: %w", err)
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read pending home claims: %w", err)
	}
	rows.Close()
	for _, k := range keys {
		_, err := tx.Exec(ctx, `UPDATE managed_home_claims SET pending_home_session_id=NULL,
			pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
			  AND pending_home_session_id=$3::uuid AND host_id=$4::uuid`,
			k.user, k.app, sessionID, hostID)
		if err != nil {
			return fmt.Errorf("apply qualified home cleanup: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit home cleanup proof: %w", err)
	}
	return nil
}

// HeldTerminalSessionIDsOnHost is the recovery scan after a capable register
// and on each capped retry tick. A lost terminal may have been followed by
// heartbeat reaping on the same connection, so this cannot be register-only.
func (s *Store) HeldTerminalSessionIDsOnHost(ctx context.Context, hostID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT c.pending_home_session_id::text
		FROM managed_home_claims c
		LEFT JOIN sessions s ON s.id=c.pending_home_session_id
		WHERE c.host_id=$1::uuid AND c.pending_home_session_id IS NOT NULL
		  AND (s.id IS NULL OR s.state IN ('stopped','failed'))
		ORDER BY c.pending_home_session_id::text`, hostID)
	if err != nil {
		return nil, fmt.Errorf("scan pending home cleanup: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read pending cleanup session: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read pending cleanup sessions: %w", err)
	}
	return ids, nil
}

// ReconcilePendingHomes asks the exact capable epoch to repeat stop until its
// qualified terminal arrives. A stop ack is deliberately ignored as proof.
// Running sessions are never stopped just to release a hold.
func (c *Coordinator) ReconcilePendingHomes(ctx context.Context, hostID string, epoch agentws.HomeCommandEpoch) {
	if epoch == nil || !epoch.SupportsHomeCleanup() {
		return
	}
	ids, err := c.store.HeldTerminalSessionIDsOnHost(ctx, hostID)
	if err != nil {
		c.log.Warn("pending home cleanup scan failed", "host_id", hostID, "err", err)
		return
	}
	for _, id := range ids {
		cmd := agentws.SessionStopCmd{Type: "session_stop", ID: newCmdID(), SessionID: id, Reason: "error"}
		if _, err := epoch.Send(cmd); err != nil {
			c.log.Warn("pending home cleanup retry failed", "host_id", hostID, "err", err)
			return
		}
	}
}

// setHomeDispatchHold runs with the session and claim locked, before the
// dispatch transaction commits. The same-session target keeps its first token.
func setHomeDispatchHold(ctx context.Context, tx pgx.Tx, userID, appID, sessionID string, capable bool, priorToken *string) (*HomeHoldDecision, error) {
	if !capable {
		if priorToken != nil {
			return nil, ErrHomeConflict
		}
		_, err := tx.Exec(ctx, `UPDATE managed_home_claims
			SET legacy_unprotected_dispatch=true
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, appID)
		if err != nil {
			return nil, fmt.Errorf("record unsupported home dispatch: %w", err)
		}
		return nil, nil
	}
	if priorToken != nil {
		return &HomeHoldDecision{Token: *priorToken, UserID: userID, AppID: appID, SessionID: sessionID}, nil
	}
	var token string
	err := tx.QueryRow(ctx, `UPDATE managed_home_claims SET
		pending_home_session_id=$3::uuid,pending_home_token=gen_random_uuid(),
		pending_home_started_at=now()
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
		  AND pending_home_token IS NULL
		RETURNING pending_home_token::text`, userID, appID, sessionID).Scan(&token)
	if err != nil {
		return nil, fmt.Errorf("hold managed home dispatch: %w", err)
	}
	return &HomeHoldDecision{Token: token, Created: true, UserID: userID, AppID: appID, SessionID: sessionID}, nil
}

// ClearNewHomeHold is only for a newly created hold whose command is proved
// to have had no home side effect. The token CAS cannot clear an older hold
// reused by a later same-session swap.
func (s *Store) ClearNewHomeHold(ctx context.Context, hold *HomeHoldDecision) error {
	if hold == nil || !hold.Created {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE managed_home_claims SET
		pending_home_session_id=NULL,pending_home_token=NULL,pending_home_started_at=NULL
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
		  AND pending_home_session_id=$3::uuid AND pending_home_token=$4::uuid`,
		hold.UserID, hold.AppID, hold.SessionID, hold.Token)
	if err != nil {
		return fmt.Errorf("release unstarted home operation: %w", err)
	}
	return nil
}

// RefreshSwapHomeHold changes only an unstarted swap's capability decision
// after its command epoch vanished before queue handoff. The session and claim
// remain locked in global order; an older reused hold cannot be downgraded to
// an unsupported connection.
func (s *Store) RefreshSwapHomeHold(ctx context.Context, sessionID, userID, appID, hostID string, prior *HomeHoldDecision, capable bool) (*HomeHoldDecision, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin swap epoch refresh: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var owner, host, state string
	var detail *string
	err = tx.QueryRow(ctx, `SELECT user_id::text,host_id::text,state,state_detail
		FROM sessions WHERE id=$1::uuid FOR UPDATE`, sessionID).Scan(&owner, &host, &state, &detail)
	if err != nil {
		return nil, fmt.Errorf("lock pending swap session: %w", err)
	}
	if owner != userID || host != hostID || state != "running" || detail == nil || *detail != swapDetailInProgress {
		return nil, ErrSessionNotSwappable
	}
	var claimHost, pendingSession, pendingToken *string
	var claimState string
	err = tx.QueryRow(ctx, `SELECT host_id::text,state,pending_home_session_id::text,pending_home_token::text
		FROM managed_home_claims WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`,
		userID, appID).Scan(&claimHost, &claimState, &pendingSession, &pendingToken)
	if err != nil {
		return nil, fmt.Errorf("lock pending swap claim: %w", err)
	}
	if claimHost == nil || *claimHost != hostID || claimState == "conflict" ||
		(pendingSession != nil && *pendingSession != sessionID) {
		return nil, ErrHomeConflict
	}
	if prior != nil && pendingToken != nil && *pendingToken != prior.Token {
		return nil, ErrHomeConflict
	}
	if !capable && pendingToken != nil {
		if prior == nil || !prior.Created {
			return nil, ErrHomeConflict
		}
		_, err = tx.Exec(ctx, `UPDATE managed_home_claims SET pending_home_session_id=NULL,
			pending_home_token=NULL,pending_home_started_at=NULL
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid AND pending_home_token=$3::uuid`,
			userID, appID, prior.Token)
		if err != nil {
			return nil, fmt.Errorf("release unstarted swap hold: %w", err)
		}
		pendingToken = nil
	}
	decision, err := setHomeDispatchHold(ctx, tx, userID, appID, sessionID, capable, pendingToken)
	if err != nil {
		return nil, err
	}
	if decision != nil && prior != nil && prior.Created && decision.Token == prior.Token {
		decision.Created = true
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return decision, nil
}
