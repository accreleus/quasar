package hostcfg

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// rotateHostReviewTokens is called under the host-row lock for a disruptive
// lifecycle transition. Lock order is deterministic across restart groups.
// The migration trigger records every issued ID and retries collisions.
func rotateHostReviewTokens(ctx context.Context, tx pgx.Tx, hostID string) error {
	rows, err := tx.Query(ctx, `SELECT group_key FROM host_approval_review_tokens
		WHERE host_id=$1::uuid ORDER BY group_key FOR UPDATE`, hostID)
	if err != nil {
		return err
	}
	var groups []string
	for rows.Next() {
		var group string
		if err := rows.Scan(&group); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, group := range groups {
		if _, err := tx.Exec(ctx, `UPDATE host_approval_review_tokens SET review_id=gen_random_uuid()
			WHERE host_id=$1::uuid AND group_key=$2`, hostID, group); err != nil {
			return err
		}
	}
	return nil
}
