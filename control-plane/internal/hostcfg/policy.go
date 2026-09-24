package hostcfg

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
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
	NextRetryAt        *time.Time `json:"next_retry_at"`
	ApprovalPreview    any        `json:"approval_preview"`
	AttemptID          *string    `json:"attempt_id,omitempty"`
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

// PolicySnapshot is one group's active digest from a complete journal
// inventory on the authenticated current connection; callers key them by
// group. A seed is a recovery target, not application proof.
type PolicySnapshot struct {
	Kind   string
	Digest string
}

// InvalidatePolicyEvidenceOnReconnect returns every next-session group's
// last observation to pending with a fresh retry budget. Old-connection
// evidence is not current; the journal inventory restores applied.
func (s *Store) InvalidatePolicyEvidenceOnReconnect(ctx context.Context, hostID string) error {
	return s.InvalidateNextSessionEvidenceOnReconnect(ctx, hostID)
}

// validatePolicyChanges is the context-free part of ValidatePolicyEdit: every
// key, source and catalog value. Cross-key and storage checks need the host
// and run inside the save transaction.
func validatePolicyChanges(changes map[string]PolicyChoice) error {
	if len(changes) == 0 {
		return policyInvalid("validation_failed", "changes must not be empty")
	}
	keys := make([]string, 0, len(changes))
	for key := range changes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := validatePolicyChoice(key, changes[key]); err != nil {
			return err
		}
	}
	return nil
}

func policyGroup(key string) (string, string) {
	if spec, ok := PolicySpec(key); ok {
		return spec.Group, spec.Scope
	}
	return key, "next_session"
}

// An Automatic hardware value is shown as applied only when the observed
// value reconstructs the exact full group digest verified by this connection's
// agent journal. A retained legacy effective map alone proves nothing.
func verifiedHardwareValues(group PolicyGroup, choices map[string]PolicyChoice, baseline map[string]any, effective map[string]string) (map[string]any, bool) {
	if group.AppliedDigest == nil || group.AppliedRevision == nil || *group.AppliedRevision != group.DesiredRevision {
		return nil, false
	}
	settings := map[string]PolicyChoice{}
	resolved := map[string]any{}
	for _, key := range PolicyGroupKeys("hardware") {
		choice, ok := choices[key]
		if !ok {
			return nil, false
		}
		settings[key] = choice
		switch choice.Source {
		case "explicit":
			if choice.Value == nil {
				return nil, false
			}
			resolved[key] = choice.Value
		case "deployment":
			value, ok := baseline[key]
			if !ok || value == nil {
				return nil, false
			}
			resolved[key] = value
		case "automatic":
			value := effective[key]
			if value == "" {
				return nil, false
			}
			resolved[key] = value
		default:
			return nil, false
		}
	}
	digest, err := digestJSON(map[string]any{"group": "hardware", "scope": "restart", "revision": group.DesiredRevision,
		"settings": settings, "resolved_settings": resolved})
	if err != nil || digest != *group.AppliedDigest {
		return nil, false
	}
	return resolved, true
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
	var everOwnedRaw, confirmedRaw []byte
	if err := tx.QueryRow(ctx, `SELECT config_policy_ever_owned_groups,config_policy_confirmed_groups FROM hosts WHERE id=$1::uuid`, hostID).Scan(&everOwnedRaw, &confirmedRaw); err != nil {
		return view, err
	}
	everOwned := decodeGroupSet(everOwnedRaw)
	confirmed := decodeGroupSet(confirmedRaw)
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
	// A group's first typed edit has no persisted row yet. Project the
	// negotiated hardware group as well, so an existing host can deliberately
	// choose Automatic through the revisioned writer. A legacy agent retains
	// its existing editor until it confirms ownership.
	persisted := map[string]bool{}
	for key := range view.Groups {
		persisted[key] = true
	}
	projectedGroups := append(NextSessionPolicyGroups(), "hardware")
	for _, key := range projectedGroups {
		if persisted[key] {
			continue
		}
		scope, _ := PolicyGroupScope(key)
		group := PolicyGroup{DesiredRevision: view.Revision, Scope: scope, Status: "upgrade_required"}
		if confirmed[key] {
			group.Status = "pending"
			remedy := "No RH05 policy change has been saved for this group. Current host behavior has not been verified through this policy."
			group.Remedy = &remedy
		} else {
			remedy := "The legacy writer remains active for this group. Upgrade the agent to enable RH05 verification."
			if everOwned[key] {
				remedy = "Typed ownership remains protected after agent downgrade. Re-upgrade the agent or repair ownership; the legacy value is not sent."
			}
			group.Remedy = &remedy
		}
		view.Groups[key] = group
	}
	details, err := decoratePolicyGroups(ctx, tx, &s.policyErrors, hostID, &view)
	if err != nil {
		return view, err
	}
	if err := tx.Commit(ctx); err != nil {
		return view, err
	}
	for _, knob := range Catalog() {
		if _, ok := view.Choices[knob.Key]; !ok {
			view.Choices[knob.Key] = PolicyChoice{Source: "deployment"}
		}
	}
	for key, group := range view.Groups {
		group.NextRetryAt = details[key].NextRetryAt
		if persisted[key] && group.Scope == "next_session" && group.Status == "pending" && group.DesiredDigest == nil && policyGroupUsesDeployment(key, view.Choices) {
			remedy := "baseline_unavailable: request a fresh agent capacity report or reconnect before verification."
			group.Remedy = &remedy
		}
		view.Groups[key] = group
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
		} else {
			group, _ := policyGroup(knob.Key)
			if group != "hardware" {
				value = effective[knob.Key]
			}
		}
		view.Resolved[knob.Key] = map[string]any{"value": value, "source": choice.Source, "observed_at": nil, "evidence_id": nil}
	}
	for key, group := range view.Groups {
		if group.Scope != "restart" {
			continue
		}
		preview, err := s.PreviewIdleApply(ctx, hostID, key)
		if err != nil {
			return view, err
		}
		if preview == nil && group.Status == "pending" {
			remedy := "Idle approval awaits complete current host inventory, a resolved configuration candidate, and any open disruptive operation."
			if key == "hardware" && (view.Choices["encoder"].Source == "automatic" || view.Choices["render_node"].Source == "automatic") {
				remedy = "Automatic hardware choice awaits one accessible GPU and a passing current media host probe. Check host readiness and device access; the proposed encoder is tested during approved startup before it is marked applied."
				missing, err := missingDeploymentHardwareBaseline(ctx, s.pool, hostID, view.Choices)
				if err != nil {
					return view, err
				}
				if len(missing) > 0 {
					remedy = "baseline_unavailable: the current agent connection has not reported " + strings.Join(missing, ", ") + ". Request a fresh capacity report or reconnect before reviewing this hardware choice."
				}
			}
			group.Remedy = &remedy
		} else if key == "hardware" && preview != nil && preview.Available && group.Status == "pending" {
			remedy := "Review the resolved hardware candidate and its evidence, then approve an idle restart. The agent verifies the selected media path before reporting it applied."
			group.Remedy = &remedy
		}
		group.ApprovalPreview = preview
		var attemptID string
		err = s.pool.QueryRow(ctx, `SELECT id::text FROM host_config_approvals
			WHERE host_id=$1::uuid AND group_key=$2 ORDER BY created_at DESC,id DESC LIMIT 1`, hostID, key).Scan(&attemptID)
		if err == nil {
			group.AttemptID = &attemptID
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return view, err
		}
		view.Groups[key] = group
	}
	return view, nil
}

// A hardware offer always covers all dependent settings. Missing persisted
// choices are deployment-source, and their values must come from this socket's
// baseline before an Automatic candidate can be reviewed.
func missingDeploymentHardwareBaseline(ctx context.Context, db idleQueryDB, hostID string, choices map[string]PolicyChoice) ([]string, error) {
	keys := []string{}
	for _, key := range PolicyGroupKeys("hardware") {
		if choices[key].Source == "deployment" {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	var connection *string
	err := db.QueryRow(ctx, `SELECT connection_incarnation::text FROM host_journal_reconciliation
		WHERE host_id=$1::uuid AND state='complete'`, hostID).Scan(&connection)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if connection == nil {
		return nil, nil
	}
	baseline, err := deploymentSettingsForConnection(ctx, db, hostID, *connection)
	if err != nil {
		return nil, err
	}
	missing := []string{}
	for _, key := range keys {
		if baseline[key] == nil {
			missing = append(missing, key)
		}
	}
	return missing, nil
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
	// Cross-key and storage rules judge the whole resulting choice set against
	// the agent-reported baseline, mount and existing managed homes, never a
	// catalog default. Existing homes are never relocated by a root change.
	if err := ValidateStoredPolicyEdit(ctx, tx, hostID, changes); err != nil {
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
	// The desired digest covers the entire persisted group, including keys
	// retained from earlier revisions. A one-key edit must still describe the
	// same candidate that the approval preview will reconstruct.
	allChoices, err := loadPolicyChoices(ctx, tx, hostID)
	if err != nil {
		return empty, err
	}
	for group, scope := range groups {
		// A deployment candidate has no digest until current-connection baseline
		// evidence resolves it. Non-owned groups retain intent for later upgrade
		// but cannot claim typed execution through the legacy writer.
		settings := map[string]PolicyChoice{}
		resolvedSettings := map[string]any{}
		for key, choice := range allChoices {
			selected, _ := policyGroup(key)
			if selected == group {
				settings[key] = choice
				if choice.Source == "explicit" {
					resolvedSettings[key] = choice.Value
				}
			}
		}
		var digest *string
		// A partial hardware edit implicitly includes deployment choices for
		// its omitted dependent keys. Preview resolves those against the live
		// connection; a digest over only the persisted subset would falsely
		// mismatch the full reviewed candidate.
		if len(settings) == len(resolvedSettings) && (group != "hardware" || len(settings) == len(PolicyGroupKeys(group))) {
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
		if scope == "restart" {
			if _, err := tx.Exec(ctx, `INSERT INTO host_approval_review_tokens(host_id,group_key,review_id)
				VALUES($1::uuid,$2,gen_random_uuid()) ON CONFLICT(host_id,group_key) DO NOTHING`, hostID, group); err != nil {
				return empty, err
			}
			// A changed disruptive group invalidates only its own unstarted
			// approval. An offered grant keeps protection until the agent's
			// complete journal proves it never accepted the command.
			var approvalID, approvalState string
			approvalErr := tx.QueryRow(ctx, `SELECT id::text,state FROM host_config_approvals
				WHERE host_id=$1::uuid AND group_key=$2 AND state IN ('approved','offered')`, hostID, group).Scan(&approvalID, &approvalState)
			if approvalErr != nil && !errors.Is(approvalErr, pgx.ErrNoRows) {
				return empty, approvalErr
			}
			if approvalErr == nil {
				if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
					return empty, err
				}
				if err := tx.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE id=$1::uuid FOR UPDATE`, approvalID).Scan(&approvalState); err != nil {
					return empty, err
				}
				if approvalState == "approved" {
					if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='superseded' WHERE id=$1::uuid`, approvalID); err != nil {
						return empty, err
					}
					if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions
						WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, approvalID); err != nil {
						return empty, err
					}
				} else {
					if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending' WHERE id=$1::uuid`, approvalID); err != nil {
						return empty, err
					}
				}
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO host_reconcile_obligations(host_id,kind,resource_key,revision,next_attempt_at) VALUES($1::uuid,'setting',$2,$3,now()) ON CONFLICT(host_id,kind,resource_key) DO UPDATE SET revision=excluded.revision,next_attempt_at=now(),retry_count=0`, hostID, group, next); err != nil {
			return empty, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET status='online' WHERE id=$1::uuid AND status='draining'
		AND NOT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID); err != nil {
		return empty, err
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
