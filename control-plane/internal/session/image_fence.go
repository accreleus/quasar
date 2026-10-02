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
	// Serialize a launch with a cleanup POST even when the catalog/adoption
	// rows were pruned. POST and requirement writers take this same ref lock
	// before the image fence row, so no new removing fence can appear after a
	// launch has concluded that there is no managed mapping.
	// Shared mode lets independent launches of the same app proceed together;
	// cleanup and requirement writers take exclusive mode.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(4,hashtext($1::text))`, imageRef); err != nil {
		return false, fmt.Errorf("lock managed image ref: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT image_id FROM (
		SELECT image_id FROM installed_images WHERE registry_ref=$1 OR local_tag=$1
		UNION ALL SELECT image_id FROM host_image_cleanup_attempts
		WHERE host_id=$2::uuid AND image_ref=$1
		UNION ALL SELECT image_id FROM host_image_success_history
		WHERE host_id=$2::uuid AND (
			COALESCE(NULLIF(current_identity->>'registry_ref',''),NULLIF(current_identity->>'local_tag',''))=$1
			OR COALESCE(NULLIF(previous_identity->>'registry_ref',''),NULLIF(previous_identity->>'local_tag',''))=$1
		)
	) managed ORDER BY image_id`, imageRef, hostID)
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
	// The scheduler's first readiness read can precede a terminal cleanup
	// transaction. Re-read after acquiring the fence: that transaction may
	// have released the fence and demoted the exact ready row while we waited.
	// A lazy on-demand digest cannot have a ready row before its first
	// launch; this renders the same exception as placement's imageReadySQL.
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(
		SELECT 1 FROM installed_images ii WHERE (ii.registry_ref=$1 OR ii.local_tag=$1)
		`+lazyOnDemandAdmissionSQL("$2::uuid", "$1")+`
		AND NOT EXISTS(SELECT 1 FROM host_images hi WHERE hi.host_id=$2::uuid AND hi.image_id=ii.image_id
			AND hi.state='ready' AND (hi.version='' OR hi.version=ii.version))
	)`, imageRef, hostID).Scan(&ready); err != nil {
		return false, fmt.Errorf("recheck managed image readiness: %w", err)
	}
	var removedUnready bool
	if err := tx.QueryRow(ctx, `SELECT `+removedManagedImageUnreadySQL("$2::uuid", "$1"), imageRef, hostID).Scan(&removedUnready); err != nil {
		return false, fmt.Errorf("recheck removed managed image: %w", err)
	}
	return ready && !removedUnready, nil
}
