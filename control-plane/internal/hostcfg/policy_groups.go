package hostcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Transient next-session failures get policyRetryBudget offers, spaced by
// policyRetryBase·2^n capped at policyRetryCap, before the group reads
// retry_exhausted and waits for Retry or a relevant condition change.
const (
	policyRetryBudget = 5
	policyRetryBase   = 5 * time.Second
	policyRetryCap    = 5 * time.Minute
)

var (
	ErrPolicyNotRetryable     = errors.New("policy group is not waiting for Retry")
	ErrPolicyApprovalRequired = errors.New("a restart-scope retry needs fresh idle approval")
)

type policyQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// policyRejectionInvalid reports agent rejection codes that describe the
// request itself. Retrying them cannot succeed, so they never loop.
func policyRejectionInvalid(code string) bool {
	switch code {
	case "invalid_value", "cross_key_invalid", "path_inaccessible", "home_root_outside_mount",
		"unsupported_source", "unsupported_group", "missing_setting", "content_mismatch",
		"resolved_mismatch", "invalid_revision", "revision_conflict", "invalid_content":
		return true
	}
	return false
}

func loadPolicyChoices(ctx context.Context, q policyQuerier, hostID string) (map[string]PolicyChoice, error) {
	rows, err := q.Query(ctx, `SELECT key,source,explicit_value FROM host_setting_choices WHERE host_id=$1::uuid`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	choices := map[string]PolicyChoice{}
	for rows.Next() {
		var key, source string
		var raw []byte
		if err := rows.Scan(&key, &source, &raw); err != nil {
			return nil, err
		}
		choice := PolicyChoice{Source: source}
		if source == "explicit" {
			if err := json.Unmarshal(raw, &choice.Value); err != nil {
				return nil, err
			}
		}
		choices[key] = choice
	}
	return choices, rows.Err()
}

// connectionBaseline returns the deployment baseline only when it was reported
// on connectionID; any other report is unusable evidence.
func connectionBaseline(ctx context.Context, q policyQuerier, hostID, connectionID string) (map[string]any, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT deployment_settings FROM hosts WHERE id=$1::uuid AND deployment_settings_connection=$2::uuid`, hostID, connectionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	baseline, err := ParseDeploymentSettings(raw)
	if err != nil {
		return nil, nil
	}
	return baseline, nil
}

// PolicyEditContext loads what ValidatePolicyEdit checks an edit against.
func (s *Store) PolicyEditContext(ctx context.Context, hostID string) (PolicyEditContext, error) {
	return policyEditContext(ctx, s.pool, hostID)
}

func policyEditContext(ctx context.Context, q policyQuerier, hostID string) (PolicyEditContext, error) {
	var out PolicyEditContext
	choices, err := loadPolicyChoices(ctx, q, hostID)
	if err != nil {
		return out, err
	}
	out.Current = choices
	var baselineRaw, effectiveRaw []byte
	if err := q.QueryRow(ctx, `SELECT deployment_settings,effective_settings FROM hosts WHERE id=$1::uuid`, hostID).Scan(&baselineRaw, &effectiveRaw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrHostNotFound
		}
		return out, err
	}
	// The mount is the agent's pre-policy QUASAR_HOME_ROOT. An agent without a
	// deployment baseline falls back to its effective report, as the legacy
	// PATCH does.
	if len(baselineRaw) > 0 {
		if baseline, err := ParseDeploymentSettings(baselineRaw); err == nil {
			out.Baseline = baseline
			out.MountedHomeRoot, _ = baseline["home_root"].(string)
		}
	} else if len(effectiveRaw) > 0 {
		effective := map[string]string{}
		if json.Unmarshal(effectiveRaw, &effective) == nil {
			out.MountedHomeRoot = effective["home_root"]
		}
	}
	rows, err := q.Query(ctx, `SELECT ref FROM user_homes WHERE host_id=$1::uuid AND provider='local'`, hostID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return out, err
		}
		out.ExistingHomeRefs = append(out.ExistingHomeRefs, ref)
	}
	return out, rows.Err()
}

// ValidateStoredPolicyEdit validates changes against the host's saved choices,
// last reported baseline, mount and existing managed homes. Callers run it
// inside the save transaction, before any write.
func ValidateStoredPolicyEdit(ctx context.Context, q policyQuerier, hostID string, changes map[string]PolicyChoice) error {
	policyCtx, err := policyEditContext(ctx, q, hostID)
	if err != nil {
		return err
	}
	return ValidatePolicyEdit(changes, policyCtx)
}

// NextSessionOffers builds at most one offer per confirmed, pending
// next-session group. Each claims its own obligation under the host lock, so
// groups progress and back off independently.
func (s *Store) NextSessionOffers(ctx context.Context, hostID, bootID, connectionID string, snapshots map[string]PolicySnapshot, newID func() string) ([]*PolicyOffer, error) {
	return s.nextSessionOffers(ctx, hostID, bootID, connectionID, snapshots, newID, 0)
}

// PolicyOffersPerPass bounds one dispatch pass. The agent socket's send queue
// is shorter than the catalog, and a dropped send still spends a retry; the
// remaining groups go out on the next heartbeat.
const PolicyOffersPerPass = 8

// NextSessionOfferBatch is NextSessionOffers for a live socket: at most
// PolicyOffersPerPass groups are claimed and returned.
func (s *Store) NextSessionOfferBatch(ctx context.Context, hostID, bootID, connectionID string, snapshots map[string]PolicySnapshot, newID func() string) ([]*PolicyOffer, error) {
	return s.nextSessionOffers(ctx, hostID, bootID, connectionID, snapshots, newID, PolicyOffersPerPass)
}

func (s *Store) nextSessionOffers(ctx context.Context, hostID, bootID, connectionID string, snapshots map[string]PolicySnapshot, newID func() string, limit int) ([]*PolicyOffer, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var confirmedRaw []byte
	var gated bool
	if err := tx.QueryRow(ctx, `SELECT config_policy_confirmed_groups,config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&confirmedRaw, &gated); err != nil {
		return nil, err
	}
	if gated {
		return nil, nil
	}
	confirmed := decodeGroupSet(confirmedRaw)
	baseline, err := connectionBaseline(ctx, tx, hostID, connectionID)
	if err != nil {
		return nil, err
	}
	choices, err := loadPolicyChoices(ctx, tx, hostID)
	if err != nil {
		return nil, err
	}
	var offers []*PolicyOffer
	for _, group := range NextSessionPolicyGroups() {
		if limit > 0 && len(offers) >= limit {
			break
		}
		snapshot, ok := snapshots[group]
		if !confirmed[group] || !ok || (snapshot.Kind != "seeded" && snapshot.Kind != "verified") || !validPolicyDigest(snapshot.Digest) {
			continue
		}
		var revision int64
		var scope, status string
		err := tx.QueryRow(ctx, `SELECT desired_revision,scope,status FROM host_setting_groups WHERE host_id=$1::uuid AND group_key=$2 FOR UPDATE`, hostID, group).Scan(&revision, &scope, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if status != "pending" || scope != "next_session" {
			continue
		}
		candidate, reason, err := resolveGroupCandidate(group, revision, choices, baseline)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			continue
		}
		factKind := "seeded_group_digest"
		if snapshot.Kind == "verified" {
			factKind = "last_verified_group_digest"
		}
		prerequisites := append(candidate.Prerequisites, map[string]any{"kind": factKind, "id": snapshot.Digest})
		sortPolicyFacts(prerequisites)
		prereqDigest, err := digestPolicyFacts(prerequisites)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$4 WHERE host_id=$1::uuid AND group_key=$2 AND desired_revision=$3 AND desired_digest IS DISTINCT FROM $4`, hostID, group, revision, candidate.Digest); err != nil {
			return nil, err
		}
		claimed, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations
			SET retry_count=retry_count+1,next_attempt_at=now()+make_interval(secs => LEAST($4::float8*power(2,retry_count),$5::float8))
			WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2 AND revision=$3 AND next_attempt_at<=now() AND retry_count<$6`,
			hostID, group, revision, policyRetryBase.Seconds(), policyRetryCap.Seconds(), policyRetryBudget)
		if err != nil {
			return nil, err
		}
		if claimed.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `UPDATE host_setting_groups g SET status='failed' FROM host_reconcile_obligations o
				WHERE g.host_id=o.host_id AND g.group_key=o.resource_key AND o.kind='setting' AND g.host_id=$1::uuid AND g.group_key=$2
				AND o.retry_count>=$3 AND o.next_attempt_at<=now() AND g.desired_revision=o.revision AND g.status='pending'`, hostID, group, policyRetryBudget); err != nil {
				return nil, err
			}
			continue
		}
		offers = append(offers, &PolicyOffer{
			Type: "config_policy_offer", AttemptID: newID(), HostID: hostID, BootIncarnation: bootID, ConnectionIncarnation: connectionID,
			Group: group, Revision: strconv.FormatInt(revision, 10), ContentSHA256: candidate.Digest, Scope: "next_session",
			ExpiresAt: time.Now().UTC().Add(30 * time.Second), PrerequisitesSHA256: prereqDigest, Prerequisites: prerequisites,
			Settings: candidate.Settings, ResolvedSettings: candidate.Resolved,
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return offers, nil
}

func policyErrorKey(hostID, group string) string { return hostID + "\x00" + group }

// ObservePolicyRejected applies an agent `failed` state for the current desired
// revision and digest; a stale report changes nothing. Invalid requests fail
// terminally and lose their obligation, transient ones keep their backoff.
func (s *Store) ObservePolicyRejected(ctx context.Context, hostID, group, revision, digest, code string) (bool, error) {
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != revision {
		return false, errors.New("invalid rejection revision")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM host_setting_groups WHERE host_id=$1::uuid AND group_key=$2 AND desired_revision=$3 AND desired_digest=$4 AND scope='next_session' FOR UPDATE`, hostID, group, n, digest).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "pending" {
		return false, nil
	}
	s.policyErrors.Store(policyErrorKey(hostID, group), code)
	switch {
	case policyRejectionInvalid(code):
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='failed' WHERE host_id=$1::uuid AND group_key=$2`, hostID, group); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2`, hostID, group); err != nil {
			return false, err
		}
	case code == "group_execution_unavailable":
		if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='upgrade_required' WHERE host_id=$1::uuid AND group_key=$2`, hostID, group); err != nil {
			return false, err
		}
	case code == "deployment_baseline_changed":
		// The fresh baseline precedes this report on the socket; re-resolve now.
		if _, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations SET next_attempt_at=now() WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2`, hostID, group); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

// RetryPolicyGroup re-arms a next-session group whose transient retry budget
// is exhausted. Invalid requests and pending groups are not retryable.
// An unknown host is ErrHostNotFound before any group check.
func (s *Store) RetryPolicyGroup(ctx context.Context, hostID, group string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT id::text FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(new(string)); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHostNotFound
		}
		return err
	}
	scope, ok := PolicyGroupScope(group)
	if !ok {
		return ErrPolicyNotRetryable
	}
	if scope != "next_session" {
		return ErrPolicyApprovalRequired
	}
	cmd, err := tx.Exec(ctx, `UPDATE host_setting_groups g SET status='pending' FROM host_reconcile_obligations o
		WHERE g.host_id=o.host_id AND g.group_key=o.resource_key AND o.kind='setting' AND g.host_id=$1::uuid AND g.group_key=$2
		AND g.status='failed' AND o.revision=g.desired_revision AND o.retry_count>=$3`, hostID, group, policyRetryBudget)
	if err != nil {
		return err
	}
	if cmd.RowsAffected() == 0 {
		return ErrPolicyNotRetryable
	}
	if _, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations SET retry_count=0,next_attempt_at=now() WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2`, hostID, group); err != nil {
		return err
	}
	s.policyErrors.Delete(policyErrorKey(hostID, group))
	return tx.Commit(ctx)
}

// ExpectedPolicyResolved is what a next-session group's applied readback must
// show on connectionID: explicit values, and deployment values only from that
// connection's baseline. ok is false while the group cannot be resolved.
func (s *Store) ExpectedPolicyResolved(ctx context.Context, hostID, connectionID, group string) (map[string]any, bool, error) {
	if scope, known := PolicyGroupScope(group); !known || scope != "next_session" {
		return nil, false, nil
	}
	choices, err := loadPolicyChoices(ctx, s.pool, hostID)
	if err != nil {
		return nil, false, err
	}
	baseline, err := connectionBaseline(ctx, s.pool, hostID, connectionID)
	if err != nil {
		return nil, false, err
	}
	candidate, reason, err := resolveGroupCandidate(group, 0, choices, baseline)
	if err != nil || reason != "" {
		return nil, false, err
	}
	return candidate.Resolved, true, nil
}

// PolicyGroupDetail is the retry state behind a group's typed view.
type PolicyGroupDetail struct {
	NextRetryAt *time.Time
	Retryable   bool
}

// DecoratePolicyGroups replaces generic pending/failed remedies with the typed
// reason: retry_exhausted (Retry offered) or validation_failed (never
// retried). Durable state distinguishes them: an exhausted group keeps its
// obligation, an invalid one has none.
// GetPolicy already applies it; the method stays for callers holding a view.
func (s *Store) DecoratePolicyGroups(ctx context.Context, hostID string, view *PolicyView) (map[string]PolicyGroupDetail, error) {
	return decoratePolicyGroups(ctx, s.pool, &s.policyErrors, hostID, view)
}

func decoratePolicyGroups(ctx context.Context, q policyQuerier, policyErrors *sync.Map, hostID string, view *PolicyView) (map[string]PolicyGroupDetail, error) {
	rows, err := q.Query(ctx, `SELECT resource_key,revision,retry_count,next_attempt_at FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='setting'`, hostID)
	if err != nil {
		return nil, err
	}
	type obligation struct {
		revision int64
		retries  int
		next     time.Time
	}
	obligations := map[string]obligation{}
	for rows.Next() {
		var key string
		var o obligation
		if err := rows.Scan(&key, &o.revision, &o.retries, &o.next); err != nil {
			rows.Close()
			return nil, err
		}
		obligations[key] = o
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	details := map[string]PolicyGroupDetail{}
	for key, group := range view.Groups {
		if group.Scope != "next_session" {
			continue
		}
		last := ""
		if code, ok := policyErrors.Load(policyErrorKey(hostID, key)); ok {
			last = fmt.Sprintf(" Last host error: %s.", code)
		}
		o, hasObligation := obligations[key]
		current := hasObligation && strconv.FormatInt(o.revision, 10) == group.DesiredRevision
		var detail PolicyGroupDetail
		switch group.Status {
		case "failed":
			var remedy string
			if current && o.retries >= policyRetryBudget {
				detail.Retryable = true
				remedy = fmt.Sprintf("retry_exhausted: The host did not confirm this setting after %d attempts.%s Use Retry, or reconnect the host.", policyRetryBudget, last)
			} else {
				remedy = "validation_failed: The host rejected this value." + last + " Change the setting; it is not retried automatically."
			}
			group.Remedy = &remedy
		case "pending":
			if current && o.retries > 0 {
				next := o.next
				detail.NextRetryAt = &next
				remedy := fmt.Sprintf("Waiting for the host to verify the next-session setting (attempt %d of %d).%s", o.retries, policyRetryBudget, last)
				group.Remedy = &remedy
			}
		}
		view.Groups[key] = group
		details[key] = detail
	}
	return details, nil
}

// ReconcilePolicySnapshots compares each next-session group's desired digest
// with the complete current-connection active snapshot. Only a matching
// verified snapshot restores applied; a seed never proves application.
func (s *Store) ReconcilePolicySnapshots(ctx context.Context, hostID, connectionID string, snapshots map[string]PolicySnapshot) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var gated *string
	if err := tx.QueryRow(ctx, `SELECT config_policy_gate_connection::text FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&gated); err != nil {
		return err
	}
	if gated == nil || *gated != connectionID {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT group_key,desired_revision,desired_digest FROM host_setting_groups WHERE host_id=$1::uuid AND scope='next_session' FOR UPDATE`, hostID)
	if err != nil {
		return err
	}
	type desired struct {
		revision int64
		digest   *string
	}
	groups := map[string]desired{}
	for rows.Next() {
		var key string
		var d desired
		if err := rows.Scan(&key, &d.revision, &d.digest); err != nil {
			rows.Close()
			return err
		}
		groups[key] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for group, d := range groups {
		snapshot, ok := snapshots[group]
		if ok && snapshot.Kind == "verified" && validPolicyDigest(snapshot.Digest) && d.digest != nil && snapshot.Digest == *d.digest {
			_, err = tx.Exec(ctx, `UPDATE host_setting_groups SET status='applied',applied_revision=$3,applied_digest=$4,evidence_connection=$5::uuid,evidence_at=now() WHERE host_id=$1::uuid AND group_key=$2 AND status IN ('pending','applied')`, hostID, group, d.revision, snapshot.Digest, connectionID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE host_setting_groups SET status='pending',evidence_connection=NULL WHERE host_id=$1::uuid AND group_key=$2 AND status='applied'`, hostID, group)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// InvalidateNextSessionEvidenceOnReconnect returns every next-session group's
// last observation to pending and re-arms a fresh retry budget: a reconnect is
// a relevant condition change, and old-connection evidence is not current.
func (s *Store) InvalidateNextSessionEvidenceOnReconnect(ctx context.Context, hostID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='pending',evidence_connection=NULL WHERE host_id=$1::uuid AND scope='next_session' AND status IN ('applied','failed')`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations(host_id,kind,resource_key,revision,next_attempt_at)
		SELECT host_id,'setting',group_key,desired_revision,now() FROM host_setting_groups WHERE host_id=$1::uuid AND scope='next_session'
		ON CONFLICT(host_id,kind,resource_key) DO UPDATE SET revision=excluded.revision,next_attempt_at=now(),retry_count=0`, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// policyGroupUsesDeployment reports whether any of group's keys resolves from
// the deployment baseline (an absent choice is deployment).
func policyGroupUsesDeployment(group string, choices map[string]PolicyChoice) bool {
	for _, key := range PolicyGroupKeys(group) {
		if choice, ok := choices[key]; !ok || choice.Source == "deployment" {
			return true
		}
	}
	return false
}

// refreshDeploymentDigests re-resolves deployment-source next-session groups
// after a new current-connection baseline. A changed digest is a relevant
// condition change: the group returns to pending with a fresh retry budget.
func refreshDeploymentDigests(ctx context.Context, tx pgx.Tx, hostID, connectionID string) error {
	baseline, err := connectionBaseline(ctx, tx, hostID, connectionID)
	if err != nil || baseline == nil {
		return err
	}
	choices, err := loadPolicyChoices(ctx, tx, hostID)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT group_key,desired_revision,desired_digest FROM host_setting_groups WHERE host_id=$1::uuid AND scope='next_session' FOR UPDATE`, hostID)
	if err != nil {
		return err
	}
	type desired struct {
		revision int64
		digest   *string
	}
	groups := map[string]desired{}
	for rows.Next() {
		var key string
		var d desired
		if err := rows.Scan(&key, &d.revision, &d.digest); err != nil {
			rows.Close()
			return err
		}
		groups[key] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for group, d := range groups {
		if !policyGroupUsesDeployment(group, choices) {
			continue
		}
		candidate, reason, err := resolveGroupCandidate(group, d.revision, choices, baseline)
		if err != nil {
			return err
		}
		if reason != "" || (d.digest != nil && *d.digest == candidate.Digest) {
			continue
		}
		cmd, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$3,
			status=CASE WHEN status IN ('applied','failed','pending') THEN 'pending' ELSE status END
			WHERE host_id=$1::uuid AND group_key=$2 AND status IN ('applied','failed','pending')`, hostID, group, candidate.Digest)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$3 WHERE host_id=$1::uuid AND group_key=$2`, hostID, group, candidate.Digest); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations(host_id,kind,resource_key,revision,next_attempt_at) VALUES($1::uuid,'setting',$2,$3,now())
			ON CONFLICT(host_id,kind,resource_key) DO UPDATE SET revision=excluded.revision,next_attempt_at=now(),retry_count=0`, hostID, group, d.revision); err != nil {
			return err
		}
	}
	return nil
}
