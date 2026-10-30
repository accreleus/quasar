package hostcfg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/accreleus/quasar/control-plane/internal/admission"
)

var ErrRecoveryHoldNotFound = errors.New("no recovery hold for this attempt")
var ErrJournalNotReconciled = errors.New("host journal reconciliation is not complete")

// ReleaseRecoveryHold is the explicit repair of an uncertain attempt's recovery
// hold (agent-api.md idle operation, #656). It refuses while the journal gate is
// not complete, so a release never races a reconciliation that could re-plant
// or prove the hold. connected is the live websocket observation, read as in
// admission.Store.Release. Returns the host's resulting status.
func (s *Store) ReleaseRecoveryHold(ctx context.Context, hostID, attemptID string, connected bool) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	var live bool
	if err := tx.QueryRow(ctx, `SELECT status, `+admission.HostIsLiveSQL+` FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status, &live); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrHostNotFound
		}
		return "", err
	}
	var gate string
	if err := tx.QueryRow(ctx, `SELECT state FROM host_journal_reconciliation WHERE host_id=$1::uuid FOR UPDATE`, hostID).Scan(&gate); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var group, digest string
	var revision int64
	err = tx.QueryRow(ctx, `UPDATE host_config_attempts t SET recovery_released_at=now()
		FROM host_admission_restrictions r
		WHERE t.host_id=$1::uuid AND t.id=$2::uuid AND t.phase='uncertain'
		AND r.host_id=t.host_id AND r.owner_kind='recovery' AND r.owner_id=t.id
		RETURNING t.group_key,t.approved_digest,t.approved_revision`, hostID, attemptID).Scan(&group, &digest, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRecoveryHoldNotFound
	}
	if err != nil {
		return "", err
	}
	if gate != "complete" {
		return "", ErrJournalNotReconciled
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='recovery' AND owner_id=$2::uuid`, hostID, attemptID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='failed' WHERE host_id=$1::uuid AND group_key=$2
		AND status='uncertain' AND desired_revision=$3 AND desired_digest=$4`, hostID, group, revision, digest); err != nil {
		return "", err
	}
	if err := tx.QueryRow(ctx, `UPDATE hosts SET status=CASE WHEN status<>'draining' OR EXISTS(
			SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid) THEN status
		WHEN $2 THEN 'online' ELSE 'offline' END
		WHERE id=$1::uuid RETURNING status`, hostID, connected && live).Scan(&status); err != nil {
		return "", err
	}
	return status, tx.Commit(ctx)
}
