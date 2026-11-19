package session

import (
	"context"
	"errors"
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
// the existing session's host remains selected for its target app and, for a
// derived Steam game, the session mounted the game's mapped tool. Managed
// swaps use GuardHomeForSwapWithHold, which performs the same check before its
// claim and durable hold writes.
func (s *Store) GuardPlacementForSwap(ctx context.Context, sessionID string, app LaunchApp, hostID string) error {
	canonicalAppID := homeAppID(app)
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
	if err := compatToolNotMounted(ctx, tx, sessionID, app); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET state_detail=$2, launch_progress=NULL WHERE id=$1::uuid`, sessionID, swapDetailInProgress); err != nil {
		return fmt.Errorf("persist pending swap: %w", err)
	}
	return tx.Commit(ctx)
}

// compatToolNotMounted refuses a swap into a derived Steam game whose per-game GE mapping names a
// tool this session did not mount at launch, or one the session started before. A swap sends no
// compat block and the agent carries the running container's launch-time table
// (compat_tools::carry), so what decides whether the tool applies is sessions.compat_tool_names,
// never the host's report now (the same question claimToolMissing asks of a parked Steam). A
// mapping committed after the launch is in no table that session carries, even when its tool is
// mounted for another game (#1427), so the swap is refused and the game is launched on its own
// instead. The comparison is commit order (mapping revision against the revision the launch read),
// never updated_at, which is now() at the mapping transaction's START and can predate the session
// when that transaction waits on mappingCapLock (#1555). A running session with NULL names mounts
// nothing, and one with no recorded revision carries no mapping. Runs in the guard transaction
// after its placement and home checks and before any claim, hold or write.
// semantics: control-api.md §Compatibility tools, "Placement".
func compatToolNotMounted(ctx context.Context, q rowQuerier, sessionID string, app LaunchApp) error {
	if !app.IsDerived() || app.ExternalSource != "steam" {
		return nil
	}
	var tag string
	var usable, mounted, carried bool
	err := q.QueryRow(ctx, `
		SELECT t.tag, t.enabled AND NOT t.name_conflict AND NOT t.pin_conflict,
		       COALESCE(t.steam_name = ANY(k.compat_tool_names), false),
		       COALESCE(m.revision <= k.compat_mapping_revision, false)
		FROM sessions k, compat_tool_mappings m JOIN compat_tools t ON t.id = m.tool_id
		WHERE k.id = $1::uuid AND m.provider = 'steam' AND m.external_id = $2 AND t.family = 'ge'`,
		sessionID, app.ExternalID).Scan(&tag, &usable, &mounted, &carried)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("swap compat tool check: %w", err)
	case carried && mounted:
		return nil
	}
	return &CompatToolRejection{err: ErrNoHostAvailable, Tool: tag, Usable: usable, Swap: true}
}
