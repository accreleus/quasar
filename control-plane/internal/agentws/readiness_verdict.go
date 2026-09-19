package agentws

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/readiness"
)

// recomputeReadinessVerdict re-derives one host's scheduling columns from its
// stored readiness report and its overrides, inside the caller's transaction.
//
// It must run in the same transaction as every write of hosts.readiness and
// after every GPU-set write, or the derived columns describe a state older than
// the report: a GPU row inserted after the report naming it would start
// unblocked and stay schedulable until the next report.
//
// Lock order is the host row, then that host's gpus rows — the order
// upsertCapacityWithDetection has always taken, so this adds no new edge to the
// lock graph. The gpus UPDATE touches only rows whose value actually changes,
// so a steady-state report takes no GPU row lock at all.
//
// A gpu_index the host does not have is ignored by construction (nothing
// matches it), and a GPU the report no longer names is cleared by the same
// statement.
//
// #263 adds the lapse deletion and its audit rows here, between the read and
// the writes, and calls this from the override handlers.
func recomputeReadinessVerdict(ctx context.Context, tx pgx.Tx, hostID string) error {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT readiness FROM hosts WHERE id = $1 FOR UPDATE`, hostID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the host was deleted under us; nothing to derive
	}
	if err != nil {
		return fmt.Errorf("lock host for readiness verdict: %w", err)
	}

	rows, err := tx.Query(ctx, `SELECT check_id FROM host_readiness_overrides WHERE host_id = $1`, hostID)
	if err != nil {
		return fmt.Errorf("read readiness overrides: %w", err)
	}
	var overrides []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan readiness override: %w", err)
		}
		overrides = append(overrides, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate readiness overrides: %w", err)
	}

	v := readiness.Evaluate(raw, overrides)
	if _, err := tx.Exec(ctx,
		`UPDATE hosts SET readiness_block_host = $2, readiness_block_homes = $3 WHERE id = $1`,
		hostID, v.BlockHost, v.BlockHomes); err != nil {
		return fmt.Errorf("write readiness host verdict: %w", err)
	}

	// Never nil: pgx sends a nil slice as SQL NULL, and `index = ANY(NULL)` is
	// NULL, which would leave every row's IS DISTINCT FROM unresolvable.
	blocked := v.BlockedGPUs
	if blocked == nil {
		blocked = []int{}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE gpus SET readiness_blocked = (index = ANY($2::int[]))
		WHERE host_id = $1 AND readiness_blocked IS DISTINCT FROM (index = ANY($2::int[]))
	`, hostID, blocked); err != nil {
		return fmt.Errorf("write readiness gpu verdict: %w", err)
	}
	return nil
}
