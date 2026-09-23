package hostcfg

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// NextIdleOffer atomically freezes one reviewed candidate and its current
// prerequisites before the transport sees it. The host lock is the same lock
// admission, policy edits and inventory updates use. A socket send never marks
// the attempt started or the group applied.
func (s *Store) NextIdleOffer(ctx context.Context, hostID, bootID, connectionID string) (*PolicyOffer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	// Serialize an offer with the jobs dispatcher. A future cleanup may become
	// due (or be pulled forward) immediately after this admission check.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::uuid::text,339))`, hostID); err != nil {
		return nil, err
	}
	var hostStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&hostStatus); err != nil {
		return nil, err
	}
	if hostStatus == "offline" {
		return nil, nil
	}
	var currentBoot, gateConnection, gateState string
	var gateBoot string
	if err := tx.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&currentBoot); err != nil {
		return nil, err
	}
	if currentBoot != bootID {
		return nil, nil
	}
	if err := tx.QueryRow(ctx, `SELECT boot_incarnation::text,connection_incarnation::text,state
		FROM host_journal_reconciliation WHERE host_id=$1::uuid FOR UPDATE`, hostID).
		Scan(&gateBoot, &gateConnection, &gateState); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if gateBoot != bootID || gateConnection != connectionID || gateState != "complete" {
		return nil, nil
	}
	var approvalID, group, digest, prereqDigest, approvalBoot, reviewID, approvalState string
	var revision int64
	var expiry time.Time
	var retryCount *int
	var retryAt *time.Time
	err = tx.QueryRow(ctx, `SELECT a.id::text,a.group_key,a.revision,a.approved_digest,a.prerequisites_digest,
		a.boot_incarnation::text,a.review_id::text,a.expires_at,a.state,o.retry_count,o.next_attempt_at
		FROM host_config_approvals a LEFT JOIN host_reconcile_obligations o
		ON o.host_id=a.host_id AND o.kind='idle_apply' AND o.resource_key=a.id::text
		WHERE a.host_id=$1::uuid AND a.state IN ('approved','offered')
		ORDER BY a.created_at LIMIT 1 FOR UPDATE OF a`, hostID).
		Scan(&approvalID, &group, &revision, &digest, &prereqDigest, &approvalBoot, &reviewID, &expiry,
			&approvalState, &retryCount, &retryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if approvalBoot != bootID {
		return nil, nil
	}
	if !expiry.After(time.Now().UTC()) {
		// This approval has never been offered, so the write-before-send rule
		// proves there is no agent execution to reconcile. Expire only its own
		// hold; an offered attempt still needs authenticated nonacceptance.
		if approvalState == "approved" {
			if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='expired'
				WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, approvalID); err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions
				WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, approvalID); err != nil {
				return nil, err
			}
			var held bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions
				WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil {
				return nil, err
			}
			if !held && hostStatus == "draining" {
				if _, err := tx.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid`, hostID); err != nil {
					return nil, err
				}
			}
		}
		return nil, tx.Commit(ctx)
	}
	if approvalState == "offered" {
		if retryCount == nil || retryAt == nil || *retryCount >= 3 || retryAt.After(time.Now().UTC()) {
			return nil, nil
		}
		var phase string
		var started *time.Time
		if err := tx.QueryRow(ctx, `SELECT phase,started_at FROM host_config_attempts
			WHERE host_id=$1::uuid AND id=$2::uuid FOR UPDATE`, hostID, approvalID).Scan(&phase, &started); err != nil {
			return nil, err
		}
		if phase != "offered" || started != nil {
			return nil, nil
		}
	}
	ready, err := idleExecutionReady(ctx, tx, hostID, connectionID)
	if err != nil || !ready {
		return nil, err
	}
	preview, err := s.previewIdleApplyExcept(ctx, tx, hostID, group, approvalID)
	if err != nil {
		return nil, err
	}
	if preview == nil || !preview.Available || preview.Revision != strconv.FormatInt(revision, 10) ||
		preview.ContentSHA256 != digest || preview.PrerequisitesSHA256 != prereqDigest ||
		preview.ApprovalReviewID != reviewID || preview.ApprovalBootIncarnation != bootID {
		return nil, nil
	}
	choices, err := loadPolicyChoices(ctx, tx, hostID)
	if err != nil {
		return nil, err
	}
	settings := make(map[string]PolicyChoice)
	for _, key := range PolicyGroupKeys(group) {
		choice, ok := choices[key]
		if !ok {
			choice = PolicyChoice{Source: "deployment"}
		}
		settings[key] = choice
	}
	facts := make([]any, len(preview.Prerequisites))
	for i, fact := range preview.Prerequisites {
		facts[i] = map[string]any{"kind": fact.Kind, "id": fact.ID}
	}
	grantExpiry := time.Now().UTC().Add(30 * time.Second)
	if expiry.Before(grantExpiry) {
		grantExpiry = expiry
	}
	offer := &PolicyOffer{Type: "config_policy_offer", AttemptID: approvalID,
		HostID: hostID, BootIncarnation: bootID, ConnectionIncarnation: connectionID,
		Group: group, Revision: preview.Revision, ContentSHA256: digest, Scope: "restart",
		ExpiresAt: grantExpiry, PrerequisitesSHA256: prereqDigest, Prerequisites: facts,
		Settings: settings, ResolvedSettings: preview.Resolved}
	if approvalState == "approved" {
		if _, err := tx.Exec(ctx, `INSERT INTO host_config_attempts
		(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,grant_connection,phase)
		VALUES($1::uuid,$2::uuid,$3,$4,$5,'restart',$6::uuid,$7::uuid,'offered')`,
			approvalID, hostID, group, digest, revision, bootID, connectionID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='offered'
		WHERE id=$1::uuid AND host_id=$2::uuid AND state='approved'`, approvalID, hostID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations
		(host_id,kind,resource_key,revision,next_attempt_at,retry_count)
		VALUES($1::uuid,'idle_apply',$2,$3,now()+interval '10 seconds',0)`, hostID, approvalID, revision); err != nil {
			return nil, err
		}
	} else {
		// The durable attempt is unchanged. A duplicate grant can only return
		// that attempt's recorded result, and the retry budget is persisted.
		if _, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations
			SET retry_count=retry_count+1,
			next_attempt_at=now()+interval '10 seconds' * (1 << LEAST(retry_count+1,3))
			WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, approvalID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return offer, nil
}

// idleExecutionReady is the final database-side check under the host lock.
// Any missing or stale authenticated inventory is unknown, never an empty host.
func idleExecutionReady(ctx context.Context, tx pgx.Tx, hostID, connectionID string) (bool, error) {
	var ready bool
	err := tx.QueryRow(ctx, `SELECT
		COALESCE(i.connection_incarnation=$2::uuid AND i.reported_at>=h.last_registered_at
			AND i.reported_at>=clock_timestamp()-interval '30 seconds'
			AND jsonb_array_length(i.running_sessions)=0,false)
		AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.host_id=h.id
			AND s.state IN ('assigned','starting','running','stopping'))
		AND COALESCE(h.source_preparation_reported_at>=h.last_registered_at
			AND h.source_preparation_reported_at>=clock_timestamp()-interval '30 seconds'
			AND jsonb_typeof(h.source_preparation->'steam'->'images')='array',false)
		AND NOT EXISTS(SELECT 1 FROM jsonb_array_elements(CASE
			WHEN jsonb_typeof(h.source_preparation->'steam'->'images')='array'
			THEN h.source_preparation->'steam'->'images' ELSE '[]'::jsonb END) image
			WHERE image->>'state' IN ('queued','preparing','waiting_image','deferred','failed'))
		AND NOT EXISTS(SELECT 1 FROM job_runs r JOIN jobs j ON j.id=r.job_id
			WHERE r.host_id=h.id AND (r.state='running' OR
				(r.state='pending' AND r.scheduled_for<=clock_timestamp() AND j.enabled AND j.managed)))
		FROM hosts h LEFT JOIN host_idle_inventory i ON i.host_id=h.id WHERE h.id=$1::uuid`,
		hostID, connectionID).Scan(&ready)
	return ready, err
}
