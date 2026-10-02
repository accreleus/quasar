package session

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// placementSelectedForHost holds the canonical policy row through an accepted
// reservation or swap. A removal takes FOR UPDATE on that same row, so it
// cannot pass a committed removal and then dispatch a newly accepted change.
func placementSelectedForHost(ctx context.Context, tx pgx.Tx, canonicalAppID, hostID string) (bool, error) {
	// Under READ COMMITTED, a SELECT that blocks on FOR SHARE keeps the
	// statement's earlier snapshot for its child-table subquery. Lock first,
	// then take a NEW statement snapshot after the writer commits.
	var locked string
	if err := tx.QueryRow(ctx, `SELECT app_id::text FROM app_placement
		WHERE app_id=$1::uuid FOR SHARE`, canonicalAppID).Scan(&locked); err != nil {
		return false, err
	}
	var selected bool
	err := tx.QueryRow(ctx, `
		SELECT mode='all_eligible' OR EXISTS (
			SELECT 1 FROM app_placement_hosts
			WHERE app_id=ap.app_id AND host_id=$2::uuid)
		FROM app_placement ap WHERE app_id=$1::uuid`, canonicalAppID, hostID).Scan(&selected)
	return selected, err
}

// GuardPlacementForSwap persists an unmanaged swap's pending detail only while
// the existing session's host remains selected for its target app. Managed
// swaps use GuardHomeForSwapWithHold, which performs the same check before its
// claim and durable hold writes.
func (s *Store) GuardPlacementForSwap(ctx context.Context, sessionID, canonicalAppID, hostID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var lockedHost, state string
	var detail *string
	if err := tx.QueryRow(ctx, `SELECT host_id::text,state,state_detail FROM sessions WHERE id=$1::uuid FOR UPDATE`, sessionID).
		Scan(&lockedHost, &state, &detail); err != nil {
		return fmt.Errorf("lock swap session: %w", err)
	}
	if lockedHost != hostID || state != "running" || (detail != nil && *detail == swapDetailInProgress) {
		return ErrSessionNotSwappable
	}
	var lockedApp string
	if err := tx.QueryRow(ctx, `SELECT id::text FROM apps WHERE id=$1::uuid FOR KEY SHARE`, canonicalAppID).Scan(&lockedApp); err != nil {
		return fmt.Errorf("lock swap app: %w", err)
	}
	selected, err := placementSelectedForHost(ctx, tx, canonicalAppID, hostID)
	if err != nil {
		return fmt.Errorf("lock swap placement: %w", err)
	}
	if !selected {
		return ErrNoHostAvailable
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state_detail=$2 WHERE id=$1::uuid`, sessionID, swapDetailInProgress); err != nil {
		return fmt.Errorf("persist pending swap: %w", err)
	}
	return tx.Commit(ctx)
}
