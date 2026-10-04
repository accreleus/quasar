package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// homeClaimOwner is the read-only candidate hint under the per-user advisory
// lock. The actual row lock/insert comes after host, placement and image-fence
// locks. Any known null or divergent location is an operator conflict,
// including tombstoned rows whose backing data has not been proved absent.
// A pending-home hold by another session refuses as homeHoldRefusal decides.
func homeClaimOwner(ctx context.Context, tx pgx.Tx, p CreateParams, sameSession ...string) (string, error) {
	owner, holder, err := homeClaimLocation(ctx, tx, p)
	if err != nil || holder == "" || (len(sameSession) > 0 && holder == sameSession[0]) {
		return owner, err
	}
	return "", homeHoldRefusal(ctx, tx, holder, owner)
}

// homeClaimLocation is homeClaimOwner without the pending-home hold: the
// owner host plus the holding session ID, or "" for none. Only advisory
// pre-schedule reads use it directly; the reservation transaction always
// rechecks the hold.
func homeClaimLocation(ctx context.Context, tx pgx.Tx, p CreateParams) (owner, holder string, _ error) {
	if !p.ManagedHome {
		return "", "", nil
	}
	var hostCount int
	var unknown bool
	var tombstoned bool
	var soleHost *string
	err := tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT uh.host_id), COALESCE(BOOL_OR(uh.host_id IS NULL), false),
		       COALESCE(BOOL_OR(uh.gc_after IS NOT NULL), false), MIN(uh.host_id::text)
		FROM user_homes uh
		JOIN apps a ON a.id=uh.app_id
		WHERE uh.user_id=$1::uuid AND COALESCE(a.parent_app_id,a.id)=$2::uuid
	`, p.UserID, p.homeAppID()).Scan(&hostCount, &unknown, &tombstoned, &soleHost)
	if err != nil {
		return "", "", fmt.Errorf("read legacy home locations: %w", err)
	}
	if hostCount > 1 || unknown || tombstoned {
		return "", "", ErrHomeConflict
	}
	var hostID *string
	var state string
	var pendingSession *string
	err = tx.QueryRow(ctx, `
		SELECT host_id::text, state, pending_home_session_id::text FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
	`, p.UserID, p.homeAppID()).Scan(&hostID, &state, &pendingSession)
	if errors.Is(err, pgx.ErrNoRows) {
		if hostCount == 1 && soleHost != nil {
			return *soleHost, "", nil
		}
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read canonical home claim: %w", err)
	}
	if state == "conflict" || hostID == nil {
		return "", "", ErrHomeConflict
	}
	if hostCount == 1 && soleHost != nil && *soleHost != *hostID {
		return "", "", ErrHomeConflict
	}
	if pendingSession != nil {
		holder = *pendingSession
	}
	return *hostID, holder, nil
}

// The pending-home hold outlives its session row: only the agent's qualified
// terminal clears it (home_hold.go), and that proof can trail the session's
// own terminal state by a moment (#434). A relaunch in that gap is not a case
// for operator review, so a hold by another session is refused by what its
// holder is doing:
//
//   - holder still live (pending … stopping): the single-writer rule's
//     retryable home_in_use, naming the holder. It is the caller's own session
//     (claims are per user), the same answer gate 2b gives without a hold.
//   - holder terminal on the claim's host, that host connected, and ended
//     within homeHoldSettleWindow: the hold is settling. ScheduleAndCreate waits
//     up to homeHoldSettleWait for the proof, then falls back to home_conflict,
//     which the contract fixes for a hold (control-api.md, the 0090 hold).
//   - anything else — no holder row, holder on another host, host offline, or
//     proof overdue: the hold will not clear by itself, so home_conflict.
//
// None of these is persisted: persistHomeConflict records location evidence
// only, never a hold.
const homeHoldSettleWindow = 2 * time.Minute

// homeHoldSettleWait bounds one launch's wait for a settling hold. A variable
// so tests can shorten it.
var homeHoldSettleWait = 10 * time.Second

const homeHoldPollInterval = 200 * time.Millisecond

// homeHoldSettlingError is a home_conflict that waiting may resolve. It
// unwraps to ErrHomeConflict, so every caller that does not wait answers as
// before.
type homeHoldSettlingError struct{ holder string }

func (e *homeHoldSettlingError) Error() string { return ErrHomeConflict.Error() }
func (e *homeHoldSettlingError) Unwrap() error { return ErrHomeConflict }

func homeHoldRefusal(ctx context.Context, tx pgx.Tx, holder, claimHost string) error {
	var state State
	var host *string
	var connected, recent bool
	err := tx.QueryRow(ctx, `SELECT s.state, s.host_id::text,
		COALESCE(h.status IN ('online','draining'), false),
		COALESCE(s.ended_at, s.updated_at) > now() - ($2::int * interval '1 second')
		FROM sessions s LEFT JOIN hosts h ON h.id=s.host_id
		WHERE s.id=$1::uuid`, holder, int(homeHoldSettleWindow/time.Second)).
		Scan(&state, &host, &connected, &recent)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrHomeConflict
	}
	if err != nil {
		return fmt.Errorf("read home hold session: %w", err)
	}
	switch {
	case !state.IsTerminal():
		return &HomeInUseError{SessionID: holder, Stopping: state == StateStopping}
	case host != nil && *host == claimHost && connected && recent:
		return &homeHoldSettlingError{holder: holder}
	default:
		return ErrHomeConflict
	}
}

// awaitHomeHoldRelease polls, outside any transaction, until holder no longer
// holds the claim (true) or until passes (false).
func (s *Store) awaitHomeHoldRelease(ctx context.Context, p CreateParams, holder string, until time.Time) bool {
	for {
		var held bool
		err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM managed_home_claims
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid AND pending_home_session_id=$3::uuid)`,
			p.UserID, p.homeAppID(), holder).Scan(&held)
		if err != nil {
			return false
		}
		if !held {
			return true
		}
		wait := time.Until(until)
		if wait <= 0 {
			return false
		}
		if wait > homeHoldPollInterval {
			wait = homeHoldPollInterval
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}
}

// claimSelectedHome runs after the GPU and host have been rechecked and before
// the session row is inserted, in the same transaction. Rollback discards a
// first reservation. After commit, even a failed dispatch leaves the claim.
func claimSelectedHome(ctx context.Context, tx pgx.Tx, p CreateParams, hostID string, sameSession ...string) error {
	if !p.ManagedHome {
		return nil
	}
	// Claim ownership precedes every user_homes row lock. A running callback
	// takes session → claim → home; reversing the latter two here deadlocks a
	// concurrent materialization. A later legacy-row conflict rolls this first
	// reservation back with the surrounding session transaction.
	_, err := tx.Exec(ctx, `
		INSERT INTO managed_home_claims
		    (user_id, canonical_app_id, host_id, state)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'reserved')
		ON CONFLICT (user_id, canonical_app_id) DO NOTHING
	`, p.UserID, p.homeAppID(), hostID)
	if err != nil {
		return fmt.Errorf("reserve canonical home: %w", err)
	}
	var owner *string
	var state string
	var pendingSession *string
	err = tx.QueryRow(ctx, `
		SELECT host_id::text, state, pending_home_session_id::text FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
		FOR UPDATE
	`, p.UserID, p.homeAppID()).Scan(&owner, &state, &pendingSession)
	if err != nil {
		return fmt.Errorf("verify canonical home claim: %w", err)
	}
	if state == "conflict" || owner == nil || *owner != hostID {
		return ErrHomeConflict
	}
	if pendingSession != nil && (len(sameSession) == 0 || *pendingSession != sameSession[0]) {
		return homeHoldRefusal(ctx, tx, *pendingSession, hostID)
	}
	// Lock every recorded location after the candidate was selected. Tombstone
	// and GC writers then serialize with this decision; a stale unlocked hint
	// cannot authorize a second host while a legacy row changes underneath it.
	rows, err := tx.Query(ctx, `
		SELECT uh.host_id::text,uh.gc_after IS NOT NULL
		FROM user_homes uh JOIN apps a ON a.id=uh.app_id
		WHERE uh.user_id=$1::uuid AND COALESCE(a.parent_app_id,a.id)=$2::uuid
		FOR UPDATE OF uh
	`, p.UserID, p.homeAppID())
	if err != nil {
		return fmt.Errorf("lock legacy home locations: %w", err)
	}
	defer rows.Close()
	legacyHosts := make(map[string]struct{})
	var unknown bool
	var tombstoned bool
	for rows.Next() {
		var legacyHost *string
		var rowTombstoned bool
		if err := rows.Scan(&legacyHost, &rowTombstoned); err != nil {
			return fmt.Errorf("scan legacy home location: %w", err)
		}
		if legacyHost == nil {
			unknown = true
		} else {
			legacyHosts[*legacyHost] = struct{}{}
		}
		tombstoned = tombstoned || rowTombstoned
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read legacy home locations: %w", err)
	}
	if len(legacyHosts) > 1 || unknown || tombstoned || (len(legacyHosts) == 1 && !hasHomeHost(legacyHosts, hostID)) {
		return ErrHomeConflict
	}
	return nil
}

func hasHomeHost(hosts map[string]struct{}, id string) bool {
	_, found := hosts[id]
	return found
}

// persistHomeConflict runs after a refused reservation's transaction has
// rolled back. A conflict discovered inside that transaction cannot be stored
// there: rolling the refused reservation back would erase the diagnosis too.
// The read of legacy locations takes no row locks; the claim is the only row
// locked or written, preserving claim → home for concurrent callbacks/GC.
func (s *Store) persistHomeConflict(ctx context.Context, p CreateParams) error {
	if !p.ManagedHome {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin home conflict diagnosis: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1,hashtext($2::text))`, lockNamespaceUser, p.UserID); err != nil {
		return fmt.Errorf("lock home conflict user: %w", err)
	}
	var owner *string
	var previous *string
	err = tx.QueryRow(ctx, `SELECT host_id::text,conflict_reason FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid FOR UPDATE`, p.UserID, p.homeAppID()).
		Scan(&owner, &previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock home conflict claim: %w", err)
	}
	claimExists := err == nil
	var hostCount int
	var unknown, tombstoned bool
	var soleHost *string
	err = tx.QueryRow(ctx, `SELECT COUNT(DISTINCT uh.host_id),
		COALESCE(BOOL_OR(uh.host_id IS NULL),false),
		COALESCE(BOOL_OR(uh.gc_after IS NOT NULL),false),MIN(uh.host_id::text)
		FROM user_homes uh JOIN apps a ON a.id=uh.app_id
		WHERE uh.user_id=$1::uuid AND COALESCE(a.parent_app_id,a.id)=$2::uuid`,
		p.UserID, p.homeAppID()).Scan(&hostCount, &unknown, &tombstoned, &soleHost)
	if err != nil {
		return fmt.Errorf("read known home conflict locations: %w", err)
	}
	reason := ""
	switch {
	case unknown:
		reason = "legacy_location_uncertain"
	case hostCount > 1:
		reason = "location_mismatch"
	case hostCount == 1 && claimExists && owner != nil && soleHost != nil && *owner != *soleHost:
		reason = "location_mismatch"
	case claimExists && owner == nil:
		reason = "claim_owner_missing"
	case tombstoned:
		reason = "gc_pending"
	}
	if reason == "" {
		return nil
	}
	// Earlier divergent/unknown evidence outranks a later missing owner or GC
	// mark. Never erase historical materialized_at when changing the state.
	if previous != nil {
		switch *previous {
		case "legacy_location_uncertain", "location_mismatch":
			reason = *previous
		case "claim_owner_missing":
			if reason == "gc_pending" {
				reason = *previous
			}
		}
	}
	if claimExists {
		_, err = tx.Exec(ctx, `UPDATE managed_home_claims SET state='conflict',conflict_reason=$3
			WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, p.UserID, p.homeAppID(), reason)
	} else {
		var claimHost *string
		if hostCount == 1 && !unknown {
			claimHost = soleHost
		}
		_, err = tx.Exec(ctx, `INSERT INTO managed_home_claims
			(user_id,canonical_app_id,host_id,state,conflict_reason)
			VALUES ($1::uuid,$2::uuid,$3::uuid,'conflict',$4)
			ON CONFLICT (user_id,canonical_app_id) DO NOTHING`, p.UserID, p.homeAppID(), claimHost, reason)
	}
	if err != nil {
		return fmt.Errorf("persist home conflict diagnosis: %w", err)
	}
	return tx.Commit(ctx)
}

// GuardHomeForSwap applies the same canonical host constraint to an existing
// session whose executable app changes without a new GPU reservation. It does
// not move a home. A first claim committed here remains protective if a later
// mount or agent operation fails, because that failure is not proof of absence.
func (s *Store) GuardHomeForSwap(ctx context.Context, sessionID, userID string, app LaunchApp, hostID string) error {
	_, err := s.GuardHomeForSwapWithHold(ctx, sessionID, userID, app, hostID, false)
	return err
}

// GuardHomeForSwapWithHold creates a cleanup-capable target hold in the same
// transaction as the durable swap guard. A prior same-session hold is reused.
func (s *Store) GuardHomeForSwapWithHold(ctx context.Context, sessionID, userID string, app LaunchApp, hostID string, capable bool) (*HomeHoldDecision, error) {
	if !app.ManagedHome {
		return nil, nil
	}
	p := CreateParams{UserID: userID, AppID: app.ID, HomeAppID: homeAppID(app), ManagedHome: true}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin swap home claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Existing-session operations lock session before claim before home. This
	// row is also the durable swap guard's owner; the in-memory target map may
	// disappear on restart, so it cannot be the sole GC protection.
	var lockedUser, lockedHost, lockedState string
	var lockedDetail *string
	if err := tx.QueryRow(ctx, `SELECT user_id::text,host_id::text,state,state_detail
		FROM sessions WHERE id=$1::uuid FOR UPDATE`, sessionID).
		Scan(&lockedUser, &lockedHost, &lockedState, &lockedDetail); err != nil {
		return nil, fmt.Errorf("lock swap session: %w", err)
	}
	if lockedUser != userID || lockedHost != hostID || lockedState != "running" ||
		(lockedDetail != nil && *lockedDetail == swapDetailInProgress) {
		return nil, ErrSessionNotSwappable
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceUser, userID); err != nil {
		return nil, fmt.Errorf("lock swap user: %w", err)
	}
	appIDs := []string{p.homeAppID()}
	if p.AppID != p.homeAppID() {
		appIDs = append(appIDs, p.AppID)
	}
	for _, appID := range appIDs {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM apps WHERE id=$1::uuid FOR KEY SHARE`, appID).Scan(&lockedID); err != nil {
			return nil, fmt.Errorf("lock swap app: %w", err)
		}
	}
	selected, err := placementSelectedForHost(ctx, tx, p.homeAppID(), hostID)
	if err != nil {
		return nil, fmt.Errorf("check swap placement: %w", err)
	}
	if !selected {
		return nil, ErrHomeConflict
	}
	owner, err := homeClaimOwner(ctx, tx, p, sessionID)
	if err != nil {
		return nil, s.finishRefusedSwapHome(ctx, tx, p, err)
	}
	if owner == "" && app.IsDerived() {
		return nil, ErrHomeNotProvisioned
	}
	if owner != "" && owner != hostID {
		return nil, ErrHomeNotProvisioned
	}
	if err := claimSelectedHome(ctx, tx, p, hostID, sessionID); err != nil {
		return nil, s.finishRefusedSwapHome(ctx, tx, p, err)
	}
	var priorToken *string
	if err := tx.QueryRow(ctx, `SELECT pending_home_token::text FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid`, userID, p.homeAppID()).Scan(&priorToken); err != nil {
		return nil, fmt.Errorf("read swap home hold: %w", err)
	}
	decision, err := setHomeDispatchHold(ctx, tx, userID, p.homeAppID(), sessionID, capable, priorToken)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state_detail=$2 WHERE id=$1::uuid`,
		sessionID, swapDetailInProgress); err != nil {
		return nil, fmt.Errorf("persist pending swap guard: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return decision, nil
}

func (s *Store) finishRefusedSwapHome(ctx context.Context, tx pgx.Tx, p CreateParams, guardErr error) error {
	if !errors.Is(guardErr, ErrHomeConflict) {
		return guardErr
	}
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("rollback refused swap claim: %w", err)
	}
	if err := s.persistHomeConflict(ctx, p); err != nil {
		return err
	}
	return guardErr
}
