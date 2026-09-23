package hostcfg

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func newPolicyAttemptID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cannot generate RH05 attempt ID: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var ErrStaleRevision = errors.New("stale policy revision")
var ErrHostNotFound = errors.New("host not found")
var ErrUpgradeRequired = errors.New("typed policy group is not owned by this host")
var ErrPolicyAttemptConflict = errors.New("policy attempt or journal reconciliation is in progress")

type PolicyChoice struct {
	Source string `json:"source"`
	Value  any    `json:"value,omitempty"`
}

type PolicyGroup struct {
	DesiredRevision    string     `json:"desired_revision"`
	AppliedRevision    *string    `json:"applied_revision"`
	DesiredDigest      *string    `json:"desired_digest"`
	AppliedDigest      *string    `json:"applied_digest"`
	Scope              string     `json:"scope"`
	Status             string     `json:"status"`
	Fresh              bool       `json:"fresh"`
	ObservedAt         *time.Time `json:"observed_at"`
	Remedy             *string    `json:"remedy"`
	ApprovalPreview    any        `json:"approval_preview"`
	EvidenceConnection *string    `json:"-"`
}

type PolicyEvidenceView struct {
	Status     string     `json:"status"`
	ObservedAt *time.Time `json:"observed_at"`
	Remedy     *string    `json:"remedy"`
}

type PolicyView struct {
	Revision         string                  `json:"revision"`
	Choices          map[string]PolicyChoice `json:"choices"`
	Resolved         map[string]any          `json:"resolved"`
	Groups           map[string]PolicyGroup  `json:"groups"`
	ImagePreparation PolicyEvidenceView      `json:"image_preparation"`
	Readiness        PolicyEvidenceView      `json:"readiness"`
}

func (s *Store) ChangedPolicyKeysSince(ctx context.Context, hostID, revision string) ([]string, error) {
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT key FROM host_setting_choices WHERE host_id=$1::uuid AND revision>$2 ORDER BY key`, hostID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

type PolicyOffer struct {
	Type                  string                  `json:"type"`
	AttemptID             string                  `json:"attempt_id"`
	HostID                string                  `json:"host_id"`
	BootIncarnation       string                  `json:"boot_incarnation"`
	ConnectionIncarnation string                  `json:"connection_incarnation"`
	Group                 string                  `json:"group"`
	Revision              string                  `json:"revision"`
	ContentSHA256         string                  `json:"content_sha256"`
	Scope                 string                  `json:"scope"`
	ExpiresAt             time.Time               `json:"expires_at"`
	PrerequisitesSHA256   string                  `json:"prerequisites_sha256"`
	Prerequisites         []any                   `json:"prerequisites"`
	Settings              map[string]PolicyChoice `json:"settings"`
	ResolvedSettings      map[string]any          `json:"resolved_settings"`
}

// PolicySnapshot is the active group digest from a complete journal inventory
// on the authenticated current connection. A seed is a recovery target, not
// application proof.
type PolicySnapshot struct {
	Kind   string
	Digest string
}

// NextSessionOffer builds the current idle-timeout offer from the durable
// obligation. Deployment resolution waits for authenticated agent evidence;
// a guessed catalog default is never sent as the effective value.
func (s *Store) NextSessionOffer(ctx context.Context, hostID, attemptID, bootID, connectionID string, snapshot *PolicySnapshot) (*PolicyOffer, error) {
	if snapshot == nil || (snapshot.Kind != "seeded" && snapshot.Kind != "verified") || !validPolicyDigest(snapshot.Digest) {
		return nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var owned bool
	var gated bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(config_policy_confirmed_groups ? 'idle_timeout_secs',false),config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&owned, &gated); err != nil {
		return nil, err
	}
	if !owned || gated {
		return nil, nil
	}
	var revision int64
	var scope, status string
	if err := tx.QueryRow(ctx, `SELECT desired_revision,scope,status FROM host_setting_groups WHERE host_id=$1::uuid AND group_key='idle_timeout_secs'`, hostID).Scan(&revision, &scope, &status); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if status != "pending" || scope != "next_session" {
		return nil, nil
	}
	candidate, err := idleCandidateForConnection(ctx, tx, hostID, connectionID)
	if err != nil || candidate == nil {
		return nil, err
	}
	factKind := "seeded_group_digest"
	if snapshot.Kind == "verified" {
		factKind = "last_verified_group_digest"
	}
	candidate.Prerequisites = append(candidate.Prerequisites, map[string]any{"kind": factKind, "id": snapshot.Digest})
	sortPolicyFacts(candidate.Prerequisites)
	candidate.PrereqDigest, err = digestPolicyFacts(candidate.Prerequisites)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$3 WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' AND desired_revision=$2 AND desired_digest IS DISTINCT FROM $3`, hostID, revision, candidate.Digest); err != nil {
		return nil, err
	}
	claimed, err := tx.Exec(ctx, `UPDATE host_reconcile_obligations SET retry_count=retry_count+1,next_attempt_at=now()+interval '10 seconds' WHERE host_id=$1::uuid AND kind='setting' AND resource_key='idle_timeout_secs' AND revision=$2 AND next_attempt_at<=now() AND retry_count<5`, hostID, revision)
	if err != nil {
		return nil, err
	}
	if claimed.RowsAffected() == 0 {
		_, _ = tx.Exec(ctx, `UPDATE host_setting_groups g SET status='failed' FROM host_reconcile_obligations o WHERE g.host_id=o.host_id AND g.group_key=o.resource_key AND g.host_id=$1::uuid AND g.group_key='idle_timeout_secs' AND o.kind='setting' AND o.retry_count>=5 AND o.next_attempt_at<=now() AND g.desired_revision=o.revision AND g.status='pending'`, hostID)
		_ = tx.Commit(ctx)
		return nil, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &PolicyOffer{Type: "config_policy_offer", AttemptID: attemptID, HostID: hostID, BootIncarnation: bootID, ConnectionIncarnation: connectionID, Group: "idle_timeout_secs", Revision: strconv.FormatInt(revision, 10), ContentSHA256: candidate.Digest, Scope: scope, ExpiresAt: time.Now().UTC().Add(30 * time.Second), PrerequisitesSHA256: candidate.PrereqDigest, Prerequisites: candidate.Prerequisites, Settings: map[string]PolicyChoice{"idle_timeout_secs": candidate.Choice}, ResolvedSettings: map[string]any{"idle_timeout_secs": candidate.Value}}, nil
}

func (s *Store) InvalidatePolicyEvidenceOnReconnect(ctx context.Context, hostID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE host_setting_groups SET status='pending',evidence_connection=NULL WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' AND status IN ('applied','failed')`, hostID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations(host_id,kind,resource_key,revision,next_attempt_at) SELECT host_id,'setting',group_key,desired_revision,now() FROM host_setting_groups WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' ON CONFLICT(host_id,kind,resource_key) DO UPDATE SET revision=excluded.revision,next_attempt_at=now(),retry_count=0`, hostID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ReconcilePolicySnapshot compares the completed, current-connection active
// journal snapshot with the desired candidate. A terminal history row alone
// never establishes application after reconnect.
func (s *Store) ReconcilePolicySnapshot(ctx context.Context, hostID, connectionID string, snapshot *PolicySnapshot) error {
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
	var revision int64
	var desired *string
	err = tx.QueryRow(ctx, `SELECT desired_revision,desired_digest FROM host_setting_groups WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' FOR UPDATE`, hostID).Scan(&revision, &desired)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if snapshot != nil && snapshot.Kind == "verified" && validPolicyDigest(snapshot.Digest) && desired != nil && snapshot.Digest == *desired {
		_, err = tx.Exec(ctx, `UPDATE host_setting_groups SET status='applied',applied_revision=$2,applied_digest=$3,evidence_connection=$4::uuid,evidence_at=now() WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' AND status IN ('pending','applied')`, hostID, revision, snapshot.Digest, connectionID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE host_setting_groups SET status='pending',evidence_connection=NULL WHERE host_id=$1::uuid AND group_key='idle_timeout_secs' AND status='applied'`, hostID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func validatePolicyChanges(changes map[string]PolicyChoice) error {
	if len(changes) == 0 {
		return errors.New("changes must not be empty")
	}
	for key, choice := range changes {
		knob, ok := byKey()[key]
		if !ok {
			return fmt.Errorf("unknown setting %q", key)
		}
		switch choice.Source {
		case "explicit":
			if choice.Value == nil {
				return fmt.Errorf("%q requires a value", key)
			}
			if err := validateValue(knob, choice.Value); err != nil {
				return err
			}
		case "deployment":
			if choice.Value != nil {
				return fmt.Errorf("%q deployment forbids a value", key)
			}
		case "automatic":
			if key != "encoder" && key != "render_node" {
				return fmt.Errorf("%q does not support automatic", key)
			}
			if choice.Value != nil {
				return fmt.Errorf("%q automatic forbids a value", key)
			}
		default:
			return fmt.Errorf("%q has invalid source", key)
		}
	}
	return nil
}

func policyGroup(key string) (string, string) {
	if key == "encoder" || key == "render_node" || key == "cuda_device" {
		return "hardware", "restart"
	}
	if byKey()[key].Class == ClassRestart {
		return key, "restart"
	}
	return key, "next_session"
}

func (s *Store) GetPolicy(ctx context.Context, hostID string) (PolicyView, error) {
	view := PolicyView{Choices: map[string]PolicyChoice{}, Resolved: map[string]any{}, Groups: map[string]PolicyGroup{}, ImagePreparation: PolicyEvidenceView{Status: "unknown"}, Readiness: PolicyEvidenceView{Status: "unknown"}}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return view, err
	}
	defer tx.Rollback(ctx)
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM host_policy_revisions WHERE host_id=$1::uuid`, hostID).Scan(&revision); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return view, ErrHostNotFound
		}
		return view, err
	}
	view.Revision = strconv.FormatInt(revision, 10)
	var everOwnedRaw []byte
	if err := tx.QueryRow(ctx, `SELECT config_policy_ever_owned_groups FROM hosts WHERE id=$1::uuid`, hostID).Scan(&everOwnedRaw); err != nil {
		return view, err
	}
	everOwned := decodeGroupSet(everOwnedRaw)
	rows, err := tx.Query(ctx, `SELECT key,source,explicit_value FROM host_setting_choices WHERE host_id=$1::uuid`, hostID)
	if err != nil {
		return view, err
	}
	for rows.Next() {
		var key, source string
		var raw []byte
		if err := rows.Scan(&key, &source, &raw); err != nil {
			rows.Close()
			return view, err
		}
		choice := PolicyChoice{Source: source}
		if source == "explicit" && len(raw) > 0 {
			if err := json.Unmarshal(raw, &choice.Value); err != nil {
				rows.Close()
				return view, err
			}
		}
		view.Choices[key] = choice
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return view, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `SELECT group_key,desired_revision,applied_revision,desired_digest,applied_digest,scope,status,evidence_at,evidence_connection::text FROM host_setting_groups WHERE host_id=$1::uuid`, hostID)
	if err != nil {
		return view, err
	}
	for rows.Next() {
		var key string
		var desired int64
		var applied *int64
		var digest, appliedDigest *string
		var scope, status string
		var observed *time.Time
		var evidenceConnection *string
		if err := rows.Scan(&key, &desired, &applied, &digest, &appliedDigest, &scope, &status, &observed, &evidenceConnection); err != nil {
			rows.Close()
			return view, err
		}
		group := PolicyGroup{DesiredRevision: strconv.FormatInt(desired, 10), DesiredDigest: digest, AppliedDigest: appliedDigest, Scope: scope, Status: status, ObservedAt: observed, EvidenceConnection: evidenceConnection}
		if applied != nil {
			str := strconv.FormatInt(*applied, 10)
			group.AppliedRevision = &str
		}
		if status == "pending" {
			remedy := "Waiting for the host to verify the next-session setting."
			group.Remedy = &remedy
		} else if status == "failed" {
			remedy := "Verification did not complete after bounded retries. Reconnect the host or save a new choice to retry."
			group.Remedy = &remedy
		} else if status == "upgrade_required" {
			remedy := "The legacy writer remains active for this group. Upgrade the agent to enable RH05 verification."
			if everOwned[key] {
				remedy = "Typed ownership remains protected after agent downgrade. Re-upgrade the agent or repair ownership; the legacy value is not sent."
			}
			group.Remedy = &remedy
		} else if status == "uncertain" {
			remedy := "Execution state is uncertain. Keep the host protected and reconcile its durable journal before new work."
			group.Remedy = &remedy
		}
		view.Groups[key] = group
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return view, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return view, err
	}
	for _, knob := range Catalog() {
		if _, ok := view.Choices[knob.Key]; !ok {
			view.Choices[knob.Key] = PolicyChoice{Source: "deployment"}
		}
	}
	if group, ok := view.Groups["idle_timeout_secs"]; ok && group.Status == "pending" && group.DesiredDigest == nil && view.Choices["idle_timeout_secs"].Source == "deployment" {
		remedy := "baseline_unavailable: request a fresh agent capacity report or reconnect before verification."
		group.Remedy = &remedy
		view.Groups["idle_timeout_secs"] = group
	}
	effective, err := s.GetEffective(ctx, hostID)
	if err != nil {
		return view, err
	}
	for _, knob := range Catalog() {
		choice := view.Choices[knob.Key]
		var value any
		if choice.Source == "explicit" {
			value = choice.Value
		} else if choice.Source != "deployment" {
			if raw, ok := effective[knob.Key]; ok {
				value = raw
			}
		}
		view.Resolved[knob.Key] = map[string]any{"value": value, "source": choice.Source, "observed_at": nil, "evidence_id": nil}
	}
	return view, nil
}

// SavePolicy serializes intent under the host row and policy revision. The
// obligation is committed with the desired state; a failed socket send cannot
// erase an offline edit.
func (s *Store) SavePolicy(ctx context.Context, hostID, expected string, changes map[string]PolicyChoice, updatedBy *string) (PolicyView, error) {
	return s.savePolicy(ctx, hostID, expected, changes, updatedBy, true)
}

func (s *Store) savePolicy(ctx context.Context, hostID, expected string, changes map[string]PolicyChoice, updatedBy *string, requireOwned bool) (PolicyView, error) {
	var empty PolicyView
	if err := validatePolicyChanges(changes); err != nil {
		return empty, err
	}
	want, err := strconv.ParseInt(expected, 10, 64)
	if err != nil || want < 0 || strconv.FormatInt(want, 10) != expected {
		return empty, errors.New("expected_revision must be a canonical decimal string")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)
	var id string
	var confirmed, everOwned []byte
	var settingsGate bool
	if err := tx.QueryRow(ctx, `SELECT id::text,config_policy_confirmed_groups,config_policy_ever_owned_groups,config_policy_gate_connection IS NOT NULL FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&id, &confirmed, &everOwned, &settingsGate); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, ErrHostNotFound
		}
		return empty, err
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM host_policy_revisions WHERE host_id=$1::uuid FOR UPDATE`, hostID).Scan(&current); err != nil {
		return empty, err
	}
	if current != want {
		return empty, ErrStaleRevision
	}
	owned := map[string]bool{}
	for _, encoded := range [][]byte{confirmed, everOwned} {
		var groups []string
		if len(encoded) > 0 && json.Unmarshal(encoded, &groups) == nil {
			for _, group := range groups {
				owned[group] = true
			}
		}
	}
	if requireOwned {
		for key := range changes {
			group, _ := policyGroup(key)
			if !owned[group] {
				return empty, ErrUpgradeRequired
			}
		}
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT overrides FROM host_settings WHERE host_id=$1::uuid`, hostID).Scan(&raw); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	overrides := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &overrides); err != nil {
			return empty, err
		}
	}
	if !requireOwned {
		for key, choice := range changes {
			group, scope := policyGroup(key)
			if scope != "restart" || owned[group] {
				continue
			}
			old, hadOld := overrides[key]
			changed := choice.Source == "explicit" && (!hadOld || !reflect.DeepEqual(old, choice.Value)) || choice.Source == "deployment" && hadOld
			if !changed {
				continue
			}
			var uncertain bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_setting_groups WHERE host_id=$1::uuid AND status='uncertain')`, hostID).Scan(&uncertain); err != nil {
				return empty, err
			}
			if settingsGate || uncertain {
				return empty, ErrPolicyAttemptConflict
			}
		}
	}
	for key, choice := range changes {
		if choice.Source == "explicit" {
			overrides[key] = choice.Value
		} else {
			delete(overrides, key)
		}
	}
	if err := ValidateResolved(Resolve(overrides)); err != nil {
		return empty, err
	}
	next := current + 1
	if _, err := tx.Exec(ctx, `UPDATE host_policy_revisions SET revision=$2,updated_at=now(),updated_by=$3 WHERE host_id=$1::uuid`, hostID, next, updatedBy); err != nil {
		return empty, err
	}
	groups := map[string]string{}
	for key, choice := range changes {
		var value any
		if choice.Source == "explicit" {
			b, e := json.Marshal(choice.Value)
			if e != nil {
				return empty, e
			}
			value = b
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_setting_choices(host_id,key,source,explicit_value,revision) VALUES($1::uuid,$2,$3,$4,$5) ON CONFLICT(host_id,key) DO UPDATE SET source=excluded.source,explicit_value=excluded.explicit_value,revision=excluded.revision`, hostID, key, choice.Source, value, next); err != nil {
			return empty, err
		}
		group, scope := policyGroup(key)
		groups[group] = scope
	}
	encoded, err := json.Marshal(overrides)
	if err != nil {
		return empty, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_settings(host_id,overrides,updated_by,updated_at) VALUES($1::uuid,$2,$3,now()) ON CONFLICT(host_id) DO UPDATE SET overrides=excluded.overrides,updated_by=excluded.updated_by,updated_at=now()`, hostID, encoded, updatedBy); err != nil {
		return empty, err
	}
	for group, scope := range groups {
		// A deployment candidate has no digest until current-connection baseline
		// evidence resolves it. Non-owned groups retain intent for later upgrade
		// but cannot claim typed execution through the legacy writer.
		settings := map[string]PolicyChoice{}
		resolvedSettings := map[string]any{}
		for key, choice := range changes {
			selected, _ := policyGroup(key)
			if selected == group {
				settings[key] = choice
				if choice.Source == "explicit" {
					resolvedSettings[key] = choice.Value
				}
			}
		}
		var digest *string
		if len(settings) == len(resolvedSettings) {
			payload := map[string]any{"group": group, "scope": scope, "revision": strconv.FormatInt(next, 10), "settings": settings, "resolved_settings": resolvedSettings}
			value, e := digestJSON(payload)
			if e != nil {
				return empty, e
			}
			digest = &value
		}
		status := "pending"
		if !owned[group] {
			status = "upgrade_required"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_setting_groups(host_id,group_key,desired_revision,desired_digest,scope,status) VALUES($1::uuid,$2,$3,$4,$5,$6) ON CONFLICT(host_id,group_key) DO UPDATE SET desired_revision=excluded.desired_revision,desired_digest=excluded.desired_digest,scope=excluded.scope,status=excluded.status`, hostID, group, next, digest, scope, status); err != nil {
			return empty, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations(host_id,kind,resource_key,revision,next_attempt_at) VALUES($1::uuid,'setting',$2,$3,now()) ON CONFLICT(host_id,kind,resource_key) DO UPDATE SET revision=excluded.revision,next_attempt_at=now(),retry_count=0`, hostID, group, next); err != nil {
			return empty, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return s.GetPolicy(ctx, hostID)
}

// SaveLegacyPatch routes a revisionless compatibility edit through the same
// optimistic policy transaction. A null always selects deployment, including
// when the previous choice was Automatic.
func (s *Store) SaveLegacyPatch(ctx context.Context, hostID string, patch map[string]any, updatedBy *string) (PolicyView, error) {
	if len(patch) == 0 {
		return s.GetPolicy(ctx, hostID)
	}
	changes := make(map[string]PolicyChoice, len(patch))
	for key, value := range patch {
		if value == nil {
			changes[key] = PolicyChoice{Source: "deployment"}
		} else {
			changes[key] = PolicyChoice{Source: "explicit", Value: value}
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		current, err := s.GetPolicy(ctx, hostID)
		if err != nil {
			return PolicyView{}, err
		}
		view, err := s.savePolicy(ctx, hostID, current.Revision, changes, updatedBy, false)
		if errors.Is(err, ErrStaleRevision) {
			continue
		}
		return view, err
	}
	return PolicyView{}, ErrStaleRevision
}

// ObservePolicyApplied accepts only authenticated, content-bound, current
// next-session readback. A stale or reordered observation is harmless.
func (s *Store) ObservePolicyApplied(ctx context.Context, hostID, group, revision, digest, scope, connectionID string) (bool, error) {
	n, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != revision {
		return false, errors.New("invalid observation revision")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	cmd, err := tx.Exec(ctx, `UPDATE host_setting_groups SET applied_revision=$3,applied_digest=$4,status='applied',evidence_connection=$6::uuid,evidence_at=now() WHERE host_id=$1::uuid AND group_key=$2 AND desired_revision=$3 AND desired_digest=$4 AND scope=$5`, hostID, group, n, digest, scope, connectionID)
	if err != nil {
		return false, err
	}
	if cmd.RowsAffected() == 0 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_reconcile_obligations WHERE host_id=$1::uuid AND kind='setting' AND resource_key=$2 AND revision=$3`, hostID, group, n); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
