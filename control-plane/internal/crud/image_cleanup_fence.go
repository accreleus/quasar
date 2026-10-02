package crud

import (
	"context"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// imageRefForApp reads the same effective image as placement/launch. Called
// while the app writer's transaction owns its row.
func imageRefForApp(ctx context.Context, tx pgx.Tx, appID string) (string, error) {
	var ref *string
	err := tx.QueryRow(ctx, `SELECT COALESCE(NULLIF(effective.runtime_spec->>'image',''),NULLIF(rp.image,''))
		FROM apps a JOIN apps effective ON effective.id=COALESCE(a.parent_app_id,a.id)
		LEFT JOIN runtime_presets rp ON rp.id=effective.runtime_preset_id
		WHERE a.id=$1::uuid`, appID).Scan(&ref)
	if err != nil {
		return "", err
	}
	if ref == nil {
		return "", nil
	}
	return *ref, nil
}

// fenceImageRequirementWrite uses a ref-specific transaction advisory lock.
// Cleanup takes the same lock before creating its fence, including a catalog-
// pruned version known only to a current agent. Sorted refs avoid cycles when
// an edit changes the selected image. It bumps only durable fences associated
// with those refs; unrelated previews keep their generations.
func fenceImageRequirementWrite(ctx context.Context, tx pgx.Tx, refs ...string) error {
	seen := map[string]bool{}
	var unique []string
	for _, ref := range refs {
		if ref != "" && !seen[ref] {
			seen[ref] = true
			unique = append(unique, ref)
		}
	}
	sort.Strings(unique)
	for _, ref := range unique {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4,hashtext($1::text))`, ref); err != nil {
			return fmt.Errorf("serialize image requirement ref: %w", err)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT f.host_id::text,f.image_id FROM host_image_operation_fences f
		WHERE f.image_id IN (
			SELECT ii.image_id FROM installed_images ii WHERE ii.registry_ref=ANY($1::text[]) OR ii.local_tag=ANY($1::text[])
			UNION SELECT a.image_id FROM host_image_cleanup_attempts a WHERE a.image_ref=ANY($1::text[])
			UNION SELECT h.image_id FROM host_image_success_history h
			WHERE h.current_identity->>'registry_ref'=ANY($1::text[]) OR h.current_identity->>'local_tag'=ANY($1::text[])
			OR h.previous_identity->>'registry_ref'=ANY($1::text[]) OR h.previous_identity->>'local_tag'=ANY($1::text[])
		) ORDER BY f.host_id,f.image_id FOR UPDATE`, unique)
	if err != nil {
		return fmt.Errorf("lock image cleanup fences: %w", err)
	}
	type key struct{ hostID, imageID string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.hostID, &k.imageID); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("read image cleanup fences: %w", err)
	}
	for _, k := range keys {
		if _, err := tx.Exec(ctx, `UPDATE host_image_operation_fences SET generation=generation+1
			WHERE host_id=$1::uuid AND image_id=$2`, k.hostID, k.imageID); err != nil {
			return fmt.Errorf("advance image cleanup generation: %w", err)
		}
	}
	return nil
}
