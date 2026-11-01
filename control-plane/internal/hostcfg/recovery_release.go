package hostcfg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrAttemptNotUncertain = errors.New("attempt is not uncertain")

// RecoveryReleaseCmd mirrors agent-api.md `config_policy_release` on the wire.
type RecoveryReleaseCmd struct {
	Type                  string `json:"type"` // "config_policy_release"
	AttemptID             string `json:"attempt_id"`
	BootIncarnation       string `json:"boot_incarnation"`
	ConnectionIncarnation string `json:"connection_incarnation"`
}

func NewRecoveryReleaseCmd(attemptID, boot, connection string) RecoveryReleaseCmd {
	return RecoveryReleaseCmd{Type: "config_policy_release", AttemptID: attemptID, BootIncarnation: boot, ConnectionIncarnation: connection}
}

// RecoveryRelease is the administrator's release of an uncertain attempt as the
// attempt view reports it (control-api.md "Releasing a recovery hold").
type RecoveryRelease struct {
	State       string     `json:"state"` // pending | released
	RequestedAt *time.Time `json:"requested_at"`
	// agent_connection | agent_upgrade | agent_report while pending; null once released.
	WaitingFor *string `json:"waiting_for"`
}

// projectHostStatusSQL makes hosts.status follow the restriction set as
// admission.Release does: any restriction keeps an online host draining, and a
// draining host with none left is online only while no disconnect is recorded.
const projectHostStatusSQL = `UPDATE hosts SET status=CASE
	WHEN EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=hosts.id)
		THEN CASE WHEN status='online' THEN 'draining' ELSE status END
	WHEN status='draining' THEN ` + releasedStatusSQL + ` ELSE status END WHERE id=$1::uuid`

// RequestRecoveryRelease durably records an administrator's release of an
// uncertain restart attempt. It lifts nothing: the hold, the group and the host
// status move only when the agent reports the attempt `released`. first is true
// only for the request that set the marker. A released attempt answers as done.
func (s *Store) RequestRecoveryRelease(ctx context.Context, hostID, attemptID string) (first bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var phase string
	err = tx.QueryRow(ctx, `SELECT phase FROM host_config_attempts
		WHERE host_id=$1::uuid AND id=$2::uuid AND scope='restart' FOR UPDATE`, hostID, attemptID).Scan(&phase)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrIdleAttemptNotFound
	}
	if err != nil {
		return false, err
	}
	switch phase {
	case "released":
		return false, nil
	case "uncertain":
	default:
		return false, ErrAttemptNotUncertain
	}
	set, err := tx.Exec(ctx, `UPDATE host_config_attempts SET release_requested_at=now()
		WHERE host_id=$1::uuid AND id=$2::uuid AND release_requested_at IS NULL`, hostID, attemptID)
	if err != nil {
		return false, err
	}
	return set.RowsAffected() == 1, tx.Commit(ctx)
}

// PendingRecoveryReleases lists the requested releases still uncertain, and only
// once connectionID's journal is reconciled: a release is never sent before then.
func (s *Store) PendingRecoveryReleases(ctx context.Context, hostID, connectionID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT t.id::text FROM host_config_attempts t
		JOIN host_journal_reconciliation g ON g.host_id=t.host_id
		WHERE t.host_id=$1::uuid AND t.scope='restart' AND t.phase='uncertain' AND t.release_requested_at IS NOT NULL
		AND g.state='complete' AND g.connection_incarnation=$2::uuid ORDER BY t.id`, hostID, connectionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) recoveryRelease(ctx context.Context, hostID, phase string, requestedAt *time.Time) (*RecoveryRelease, error) {
	if phase == "released" {
		return &RecoveryRelease{State: "released", RequestedAt: requestedAt}, nil
	}
	if phase != "uncertain" || requestedAt == nil {
		return nil, nil
	}
	// A connection without the hardware restart grant is sent nothing (agentws
	// policyRecoveryRelease), so it reads as waiting for a connection, as no agent does.
	var reconciled, capable bool
	if err := s.pool.QueryRow(ctx, `SELECT (h.status<>'offline' AND h.agent_disconnected_at IS NULL AND COALESCE(g.state='complete',false)
			AND COALESCE(h.config_policy_confirmed_groups ? 'hardware',false)),
		COALESCE(h.config_policy_versions->>'recovery_release','')='1'
		FROM hosts h LEFT JOIN host_journal_reconciliation g ON g.host_id=h.id WHERE h.id=$1::uuid`, hostID).Scan(&reconciled, &capable); err != nil {
		return nil, err
	}
	waiting := "agent_report"
	if !reconciled {
		waiting = "agent_connection"
	} else if !capable {
		waiting = "agent_upgrade"
	}
	return &RecoveryRelease{State: "pending", RequestedAt: requestedAt, WaitingFor: &waiting}, nil
}
