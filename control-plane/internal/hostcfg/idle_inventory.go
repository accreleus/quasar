package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ObserveIdleHeartbeat stores the agent's current-connection session list for
// operator wait reasons. A missing list is unknown, not an empty host.
func (s *Store) ObserveIdleHeartbeat(ctx context.Context, hostID, connectionID string, running []string) error {
	valid := len(running) <= 1024
	for _, id := range running {
		if id == "" || len(id) > 64 {
			valid = false
		}
	}
	var raw []byte
	var err error
	if valid && running != nil {
		raw, err = json.Marshal(running)
		if err != nil {
			return err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID); err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation
		WHERE host_id=$1::uuid AND state IN ('pending','complete')`, hostID).Scan(&current); err != nil {
		return err
	}
	if current == nil || *current != connectionID {
		return ErrApprovalSuperseded
	}
	if running == nil || !valid {
		if _, err := tx.Exec(ctx, `DELETE FROM host_idle_inventory WHERE host_id=$1::uuid`, hostID); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		if !valid {
			return ErrApprovalSuperseded
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_idle_inventory(host_id,connection_incarnation,running_sessions)
		VALUES($1::uuid,$2::uuid,$3::jsonb) ON CONFLICT(host_id) DO UPDATE SET
		connection_incarnation=excluded.connection_incarnation,running_sessions=excluded.running_sessions,
		reported_at=now()`, hostID, connectionID, raw); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BeginJournalReconciliation is called only for a newly authenticated RH05
// connection. It closes admission before any inventory page is considered.
func (s *Store) BeginJournalReconciliation(ctx context.Context, hostID, connectionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status, boot string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&boot); err != nil {
		return err
	}
	if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
		return err
	}
	// A same-boot waiting grant was never offered under the write-before-send
	// invariant. Fence it locally; an offered grant needs journal proof.
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions r USING host_config_approvals a
		WHERE r.host_id=$1::uuid AND r.host_id=a.host_id AND r.owner_kind='idle_apply'
		AND r.owner_id=a.id AND a.state='approved' AND a.boot_incarnation=$2::uuid`, hostID, boot); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='revoked_unstarted'
		WHERE host_id=$1::uuid AND state='approved' AND boot_incarnation=$2::uuid`, hostID, boot); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending'
		WHERE host_id=$1::uuid AND state='offered'`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_journal_reconciliation(host_id,boot_incarnation,connection_incarnation,state)
		VALUES($1::uuid,$2::uuid,$3::uuid,'pending') ON CONFLICT(host_id) DO UPDATE SET
		boot_incarnation=excluded.boot_incarnation,connection_incarnation=excluded.connection_incarnation,
		state='pending',completed_at=NULL,continuation_cursor=NULL`, hostID, boot, connectionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_idle_inventory WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		VALUES($1::uuid,'reconciliation','00000000-0000-0000-0000-000000000002'::uuid,'journal_reconciliation')
		ON CONFLICT(host_id,owner_kind,owner_id) DO UPDATE SET reason='journal_reconciliation'`, hostID); err != nil {
		return err
	}
	if status == "online" {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id=$1::uuid`, hostID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// EndJournalConnection closes the authority of the disconnected authenticated
// socket. A queued capacity write cannot recreate its hardware evidence after
// this transaction, even if it was decoded before the socket closed.
func (s *Store) EndJournalConnection(ctx context.Context, hostID, connectionID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SELECT id FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID); err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation
		WHERE host_id=$1::uuid FOR UPDATE`, hostID).Scan(&current); err != nil {
		return err
	}
	if current == nil || *current != connectionID {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE host_journal_reconciliation SET connection_incarnation=NULL,
		state='pending',completed_at=NULL,continuation_cursor=NULL WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_hardware_evidence WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_idle_inventory WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_journal_active_snapshots WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		VALUES($1::uuid,'reconciliation','00000000-0000-0000-0000-000000000002'::uuid,'journal_reconciliation')
		ON CONFLICT(host_id,owner_kind,owner_id) DO UPDATE SET reason='journal_reconciliation'`, hostID); err != nil {
		return err
	}
	if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending'
		WHERE host_id=$1::uuid AND state='offered'`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions r USING host_config_approvals a
		WHERE r.host_id=$1::uuid AND r.host_id=a.host_id AND r.owner_kind='idle_apply'
		AND r.owner_id=a.id AND a.state='approved'`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='superseded'
		WHERE host_id=$1::uuid AND state='approved'`, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CompleteJournalReconciliation may be called only after the websocket reader
// has validated the complete bounded-page snapshot on the current authenticated
// connection. A started restart entry is reconstructed only when its durable
// identity matches a surviving offer or attempt; unknown entries quarantine.
func (s *Store) CompleteJournalReconciliation(ctx context.Context, hostID, connectionID string, restartEntries []JournalInventoryEntry, snapshots ...map[string]PolicySnapshot) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var boot, status string
	var deliveryGate bool
	if err := tx.QueryRow(ctx, `SELECT status,config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status, &deliveryGate); err != nil {
		return err
	}
	if !deliveryGate {
		if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
			return err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&boot); err != nil {
		return err
	}
	var gateBoot, gateConnection, gateState string
	if err := tx.QueryRow(ctx, `SELECT boot_incarnation::text,connection_incarnation::text,state
		FROM host_journal_reconciliation WHERE host_id=$1::uuid FOR UPDATE`, hostID).Scan(&gateBoot, &gateConnection, &gateState); err != nil {
		return err
	}
	if gateBoot != boot || gateConnection != connectionID || gateState != "pending" {
		return ErrApprovalSuperseded
	}
	if deliveryGate {
		return tx.Commit(ctx)
	}
	quarantine := false
	seen := make(map[string]bool, len(restartEntries))
	for _, entry := range restartEntries {
		seen[entry.AttemptID] = true
		if entry.HostID != hostID || entry.Scope != "restart" || !validPolicyDigest(entry.Digest) || entry.AttemptID == "" || entry.Group == "" || entry.Phase == "" || entry.Sequence == "" {
			quarantine = true
			break
		}
		if ok, err := validateRestartInventoryEntry(ctx, tx, hostID, entry); err != nil {
			return err
		} else if !ok {
			quarantine = true
			break
		}
	}
	knownAccepted, err := tx.Query(ctx, `SELECT id::text FROM host_config_attempts
		WHERE host_id=$1::uuid AND scope='restart' AND started_at IS NOT NULL`, hostID)
	if err != nil {
		return err
	}
	for knownAccepted.Next() {
		var id string
		if err := knownAccepted.Scan(&id); err != nil {
			knownAccepted.Close()
			return err
		}
		if !seen[id] {
			quarantine = true
		}
	}
	err = knownAccepted.Err()
	knownAccepted.Close()
	if err != nil {
		return err
	}
	if quarantine {
		if _, err := tx.Exec(ctx, `UPDATE host_journal_reconciliation SET state='quarantined',completed_at=NULL WHERE host_id=$1::uuid`, hostID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_admission_restrictions SET reason='journal_quarantine'
			WHERE host_id=$1::uuid AND owner_kind='reconciliation'`, hostID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	pending, err := tx.Query(ctx, `SELECT id::text FROM host_config_approvals
		WHERE host_id=$1::uuid AND state='cancel_pending'`, hostID)
	if err != nil {
		return err
	}
	var absent []string
	for pending.Next() {
		var id string
		if err := pending.Scan(&id); err != nil {
			pending.Close()
			return err
		}
		if seen[id] {
			// Durable acceptance won the race with cancellation. Reconstruct it
			// below; cancellation cannot revoke execution after fsync.
		} else {
			absent = append(absent, id)
		}
	}
	err = pending.Err()
	pending.Close()
	if err != nil {
		return err
	}
	if quarantine {
		if _, err := tx.Exec(ctx, `UPDATE host_journal_reconciliation SET state='quarantined',completed_at=NULL WHERE host_id=$1::uuid`, hostID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_admission_restrictions SET reason='journal_quarantine'
			WHERE host_id=$1::uuid AND owner_kind='reconciliation'`, hostID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	for _, entry := range restartEntries {
		active := false
		if len(snapshots) != 0 {
			current := snapshots[0][entry.Group]
			active = current.Kind == "verified" && current.Digest == entry.Digest
		}
		if err := applyRestartInventoryEntry(ctx, tx, hostID, connectionID, entry, active); err != nil {
			return err
		}
	}
	// The active group projection becomes usable only in the same transaction
	// that completes this authenticated current-connection inventory.
	if _, err := tx.Exec(ctx, `DELETE FROM host_journal_active_snapshots WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if len(snapshots) != 0 {
		for group, snapshot := range snapshots[0] {
			if snapshot.Kind != "seeded" && snapshot.Kind != "verified" || !validPolicyDigest(snapshot.Digest) {
				return ErrApprovalSuperseded
			}
			if _, err := tx.Exec(ctx, `INSERT INTO host_journal_active_snapshots(host_id,group_key,connection_incarnation,kind,digest)
				VALUES($1::uuid,$2,$3::uuid,$4,$5)`, hostID, group, connectionID, snapshot.Kind, snapshot.Digest); err != nil {
				return err
			}
		}
	}
	for _, id := range absent {
		if _, err := tx.Exec(ctx, `DELETE FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_config_attempts SET phase='revoked_unstarted',terminal_at=now()
			WHERE host_id=$1::uuid AND id=$2::uuid AND phase='offered' AND started_at IS NULL`, hostID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='revoked_unstarted'
			WHERE host_id=$1::uuid AND id=$2::uuid AND state='cancel_pending'`, hostID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions
			WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, id); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE host_journal_reconciliation SET state='complete',completed_at=now(),continuation_cursor=NULL WHERE host_id=$1::uuid`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='reconciliation' AND owner_id='00000000-0000-0000-0000-000000000002'::uuid`, hostID); err != nil {
		return err
	}
	var held bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil {
		return err
	}
	if held && status == "online" {
		status = "draining"
	} else if !held && status == "draining" {
		status = "online"
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET status=$2 WHERE id=$1::uuid`, hostID, status); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type JournalInventoryEntry struct {
	AttemptID, HostID, Group, Digest, Scope, Phase, Sequence string
	Revision, GrantBoot, GrantConnection                     string
	ErrorCode                                                string
	VerifiedAt                                               *time.Time
}

func validateRestartInventoryEntry(ctx context.Context, tx pgx.Tx, hostID string, entry JournalInventoryEntry) (bool, error) {
	seq, err := strconv.ParseInt(entry.Sequence, 10, 64)
	allowed := map[string]bool{"accepted": true, "activating": true, "awaiting_startup": true, "verifying": true,
		"failed": true, "recovery_verifying": true, "recovery_awaiting_startup": true, "applied": true,
		"recovered": true, "uncertain": true}
	if err != nil || seq < 0 || strconv.FormatInt(seq, 10) != entry.Sequence ||
		!allowed[entry.Phase] || seq == 0 && entry.Phase != "failed" ||
		(entry.Phase == "applied" && entry.VerifiedAt == nil) ||
		entry.Revision == "" || entry.GrantBoot == "" || entry.GrantConnection == "" {
		return false, nil
	}
	revision, err := strconv.ParseInt(entry.Revision, 10, 64)
	if err != nil || revision < 0 || strconv.FormatInt(revision, 10) != entry.Revision {
		return false, nil
	}
	var group, digest, grantBoot string
	var grantConnection *string
	var oldRevision, oldSeq int64
	var phase string
	err = tx.QueryRow(ctx, `SELECT group_key,approved_digest,approved_revision,boot_incarnation::text,
		grant_connection::text,phase,COALESCE(journal_sequence,-1) FROM host_config_attempts
		WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, entry.AttemptID).
		Scan(&group, &digest, &oldRevision, &grantBoot, &grantConnection, &phase, &oldSeq)
	if err == nil {
		return group == entry.Group && digest == entry.Digest && oldRevision == revision && grantBoot == entry.GrantBoot &&
			grantConnection != nil && *grantConnection == entry.GrantConnection &&
			seq >= oldSeq && (seq != oldSeq || phase == entry.Phase), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_config_attempts
		WHERE host_id=$1::uuid AND scope='restart' AND id<>$2::uuid AND terminal_at IS NULL)`, hostID, entry.AttemptID).Scan(&conflicting); err != nil {
		return false, err
	}
	if conflicting {
		return false, nil
	}
	var approvalGroup, approvalDigest, approvalBoot, approvalState string
	var approvalRevision int64
	err = tx.QueryRow(ctx, `SELECT group_key,approved_digest,revision,boot_incarnation::text,state
		FROM host_config_approvals WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, entry.AttemptID).
		Scan(&approvalGroup, &approvalDigest, &approvalRevision, &approvalBoot, &approvalState)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return approvalGroup == entry.Group && approvalDigest == entry.Digest && approvalRevision == revision &&
		approvalBoot == entry.GrantBoot && (approvalState == "offered" || approvalState == "cancel_pending" || approvalState == "accepted") && seq > 0, nil
}

func applyRestartInventoryEntry(ctx context.Context, tx pgx.Tx, hostID, connectionID string, entry JournalInventoryEntry, active bool) error {
	seq, _ := strconv.ParseInt(entry.Sequence, 10, 64)
	revision, _ := strconv.ParseInt(entry.Revision, 10, 64)
	terminal := entry.Phase == "applied" || entry.Phase == "recovered" || entry.Phase == "uncertain" ||
		entry.Phase == "revoked_unstarted" || entry.Phase == "failed" && seq == 0
	if _, err := tx.Exec(ctx, `INSERT INTO host_config_attempts
		(id,host_id,group_key,approved_digest,approved_revision,scope,boot_incarnation,grant_connection,phase,journal_sequence,started_at,terminal_at,recovery_attempted,error_code)
		VALUES($1::uuid,$2::uuid,$3,$4,$5,'restart',$6::uuid,$7::uuid,$8,$9,
			CASE WHEN $9::bigint>0 THEN now() ELSE NULL END,CASE WHEN $10::boolean THEN now() ELSE NULL END,
			$8 IN ('recovery_verifying','recovery_awaiting_startup','recovered','uncertain'),NULLIF($11::text,''))
		ON CONFLICT(id) DO UPDATE SET phase=excluded.phase,journal_sequence=excluded.journal_sequence,
			started_at=COALESCE(host_config_attempts.started_at,excluded.started_at),
			terminal_at=COALESCE(host_config_attempts.terminal_at,excluded.terminal_at),
			recovery_attempted=host_config_attempts.recovery_attempted OR excluded.recovery_attempted,
			error_code=COALESCE(excluded.error_code,host_config_attempts.error_code)
		WHERE host_config_attempts.journal_sequence IS NULL OR host_config_attempts.journal_sequence<$9::bigint`,
		entry.AttemptID, hostID, entry.Group, entry.Digest, revision, entry.GrantBoot, entry.GrantConnection, entry.Phase, seq, terminal, safeIdleErrorCode(entry.ErrorCode)); err != nil {
		return err
	}
	if seq > 0 {
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='accepted' WHERE host_id=$1::uuid AND id=$2::uuid AND state IN ('offered','cancel_pending')`, hostID, entry.AttemptID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, entry.AttemptID); err != nil {
			return err
		}
	}
	if entry.Phase == "applied" && active {
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET applied_revision=$3,applied_digest=$4,status='applied',
			evidence_connection=$5::uuid,evidence_at=$6 WHERE host_id=$1::uuid AND group_key=$2
			AND desired_revision=$3 AND desired_digest=$4`, hostID, entry.Group, revision, entry.Digest, connectionID, entry.VerifiedAt); err != nil {
			return err
		}
	}
	if entry.Phase == "recovered" || entry.Phase == "uncertain" || entry.Phase == "failed" {
		status := "failed"
		if entry.Phase == "uncertain" {
			status = "uncertain"
		}
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status=$3 WHERE host_id=$1::uuid AND group_key=$2
			AND desired_revision=$4 AND desired_digest=$5`, hostID, entry.Group, status, revision, entry.Digest); err != nil {
			return err
		}
	}
	if entry.Phase == "uncertain" {
		if _, err := tx.Exec(ctx, `UPDATE host_admission_restrictions SET owner_kind='recovery',reason='configuration_recovery'
			WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, entry.AttemptID); err != nil {
			return err
		}
	} else if terminal {
		if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, entry.AttemptID); err != nil {
			return err
		}
	}
	return nil
}

// JournalGate protects readers that cannot distinguish a missing inventory
// page from an empty journal.
func (s *Store) JournalGate(ctx context.Context, hostID string) (string, error) {
	var state string
	err := s.pool.QueryRow(ctx, `SELECT state FROM host_journal_reconciliation WHERE host_id=$1::uuid`, hostID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "pending", nil
	}
	if err != nil {
		return "", fmt.Errorf("journal gate: %w", err)
	}
	return state, nil
}
