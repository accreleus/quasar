package session

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// lockRequiredImageFence holds every managed image's cleanup fence sharing the
// selected exact ref through session reservation. A cleanup attempt takes its
// fence row FOR UPDATE.
// Apps with an unmanaged image have no fence and keep their existing launch
// behavior. This runs after placement locking and before the home claim.
func lockRequiredImageFence(ctx context.Context, tx pgx.Tx, hostID, imageRef string) (bool, error) {
	if imageRef == "" {
		return true, nil
	}
	rows, err := tx.Query(ctx, `SELECT image_id FROM installed_images
		WHERE registry_ref=$1 OR local_tag=$1 ORDER BY image_id`, imageRef)
	if err != nil {
		return false, fmt.Errorf("resolve managed image fence: %w", err)
	}
	var imageIDs []string
	for rows.Next() {
		var imageID string
		if err := rows.Scan(&imageID); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan managed image fence: %w", err)
		}
		imageIDs = append(imageIDs, imageID)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("list managed image fences: %w", err)
	}
	for _, imageID := range imageIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
			VALUES($1::uuid,$2,'idle') ON CONFLICT DO NOTHING`, hostID, imageID); err != nil {
			return false, fmt.Errorf("create managed image fence: %w", err)
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM host_image_operation_fences
			WHERE host_id=$1::uuid AND image_id=$2 FOR SHARE`, hostID, imageID).Scan(&state); err != nil {
			return false, fmt.Errorf("lock managed image fence: %w", err)
		}
		if state == "removing" {
			return false, nil
		}
	}
	return true, nil
}
