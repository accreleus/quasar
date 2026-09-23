package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// homeClaimOwner is the read-only candidate hint under the per-user advisory
// lock. The actual row lock/insert comes after host, placement and image-fence
// locks. Any known null or divergent location is an operator conflict,
// including tombstoned rows whose backing data has not been proved absent.
func homeClaimOwner(ctx context.Context, tx pgx.Tx, p CreateParams) (string, error) {
	if !p.ManagedHome {
		return "", nil
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
		return "", fmt.Errorf("read legacy home locations: %w", err)
	}
	if hostCount > 1 || unknown || tombstoned {
		return "", ErrHomeConflict
	}
	var hostID *string
	var state string
	err = tx.QueryRow(ctx, `
		SELECT host_id::text, state FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
	`, p.UserID, p.homeAppID()).Scan(&hostID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		if hostCount == 1 && soleHost != nil {
			return *soleHost, nil
		}
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read canonical home claim: %w", err)
	}
	if state == "conflict" || hostID == nil {
		return "", ErrHomeConflict
	}
	if hostCount == 1 && soleHost != nil && *soleHost != *hostID {
		return "", ErrHomeConflict
	}
	return *hostID, nil
}

// claimSelectedHome runs after the GPU and host have been rechecked and before
// the session row is inserted, in the same transaction. Rollback discards a
// first reservation. After commit, even a failed dispatch leaves the claim.
func claimSelectedHome(ctx context.Context, tx pgx.Tx, p CreateParams, hostID string) error {
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
	err = tx.QueryRow(ctx, `
		SELECT host_id::text, state FROM managed_home_claims
		WHERE user_id=$1::uuid AND canonical_app_id=$2::uuid
		FOR UPDATE
	`, p.UserID, p.homeAppID()).Scan(&owner, &state)
	if err != nil {
		return fmt.Errorf("verify canonical home claim: %w", err)
	}
	if state == "conflict" || owner == nil || *owner != hostID {
		return ErrHomeConflict
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
	if !app.ManagedHome {
		return nil
	}
	p := CreateParams{UserID: userID, AppID: app.ID, HomeAppID: homeAppID(app), ManagedHome: true}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin swap home claim: %w", err)
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
		return fmt.Errorf("lock swap session: %w", err)
	}
	if lockedUser != userID || lockedHost != hostID || lockedState != "running" ||
		(lockedDetail != nil && *lockedDetail == swapDetailInProgress) {
		return ErrSessionNotSwappable
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2::text))`, lockNamespaceUser, userID); err != nil {
		return fmt.Errorf("lock swap user: %w", err)
	}
	appIDs := []string{p.homeAppID()}
	if p.AppID != p.homeAppID() {
		appIDs = append(appIDs, p.AppID)
	}
	for _, appID := range appIDs {
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM apps WHERE id=$1::uuid FOR KEY SHARE`, appID).Scan(&lockedID); err != nil {
			return fmt.Errorf("lock swap app: %w", err)
		}
	}
	owner, err := homeClaimOwner(ctx, tx, p)
	if err != nil {
		return s.finishRefusedSwapHome(ctx, tx, p, err)
	}
	if owner == "" && app.IsDerived() {
		return ErrHomeNotProvisioned
	}
	if owner != "" && owner != hostID {
		return ErrHomeNotProvisioned
	}
	if err := claimSelectedHome(ctx, tx, p, hostID); err != nil {
		return s.finishRefusedSwapHome(ctx, tx, p, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state_detail=$2 WHERE id=$1::uuid`,
		sessionID, swapDetailInProgress); err != nil {
		return fmt.Errorf("persist pending swap guard: %w", err)
	}
	return tx.Commit(ctx)
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
