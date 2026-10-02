package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SteamPublishPermit records the final selected-requirement check for this
// exact agent claim. The conditional UPDATE is one READ COMMITTED snapshot:
// a placement removal before it denies; a later removal is later work.
func (s *Store) SteamPublishPermit(ctx context.Context, runID, hostID, token, imageID, registryRef, version, revision string, claimTimeout time.Duration) (bool, error) {
	var accepted bool
	err := s.pool.QueryRow(ctx, `UPDATE job_runs r
	 SET publish_permit_accepted_at=statement_timestamp()
	 WHERE r.id=$1::uuid AND r.host_id=$2::uuid AND r.job_id='template.warmup'
	 AND r.state='running' AND r.template_publish_claim_token=$3::uuid
	 AND r.claimed_at IS NOT NULL
	 AND r.claimed_at > statement_timestamp()-make_interval(secs => $8::double precision)
	 AND r.params->>'image_id'=$4 AND r.params->>'registry_ref'=$5
	 AND r.params->>'version'=$6 AND r.params->>'policy_revision'=$7
	 AND EXISTS (SELECT 1 FROM hosts h
	   JOIN host_images hi ON hi.host_id=h.id
	   JOIN instance_settings s ON s.id=true
	   WHERE h.id=r.host_id AND h.status='online' AND h.agent_disconnected_at IS NULL
	   AND h.source_policy_versions->>'template_publish_permit'='1'
	   AND h.source_preparation_connection_id=r.template_publish_connection_id
	   AND h.source_preparation_connection_id IS NOT NULL
	   AND s.steam_preparation_enabled
	   AND s.steam_preparation_revision::text=$7
	   AND s.steam_preparation_image->>'image_id'=$4
	   AND s.steam_preparation_image->>'registry_ref'=$5
	   AND s.steam_preparation_image->>'version'=$6
	   AND h.source_preparation->'steam'->>'policy_revision'=$7
	   AND EXISTS (SELECT 1 FROM jsonb_array_elements(h.source_preparation->'steam'->'images') rep
	     WHERE rep->>'image_id'=$4 AND rep->>'registry_ref'=$5
	     AND rep->>'version'=$6 AND rep->>'preparation_enabled'='true')
	   AND hi.image_id=$4 AND hi.version=$6 AND hi.state='ready'
	   AND EXISTS (SELECT 1 FROM apps a
	     JOIN apps effective ON effective.id=COALESCE(a.parent_app_id,a.id)
	     JOIN app_placement ap ON ap.app_id=effective.id
	     LEFT JOIN runtime_presets rp ON rp.id=effective.runtime_preset_id
	     WHERE a.enabled AND effective.enabled
	     AND (CASE WHEN jsonb_typeof(effective.runtime_spec->'image')='string'
	       AND effective.runtime_spec->>'image'<>''
	       THEN effective.runtime_spec->>'image' ELSE NULLIF(rp.image,'') END)=$5
	     AND (ap.mode='all_eligible' OR EXISTS (SELECT 1 FROM app_placement_hosts aph
	       WHERE aph.app_id=ap.app_id AND aph.host_id=h.id))))
	 RETURNING true`, runID, hostID, token, imageID, registryRef, version, revision, claimTimeout.Seconds()).Scan(&accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check Steam template publication: %w", err)
	}
	return accepted, nil
}
