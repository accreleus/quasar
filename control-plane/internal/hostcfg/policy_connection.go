package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
)

func decodeGroupSet(raw []byte) map[string]bool {
	set := map[string]bool{}
	var names []string
	if len(raw) > 0 && json.Unmarshal(raw, &names) == nil {
		for _, name := range names {
			set[name] = true
		}
	}
	return set
}

func sortedGroupSet(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BeginPolicyConnection makes the new connection unavailable before its first
// capacity can mark the GPU usable. A downgraded agent stays unavailable when
// a prior typed writer owned a group; no pending desire enters its legacy map.
func (s *Store) BeginPolicyConnection(ctx context.Context, hostID, connectionID string, versions map[string]int, advertised []string, typedV2 bool) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var ever []byte
	if err := tx.QueryRow(ctx, `SELECT config_policy_ever_owned_groups FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&ever); err != nil {
		return false, err
	}
	gate := typedV2 || len(decodeGroupSet(ever)) > 0
	versionJSON, err := json.Marshal(versions)
	if err != nil {
		return false, err
	}
	advertisedJSON, err := json.Marshal(advertised)
	if err != nil {
		return false, err
	}
	var gateID any
	if gate {
		gateID = connectionID
	}
	_, err = tx.Exec(ctx, `UPDATE hosts SET config_policy_versions=$2::jsonb,config_policy_advertised_groups=$3::jsonb,
		config_policy_reported_at=now(),config_policy_gate_connection=$4::uuid,config_policy_delivery_id=NULL
		WHERE id=$1::uuid`, hostID, versionJSON, advertisedJSON, gateID)
	if err != nil {
		return false, err
	}
	return gate, tx.Commit(ctx)
}

// ConfirmPolicyGroups binds the accepted echo to the current gated connection
// and grows the durable ever-owned set. It cannot shrink ownership.
func (s *Store) ConfirmPolicyGroups(ctx context.Context, hostID, connectionID string, accepted []string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var ever []byte
	err = tx.QueryRow(ctx, `SELECT config_policy_ever_owned_groups FROM hosts WHERE id=$1::uuid AND config_policy_gate_connection=$2::uuid FOR UPDATE`, hostID, connectionID).Scan(&ever)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	union := decodeGroupSet(ever)
	for _, name := range accepted {
		union[name] = true
	}
	acceptedJSON, _ := json.Marshal(accepted)
	unionJSON, _ := json.Marshal(sortedGroupSet(union))
	_, err = tx.Exec(ctx, `UPDATE hosts SET config_policy_confirmed_groups=$3::jsonb,config_policy_ever_owned_groups=$4::jsonb
		WHERE id=$1::uuid AND config_policy_gate_connection=$2::uuid`, hostID, connectionID, acceptedJSON, unionJSON)
	if err != nil {
		return false, err
	}
	for _, group := range accepted {
		cmd, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='pending' WHERE host_id=$1::uuid AND group_key=$2 AND status='upgrade_required'`, hostID, group)
		if err != nil {
			return false, err
		}
		if cmd.RowsAffected() > 0 {
			if _, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations SET retry_count=0,next_attempt_at=now() WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2`, hostID, group); err != nil {
				return false, err
			}
		}
	}
	return true, tx.Commit(ctx)
}

func (s *Store) LegacyOwnedOverrides(ctx context.Context, hostID string, provisional []string) (map[string]any, error) {
	var raw, confirmed, ever []byte
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(hs.overrides,'{}'::jsonb),h.config_policy_confirmed_groups,h.config_policy_ever_owned_groups
		FROM hosts h LEFT JOIN host_settings hs ON hs.host_id=h.id WHERE h.id=$1::uuid`, hostID).Scan(&raw, &confirmed, &ever)
	if err != nil {
		return nil, err
	}
	overrides := map[string]any{}
	if err := json.Unmarshal(raw, &overrides); err != nil {
		return nil, err
	}
	owned := decodeGroupSet(confirmed)
	for group := range decodeGroupSet(ever) {
		owned[group] = true
	}
	for _, group := range provisional {
		owned[group] = true
	}
	for key := range overrides {
		group, _ := policyGroup(key)
		if owned[group] {
			delete(overrides, key)
		}
	}
	return overrides, nil
}

// PrepareLegacyDelivery snapshots the full legacy-owned map and, while the
// initial gate is closed, replaces the one delivery ID that may lift it. The
// host lock serializes this projection with policy edits and registration.
func (s *Store) PrepareLegacyDelivery(ctx context.Context, hostID, connectionID, deliveryID string, provisional []string) (map[string]any, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	var gate *string
	var confirmed, ever, raw []byte
	err = tx.QueryRow(ctx, `SELECT h.config_policy_gate_connection::text,h.config_policy_confirmed_groups,h.config_policy_ever_owned_groups,
		COALESCE(hs.overrides,'{}'::jsonb) FROM hosts h LEFT JOIN host_settings hs ON hs.host_id=h.id WHERE h.id=$1::uuid FOR UPDATE OF h`, hostID).Scan(&gate, &confirmed, &ever, &raw)
	if err != nil {
		return nil, false, err
	}
	if gate != nil && *gate != connectionID {
		return nil, false, nil
	}
	overrides := map[string]any{}
	if err := json.Unmarshal(raw, &overrides); err != nil {
		return nil, false, err
	}
	owned := decodeGroupSet(confirmed)
	for group := range decodeGroupSet(ever) {
		owned[group] = true
	}
	for _, group := range provisional {
		owned[group] = true
	}
	for key := range overrides {
		group, _ := policyGroup(key)
		if owned[group] {
			delete(overrides, key)
		}
	}
	if gate != nil {
		if len(confirmed) == 0 {
			return nil, false, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE hosts SET config_policy_delivery_id=$2::uuid WHERE id=$1::uuid`, hostID, deliveryID); err != nil {
			return nil, false, err
		}
	}
	return overrides, true, tx.Commit(ctx)
}

func (s *Store) PolicyOwnedGroups(ctx context.Context, hostID string, provisional []string) (map[string]bool, error) {
	var confirmed, ever []byte
	if err := s.pool.QueryRow(ctx, `SELECT config_policy_confirmed_groups,config_policy_ever_owned_groups FROM hosts WHERE id=$1::uuid`, hostID).Scan(&confirmed, &ever); err != nil {
		return nil, err
	}
	owned := decodeGroupSet(confirmed)
	for group := range decodeGroupSet(ever) {
		owned[group] = true
	}
	for _, group := range provisional {
		owned[group] = true
	}
	return owned, nil
}

// PolicyRestartConflict protects a legacy restart while the initial RH05
// inventory/delivery fence is open or a policy outcome is uncertain.
func (s *Store) PolicyRestartConflict(ctx context.Context, hostID string) (bool, error) {
	var blocked bool
	err := s.pool.QueryRow(ctx, `SELECT h.config_policy_gate_connection IS NOT NULL OR EXISTS (
		SELECT 1 FROM host_setting_groups g WHERE g.host_id=h.id AND g.status='uncertain'
	) FROM hosts h WHERE h.id=$1::uuid`, hostID).Scan(&blocked)
	return blocked, err
}

func (s *Store) HoldPolicyConnection(ctx context.Context, hostID, connectionID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE hosts SET config_policy_gate_connection=$2::uuid,config_policy_delivery_id=NULL WHERE id=$1::uuid AND config_policy_versions->>'typed_settings'='2'`, hostID, connectionID)
	return err
}

func (s *Store) ParkPolicyGroupUpgradeRequired(ctx context.Context, hostID, group string) error {
	_, err := s.pool.Exec(ctx, `UPDATE host_setting_groups SET status='upgrade_required' WHERE host_id=$1::uuid AND group_key=$2 AND status IN ('pending','failed')`, hostID, group)
	return err
}

func (s *Store) SetInitialDelivery(ctx context.Context, hostID, connectionID, deliveryID string) (bool, error) {
	cmd, err := s.pool.Exec(ctx, `UPDATE hosts SET config_policy_delivery_id=$3::uuid WHERE id=$1::uuid AND config_policy_gate_connection=$2::uuid AND config_policy_confirmed_groups IS NOT NULL`, hostID, connectionID, deliveryID)
	return cmd.RowsAffected() == 1, err
}

func (s *Store) AcknowledgeInitialDelivery(ctx context.Context, hostID, connectionID, deliveryID string) (bool, error) {
	// A fresh v2 agent first advertises no groups while it durably seeds its
	// legacy overlay. Its exact map acknowledgement stops retransmission, but
	// admission remains gated until it reconnects advertising a seeded
	// next-session group.
	cmd, err := s.pool.Exec(ctx, `UPDATE hosts SET
		config_policy_gate_connection=CASE WHEN config_policy_confirmed_groups ?| $4::text[] THEN NULL ELSE config_policy_gate_connection END,
		config_policy_delivery_id=CASE WHEN config_policy_confirmed_groups ?| $4::text[] THEN NULL ELSE config_policy_delivery_id END
		WHERE id=$1::uuid AND config_policy_gate_connection=$2::uuid AND config_policy_delivery_id=$3::uuid AND config_policy_confirmed_groups IS NOT NULL`, hostID, connectionID, deliveryID, NextSessionPolicyGroups())
	return cmd.RowsAffected() == 1, err
}
