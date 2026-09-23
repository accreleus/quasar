package hostcfg

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func safeIdleErrorCode(code string) string {
	if code == "" {
		return ""
	}
	if len(code) > 64 {
		return "unknown_failure"
	}
	for _, char := range code {
		if char != '_' && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return "unknown_failure"
		}
	}
	return code
}

// IdleJournalState is a result from the authenticated current agent socket.
// Historical grant identities remain attached to their original offer.
type IdleJournalState struct {
	AttemptID, Group, Revision, Digest, GrantBoot, GrantConnection string
	Phase, Sequence, ErrorCode                                     string
	VerifiedAt                                                     *time.Time
}

// ObserveIdleState advances one durable attempt under the host lock. A lost
// accepted report can be reconstructed from the offered row; repeated or
// out-of-order reports never mark a group applied or release another owner.
func (s *Store) ObserveIdleState(ctx context.Context, hostID, connectionID string, state IdleJournalState) (bool, error) {
	seq, err := strconv.ParseInt(state.Sequence, 10, 64)
	if err != nil || seq < 0 || strconv.FormatInt(seq, 10) != state.Sequence || !validPolicyDigest(state.Digest) {
		return false, ErrApprovalSuperseded
	}
	terminal := state.Phase == "applied" || state.Phase == "recovered" || state.Phase == "uncertain" || state.Phase == "revoked_unstarted" || state.Phase == "failed" && seq == 0
	valid := map[string]bool{"accepted": true, "activating": true, "awaiting_startup": true,
		"verifying": true, "failed": true, "recovery_verifying": true,
		"recovery_awaiting_startup": true, "recovered": true, "uncertain": true,
		"applied": true, "revoked_unstarted": true}
	if !valid[state.Phase] || (seq == 0 && state.Phase != "failed" && state.Phase != "revoked_unstarted") ||
		(seq > 0 && state.Phase == "revoked_unstarted") {
		return false, ErrApprovalSuperseded
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var hostStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&hostStatus); err != nil {
		return false, err
	}
	var current *string
	if err := tx.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation WHERE host_id=$1::uuid`, hostID).Scan(&current); err != nil {
		return false, err
	}
	if current == nil || *current != connectionID {
		return false, ErrApprovalSuperseded
	}
	var group, digest, grantBoot, grantConnection, phase string
	var revision, oldSeq int64
	var started *time.Time
	err = tx.QueryRow(ctx, `SELECT group_key,approved_digest,approved_revision,boot_incarnation::text,
		grant_connection::text,phase,COALESCE(journal_sequence,-1),started_at
		FROM host_config_attempts WHERE host_id=$1::uuid AND id=$2::uuid FOR UPDATE`, hostID, state.AttemptID).
		Scan(&group, &digest, &revision, &grantBoot, &grantConnection, &phase, &oldSeq, &started)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrIdleAttemptNotFound
	}
	if err != nil {
		return false, err
	}
	if group != state.Group || digest != state.Digest || strconv.FormatInt(revision, 10) != state.Revision || grantBoot != state.GrantBoot || grantConnection != state.GrantConnection {
		return false, ErrApprovalSuperseded
	}
	if state.Phase == "failed" && seq == 0 && state.ErrorCode == "host_busy" {
		// The agent's final local check found work absent from the last DB
		// heartbeat. Keep the same protected grant eligible for bounded retry.
		return false, tx.Commit(ctx)
	}
	if seq < oldSeq {
		return false, tx.Commit(ctx)
	}
	if seq == oldSeq {
		if phase != state.Phase {
			return false, ErrApprovalSuperseded
		}
		return false, tx.Commit(ctx)
	}
	if phase == "applied" || phase == "recovered" || phase == "uncertain" || phase == "revoked_unstarted" {
		return false, ErrApprovalSuperseded
	}
	if started != nil && seq == 0 {
		return false, ErrApprovalSuperseded
	}
	if state.Phase == "applied" && state.VerifiedAt == nil {
		return false, ErrApprovalSuperseded
	}
	if _, err := tx.Exec(ctx, `UPDATE host_config_attempts SET phase=$3,journal_sequence=$4,
		started_at=CASE WHEN $4::bigint>0 THEN COALESCE(started_at,now()) ELSE started_at END,
		terminal_at=CASE WHEN $5 THEN now() ELSE terminal_at END,
		recovery_attempted=recovery_attempted OR $3 IN ('recovery_verifying','recovery_awaiting_startup','recovered','uncertain'),
		error_code=CASE WHEN $6='' THEN error_code ELSE $6 END
		WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, state.AttemptID, state.Phase, seq, terminal, safeIdleErrorCode(state.ErrorCode)); err != nil {
		return false, err
	}
	if seq > 0 {
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='accepted' WHERE host_id=$1::uuid AND id=$2::uuid AND state IN ('offered','cancel_pending')`, hostID, state.AttemptID); err != nil {
			return false, err
		}
	}
	if state.Phase == "applied" {
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET applied_revision=$3,applied_digest=$4,
			status='applied',evidence_connection=$5::uuid,evidence_at=$6
			WHERE host_id=$1::uuid AND group_key=$2 AND desired_revision=$3 AND desired_digest=$4`, hostID, state.Group, revision, state.Digest, connectionID, state.VerifiedAt); err != nil {
			return false, err
		}
	}
	if state.Phase == "recovered" || state.Phase == "uncertain" || state.Phase == "failed" {
		status := "failed"
		if state.Phase == "uncertain" {
			status = "uncertain"
		}
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status=$3 WHERE host_id=$1::uuid AND group_key=$2
			AND desired_revision=$4 AND desired_digest=$5`, hostID, state.Group, status, revision, state.Digest); err != nil {
			return false, err
		}
	}
	if state.Phase == "uncertain" {
		if _, err := tx.Exec(ctx, `UPDATE host_admission_restrictions SET owner_kind='recovery',reason='configuration_recovery'
			WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, state.AttemptID); err != nil {
			return false, err
		}
	} else if terminal {
		if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, state.AttemptID); err != nil {
			return false, err
		}
		if hostStatus == "draining" {
			if _, err := tx.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid AND NOT EXISTS
				(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID); err != nil {
				return false, err
			}
		}
	}
	if seq > 0 || terminal {
		if _, err := tx.Exec(ctx, `DELETE FROM host_reconcile_obligations WHERE host_id=$1::uuid
			AND kind='idle_apply' AND resource_key=$2`, hostID, state.AttemptID); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}
