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
	var soleHost *string
	err := tx.QueryRow(ctx, `
		SELECT COUNT(DISTINCT uh.host_id), COALESCE(BOOL_OR(uh.host_id IS NULL), false),
		       MIN(uh.host_id::text)
		FROM user_homes uh
		JOIN apps a ON a.id=uh.app_id
		WHERE uh.user_id=$1::uuid AND COALESCE(a.parent_app_id,a.id)=$2::uuid
	`, p.UserID, p.homeAppID()).Scan(&hostCount, &unknown, &soleHost)
	if err != nil {
		return "", fmt.Errorf("read legacy home locations: %w", err)
	}
	if hostCount > 1 || unknown {
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
	// Lock every recorded location after the candidate was selected. Tombstone
	// and GC writers then serialize with this decision; a stale unlocked hint
	// cannot authorize a second host while a legacy row changes underneath it.
	rows, err := tx.Query(ctx, `
		SELECT uh.host_id::text
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
	for rows.Next() {
		var legacyHost *string
		if err := rows.Scan(&legacyHost); err != nil {
			return fmt.Errorf("scan legacy home location: %w", err)
		}
		if legacyHost == nil {
			unknown = true
		} else {
			legacyHosts[*legacyHost] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read legacy home locations: %w", err)
	}
	if len(legacyHosts) > 1 || unknown || (len(legacyHosts) == 1 && !hasHomeHost(legacyHosts, hostID)) {
		return ErrHomeConflict
	}
	_, err = tx.Exec(ctx, `
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
	return nil
}

func hasHomeHost(hosts map[string]struct{}, id string) bool {
	_, found := hosts[id]
	return found
}

// GuardHomeForSwap applies the same canonical host constraint to an existing
// session whose executable app changes without a new GPU reservation. It does
// not move a home. A first claim committed here remains protective if a later
// mount or agent operation fails, because that failure is not proof of absence.
func (s *Store) GuardHomeForSwap(ctx context.Context, userID string, app LaunchApp, hostID string) error {
	if !app.ManagedHome {
		return nil
	}
	p := CreateParams{UserID: userID, AppID: app.ID, HomeAppID: homeAppID(app), ManagedHome: true}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin swap home claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
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
		return err
	}
	if owner == "" && app.IsDerived() {
		return ErrHomeNotProvisioned
	}
	if owner != "" && owner != hostID {
		return ErrHomeConflict
	}
	if err := claimSelectedHome(ctx, tx, p, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
