package hostcfg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrApprovalSuperseded = errors.New("idle approval no longer matches reviewed policy")
var ErrIdleAttemptConflict = errors.New("another disruptive host attempt is open")
var ErrIdleAttemptNotFound = errors.New("idle apply attempt not found")
var ErrIdleCancelTooLate = errors.New("idle apply already accepted")

// IdleApplyAttempt is the public projection of one owned waiting restriction.
// No field here claims that the setting has applied.
type IdleApplyAttempt struct {
	AttemptID           string     `json:"attempt_id"`
	Group               string     `json:"group"`
	Revision            string     `json:"revision"`
	ContentSHA256       string     `json:"content_sha256"`
	PrerequisitesSHA256 string     `json:"prerequisites_sha256"`
	Phase               string     `json:"phase"`
	Started             bool       `json:"started"`
	AdmissionRestricted bool       `json:"admission_restricted"`
	Remedy              *string    `json:"remedy"`
	NextRetryAt         *time.Time `json:"next_retry_at"`
}

type ApprovalFact struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type ApprovalPreview struct {
	Available               bool           `json:"available"`
	Revision                string         `json:"revision"`
	ContentSHA256           string         `json:"content_sha256"`
	Resolved                map[string]any `json:"resolved"`
	PrerequisitesSHA256     string         `json:"prerequisites_sha256"`
	Prerequisites           []ApprovalFact `json:"prerequisites"`
	ApprovalBootIncarnation string         `json:"approval_boot_incarnation"`
	ApprovalReviewID        string         `json:"approval_review_id"`
	Remedy                  *string        `json:"remedy"`
}

type ApprovalReview struct {
	ExpectedRevision        string         `json:"expected_revision"`
	ContentSHA256           string         `json:"content_sha256"`
	PrerequisitesSHA256     string         `json:"prerequisites_sha256"`
	Prerequisites           []ApprovalFact `json:"prerequisites"`
	ApprovalBootIncarnation string         `json:"approval_boot_incarnation"`
	ApprovalReviewID        string         `json:"approval_review_id"`
	ExpiresAt               time.Time      `json:"expires_at"`
}

// idleQueryDB lets the reviewed candidate use the approval transaction's
// connection. No preview query may borrow another pooled connection while the
// approval holds the host lock.
type idleQueryDB interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// PreviewIdleApply projects a restart group's already-saved content through
// its current source choices. An unresolved deployment or Automatic choice is
// not guessed from a catalog default or sysfs. Such a candidate has no grant.
func (s *Store) PreviewIdleApply(ctx context.Context, hostID, group string) (*ApprovalPreview, error) {
	return s.previewIdleApply(ctx, s.pool, hostID, group)
}

func (s *Store) previewIdleApply(ctx context.Context, db idleQueryDB, hostID, group string) (*ApprovalPreview, error) {
	return s.previewIdleApplyExcept(ctx, db, hostID, group, "")
}

// previewIdleApplyExcept is used only by the grant transaction after it has
// locked its own waiting approval. That row must not make its own unchanged
// candidate appear unavailable; every other disruptive operation still does.
func (s *Store) previewIdleApplyExcept(ctx context.Context, db idleQueryDB, hostID, group, ownApprovalID string) (*ApprovalPreview, error) {
	var revision int64
	var digest *string
	var scope, status string
	err := db.QueryRow(ctx, `SELECT desired_revision,desired_digest,scope,status FROM host_setting_groups
		WHERE host_id=$1::uuid AND group_key=$2`, hostID, group).Scan(&revision, &digest, &scope, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if scope != "restart" {
		return nil, nil
	}
	preview := &ApprovalPreview{Revision: strconv.FormatInt(revision, 10),
		Resolved: map[string]any{}, Prerequisites: []ApprovalFact{}}
	if digest != nil {
		preview.ContentSHA256 = *digest
	}
	if err := db.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&preview.ApprovalBootIncarnation); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var gateState, hostStatus string
	var gateBoot string
	var gateConnection *string
	err = db.QueryRow(ctx, `SELECT j.state,j.boot_incarnation::text,j.connection_incarnation::text,h.status
		FROM host_journal_reconciliation j JOIN hosts h ON h.id=j.host_id WHERE j.host_id=$1::uuid`, hostID).
		Scan(&gateState, &gateBoot, &gateConnection, &hostStatus)
	if errors.Is(err, pgx.ErrNoRows) || gateState != "complete" || gateBoot != preview.ApprovalBootIncarnation || gateConnection == nil || hostStatus == "offline" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshotKind, snapshotDigest string
	err = db.QueryRow(ctx, `SELECT kind,digest FROM host_journal_active_snapshots
		WHERE host_id=$1::uuid AND group_key=$2 AND connection_incarnation=$3::uuid`, hostID, group, *gateConnection).Scan(&snapshotKind, &snapshotDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	factKind := "seeded_group_digest"
	if snapshotKind == "verified" {
		factKind = "last_verified_group_digest"
	}
	preview.Prerequisites = append(preview.Prerequisites, ApprovalFact{Kind: factKind, ID: snapshotDigest})
	acceptedDigest, err := acceptedAttemptSetDigest(ctx, db, hostID, group)
	if err != nil {
		return nil, err
	}
	preview.Prerequisites = append(preview.Prerequisites, ApprovalFact{Kind: "accepted_attempts", ID: acceptedDigest})
	err = db.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens
		WHERE host_id=$1::uuid AND group_key=$2`, hostID, group).Scan(&preview.ApprovalReviewID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var disruptiveOpen bool
	if err := db.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM host_config_approvals WHERE host_id=$1::uuid AND state IN ('approved','offered','cancel_pending')
		AND ($2::text='' OR id<>NULLIF($2::text,'')::uuid)
		UNION ALL
		SELECT 1 FROM host_config_attempts t WHERE t.host_id=$1::uuid AND t.scope='restart'
		AND ($2::text='' OR t.id<>NULLIF($2::text,'')::uuid)
		AND (t.terminal_at IS NULL OR (t.phase='uncertain' AND EXISTS(
			SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=t.host_id AND r.owner_id=t.id
			AND r.owner_kind IN ('idle_apply','recovery')))))`, hostID, ownApprovalID).Scan(&disruptiveOpen); err != nil {
		return nil, err
	}
	if disruptiveOpen {
		return nil, nil
	}
	if status != "pending" && status != "failed" {
		return nil, nil
	}
	rows, err := db.Query(ctx, `SELECT key,source,explicit_value FROM host_setting_choices
		WHERE host_id=$1::uuid AND revision<=$2 ORDER BY key`, hostID, revision)
	if err != nil {
		return nil, err
	}
	type choiceRow struct {
		key, source string
		raw         []byte
	}
	var choices []choiceRow
	for rows.Next() {
		var row choiceRow
		if err := rows.Scan(&row.key, &row.source, &row.raw); err != nil {
			rows.Close()
			return nil, err
		}
		choices = append(choices, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// A saved hardware edit may mention only one key. The executor resolves the
	// whole dependent group, so every omitted member remains deployment-source
	// and must come from this connection's baseline. Include those members in
	// the reviewed content rather than offering a partial group the agent will
	// reject (or silently inherit from a stale active snapshot).
	if group == "hardware" {
		seen := map[string]bool{}
		for _, row := range choices {
			seen[row.key] = true
		}
		for _, key := range PolicyGroupKeys(group) {
			if !seen[key] {
				choices = append(choices, choiceRow{key: key, source: "deployment"})
			}
		}
	}
	settings := map[string]PolicyChoice{}
	deploymentValues := map[string]any{}
	automaticKeys := []string{}
	var baseline map[string]any
	for _, row := range choices {
		key, source, raw := row.key, row.source, row.raw
		selected, _ := policyGroup(key)
		if selected != group {
			continue
		}
		choice := PolicyChoice{Source: source}
		var value any
		switch source {
		case "explicit":
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, err
			}
			choice.Value = value
		case "deployment":
			if baseline == nil {
				baseline, err = deploymentSettingsForConnection(ctx, db, hostID, *gateConnection)
				if err != nil {
					return nil, err
				}
			}
			var ok bool
			value, ok = baseline[key]
			if !ok || (value == nil && !byKey()[key].Nullable) {
				return nil, nil
			}
			deploymentValues[key] = value
		case "automatic":
			automaticKeys = append(automaticKeys, key)
		default:
			return nil, ErrApprovalSuperseded
		}
		settings[key] = choice
		if source != "automatic" {
			preview.Resolved[key] = value
		}
	}
	if len(settings) == 0 {
		return nil, nil
	}
	if len(deploymentValues) != 0 {
		id, err := digestJSON(deploymentValues)
		if err != nil {
			return nil, err
		}
		preview.Prerequisites = append(preview.Prerequisites, ApprovalFact{Kind: "deployment_baseline", ID: id})
	}
	if len(automaticKeys) != 0 {
		selectedNode, _ := preview.Resolved["render_node"].(string)
		gpu, hardwareFacts, err := automaticHardwareEvidence(ctx, db, hostID, selectedNode)
		if err != nil {
			return nil, err
		}
		if gpu == nil {
			return nil, nil
		}
		for _, key := range automaticKeys {
			switch key {
			case "encoder":
				value := automaticEncoder(gpu.Vendor)
				if value == "" {
					return nil, nil
				}
				preview.Resolved[key] = value
			case "render_node":
				preview.Resolved[key] = gpu.RenderNode
			default:
				return nil, ErrApprovalSuperseded
			}
		}
		// This current media pass proves a usable device path. The candidate
		// encoder may differ from the current one; the restart verifier probes
		// the exact resolved candidate before recording application.
		preview.Prerequisites = append(preview.Prerequisites, hardwareFacts...)
	}
	sort.Slice(preview.Prerequisites, func(i, j int) bool {
		a, b := preview.Prerequisites[i], preview.Prerequisites[j]
		return a.Kind < b.Kind || (a.Kind == b.Kind && a.ID < b.ID)
	})
	facts := make([]any, 0, len(preview.Prerequisites))
	for _, fact := range preview.Prerequisites {
		facts = append(facts, map[string]any{"kind": fact.Kind, "id": fact.ID})
	}
	preview.PrerequisitesSHA256, err = digestPolicyFacts(facts)
	if err != nil {
		return nil, err
	}
	calculated, err := digestJSON(map[string]any{"group": group, "scope": scope, "revision": preview.Revision,
		"settings": settings, "resolved_settings": preview.Resolved})
	if err != nil {
		return nil, err
	}
	if preview.ContentSHA256 != "" && calculated != preview.ContentSHA256 {
		return nil, nil
	}
	preview.ContentSHA256 = calculated
	preview.Available = true
	return preview, nil
}

func acceptedAttemptSetDigest(ctx context.Context, db idleQueryDB, hostID, group string) (string, error) {
	rows, err := db.Query(ctx, `SELECT id::text,phase FROM host_config_attempts
		WHERE host_id=$1::uuid AND group_key=$2 AND started_at IS NOT NULL ORDER BY id::text`, hostID, group)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var id, phase string
		if err := rows.Scan(&id, &phase); err != nil {
			return "", err
		}
		if _, err := h.Write([]byte(id + "\x00" + phase + "\n")); err != nil {
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StartRH05Boot is called before HTTP admission starts. The boot identity and
// inventory holds commit together; a restored approval can never be reused by
// a later process incarnation. A sent offer keeps its hold until agent journal
// reconciliation proves whether acceptance happened.
func (s *Store) StartRH05Boot(ctx context.Context) (string, error) {
	boot := newPolicyAttemptID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `INSERT INTO rh05_control_boot(id,incarnation,started_at)
		VALUES(true,$1::uuid,now()) ON CONFLICT(id) DO UPDATE SET incarnation=excluded.incarnation,started_at=excluded.started_at`, boot); err != nil {
		return "", err
	}
	hostRows, err := tx.Query(ctx, `SELECT id::text FROM hosts ORDER BY id FOR UPDATE`)
	if err != nil {
		return "", err
	}
	var hostIDs []string
	for hostRows.Next() {
		var id string
		if err := hostRows.Scan(&id); err != nil {
			hostRows.Close()
			return "", err
		}
		hostIDs = append(hostIDs, id)
	}
	err = hostRows.Err()
	hostRows.Close()
	if err != nil {
		return "", err
	}
	for _, hostID := range hostIDs {
		if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
			return "", err
		}
	}
	// A stopped-stack database restore may have erased an offer committed after
	// the snapshot. Even an apparently waiting old approval keeps protection
	// until the current agent's complete journal proves nonacceptance.
	if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending'
		WHERE state IN ('approved','offered')`); err != nil {
		return "", err
	}
	// Every host that negotiated v2 or ever had a typed-owned group must prove
	// complete journal state after this boot before scheduling resumes.
	if _, err := tx.Exec(ctx, `INSERT INTO host_journal_reconciliation(host_id,boot_incarnation,state)
		SELECT id,$1::uuid,'pending' FROM hosts
		WHERE config_policy_versions->>'typed_settings'='2' OR config_policy_ever_owned_groups<>'[]'::jsonb
		ON CONFLICT(host_id) DO UPDATE SET boot_incarnation=excluded.boot_incarnation,
		connection_incarnation=NULL,state='pending',completed_at=NULL,continuation_cursor=NULL`, boot); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM host_idle_inventory`); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		SELECT host_id,'reconciliation','00000000-0000-0000-0000-000000000002'::uuid,'journal_reconciliation'
		FROM host_journal_reconciliation ON CONFLICT(host_id,owner_kind,owner_id) DO UPDATE SET reason='journal_reconciliation'`); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE hosts SET status='draining' WHERE status='online' AND
		EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=hosts.id)`); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return boot, nil
}

func (s *Store) GetIdleApply(ctx context.Context, hostID, attemptID string) (IdleApplyAttempt, error) {
	var result IdleApplyAttempt
	var revision int64
	var state string
	var attemptPhase *string
	var startedAt *time.Time
	var errorCode *string
	var held bool
	var expiry time.Time
	err := s.pool.QueryRow(ctx, `SELECT a.id::text,a.group_key,a.revision,a.approved_digest,a.prerequisites_digest,
		a.state,t.phase,t.started_at,t.error_code,a.expires_at,
		EXISTS(SELECT 1 FROM host_admission_restrictions r WHERE r.host_id=a.host_id AND r.owner_id=a.id AND r.owner_kind IN ('idle_apply','recovery'))
		FROM host_config_approvals a LEFT JOIN host_config_attempts t ON t.id=a.id
		WHERE a.host_id=$1::uuid AND a.id=$2::uuid`, hostID, attemptID).
		Scan(&result.AttemptID, &result.Group, &revision, &result.ContentSHA256, &result.PrerequisitesSHA256, &state, &attemptPhase, &startedAt, &errorCode, &expiry, &held)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrIdleAttemptNotFound
	}
	if err != nil {
		return result, err
	}
	result.Revision = strconv.FormatInt(revision, 10)
	result.Started = startedAt != nil
	result.AdmissionRestricted = held
	if attemptPhase != nil && *attemptPhase != "offered" {
		result.Phase = *attemptPhase
		if errorCode != nil && *errorCode != "" && (*attemptPhase == "failed" || *attemptPhase == "recovered" || *attemptPhase == "uncertain") {
			remedy := "Configuration attempt failed: " + safeIdleErrorCode(*errorCode) + ". Review host diagnostics before retrying."
			result.Remedy = &remedy
		}
		return result, nil
	}
	if attemptPhase != nil && *attemptPhase == "offered" && state == "offered" && errorCode != nil && *errorCode != "" {
		remedy := "Agent rejected one delivery: " + safeIdleErrorCode(*errorCode) + ". Admission remains protected while a complete authenticated journal checks whether another delivery began."
		result.Remedy = &remedy
	} else if attemptPhase != nil && *attemptPhase == "offered" && state == "offered" && !expiry.After(time.Now().UTC()) {
		remedy := "Offer deadline passed. Admission remains protected while a complete authenticated agent journal checks whether execution began."
		result.Remedy = &remedy
	} else if attemptPhase != nil && *attemptPhase == "offered" && state == "offered" {
		var retries int
		var next time.Time
		if err := s.pool.QueryRow(ctx, `SELECT retry_count,next_attempt_at FROM host_reconcile_obligations
			WHERE host_id=$1::uuid AND kind='idle_apply' AND resource_key=$2`, hostID, attemptID).Scan(&retries, &next); err == nil {
			if retries < 3 {
				result.NextRetryAt = &next
			} else {
				remedy := "Delivery retry budget exhausted. Admission remains protected while a complete agent journal checks whether execution began."
				result.Remedy = &remedy
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
	}
	switch state {
	case "approved":
		result.Phase = "waiting"
		remedy, err := s.idleWaitRemedy(ctx, hostID)
		if err != nil {
			return result, err
		}
		result.Remedy = &remedy
	case "expired", "superseded", "revoked_unstarted":
		result.Phase = "revoked_unstarted"
	case "cancel_pending":
		result.Phase = "cancel_pending"
		remedy := "Cancellation awaits a complete authenticated agent journal proving nonacceptance. Admission remains protected."
		result.Remedy = &remedy
	default:
		result.Phase = state
	}
	return result, nil
}

func (s *Store) CurrentIdleApply(ctx context.Context, hostID string) (IdleApplyAttempt, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM host_config_approvals
		WHERE host_id=$1::uuid AND state IN ('approved','offered','cancel_pending','accepted')
		ORDER BY created_at DESC LIMIT 1`, hostID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return IdleApplyAttempt{}, ErrIdleAttemptNotFound
	}
	if err != nil {
		return IdleApplyAttempt{}, err
	}
	return s.GetIdleApply(ctx, hostID, id)
}

// CancelIdleApply fences a never-offered approval and releases exactly its
// restriction in one host-locked transaction. An offered grant is retained as
// cancel_pending until an authenticated agent journal proves nonacceptance.
func (s *Store) CancelIdleApply(ctx context.Context, hostID, attemptID string, connected bool) (IdleApplyAttempt, error) {
	var empty IdleApplyAttempt
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var hostStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&hostStatus); errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrIdleAttemptNotFound
	} else if err != nil {
		return empty, err
	}
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, attemptID).Scan(&state); errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrIdleAttemptNotFound
	} else if err != nil {
		return empty, err
	}
	if state == "approved" || state == "offered" {
		if err := rotateHostReviewTokens(ctx, tx, hostID); err != nil {
			return empty, err
		}
	}
	if err := tx.QueryRow(ctx, `SELECT state FROM host_config_approvals WHERE host_id=$1::uuid AND id=$2::uuid FOR UPDATE`, hostID, attemptID).Scan(&state); err != nil {
		return empty, err
	}
	switch state {
	case "approved":
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='revoked_unstarted' WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, attemptID); err != nil {
			return empty, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM host_admission_restrictions WHERE host_id=$1::uuid AND owner_kind='idle_apply' AND owner_id=$2::uuid`, hostID, attemptID); err != nil {
			return empty, err
		}
		var held bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_admission_restrictions WHERE host_id=$1::uuid)`, hostID).Scan(&held); err != nil {
			return empty, err
		}
		if !held && hostStatus == "draining" {
			next := "offline"
			if connected {
				next = "online"
			}
			if _, err := tx.Exec(ctx, `UPDATE hosts SET status=$2 WHERE id=$1::uuid`, hostID, next); err != nil {
				return empty, err
			}
		}
	case "offered":
		if _, err := tx.Exec(ctx, `UPDATE host_config_approvals SET state='cancel_pending' WHERE host_id=$1::uuid AND id=$2::uuid`, hostID, attemptID); err != nil {
			return empty, err
		}
	case "cancel_pending", "revoked_unstarted", "expired", "superseded":
		// Repeated cancellation cannot release a different owner's restriction.
	case "accepted":
		return empty, ErrIdleCancelTooLate
	default:
		return empty, ErrIdleAttemptConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return s.GetIdleApply(ctx, hostID, attemptID)
}

// ApproveIdleApply rechecks a reviewed candidate before acquiring its hold.
func (s *Store) ApproveIdleApply(ctx context.Context, hostID, group string, review ApprovalReview) (IdleApplyAttempt, error) {
	var empty IdleApplyAttempt
	if group == "" || !validPolicyDigest(review.ContentSHA256) || !validPolicyDigest(review.PrerequisitesSHA256) ||
		review.ApprovalBootIncarnation == "" || review.ApprovalReviewID == "" ||
		review.ExpiresAt.IsZero() || !review.ExpiresAt.After(time.Now().UTC()) {
		return empty, ErrApprovalSuperseded
	}
	// PostgreSQL timestamps have microsecond precision. Normalize the parsed
	// RFC3339 instant once so retry comparison survives persistence.
	review.ExpiresAt = review.ExpiresAt.UTC().Truncate(time.Microsecond)
	revision, err := strconv.ParseInt(review.ExpectedRevision, 10, 64)
	if err != nil || revision < 0 || strconv.FormatInt(revision, 10) != review.ExpectedRevision {
		return empty, ErrApprovalSuperseded
	}
	// Recheck current server-derived content before acquiring admission. The
	// host row lock serializes approval with settings edits and reservations.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM hosts WHERE id=$1::uuid FOR UPDATE`, hostID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrHostNotFound
	} else if err != nil {
		return empty, err
	}
	var boot string
	if err := tx.QueryRow(ctx, `SELECT incarnation::text FROM rh05_control_boot WHERE id=true`).Scan(&boot); err != nil {
		return empty, err
	}
	if boot != review.ApprovalBootIncarnation || !review.ExpiresAt.After(time.Now().UTC()) {
		return empty, ErrApprovalSuperseded
	}
	// Review IDs are durable per grant. Classify an old identical retry before
	// considering a new grant, even after the current review token has rotated.
	var priorID, priorState string
	var priorRevision int64
	var priorDigest, priorPrereq, priorBoot string
	var priorExpiry time.Time
	err = tx.QueryRow(ctx, `SELECT id::text,state,revision,approved_digest,prerequisites_digest,
		boot_incarnation::text,expires_at FROM host_config_approvals WHERE host_id=$1::uuid
		AND group_key=$2 AND review_id=$3::uuid`, hostID, group, review.ApprovalReviewID).
		Scan(&priorID, &priorState, &priorRevision, &priorDigest, &priorPrereq, &priorBoot, &priorExpiry)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if err == nil {
		identical := priorRevision == revision && priorDigest == review.ContentSHA256 &&
			priorPrereq == review.PrerequisitesSHA256 && priorBoot == boot && priorExpiry.Equal(review.ExpiresAt)
		if identical && (priorState == "approved" || priorState == "offered") {
			if err := tx.Commit(ctx); err != nil {
				return empty, err
			}
			return s.GetIdleApply(ctx, hostID, priorID)
		}
		if priorState == "cancel_pending" || priorState == "revoked_unstarted" ||
			priorState == "expired" || priorState == "superseded" {
			return empty, ErrApprovalSuperseded
		}
	}
	var existingID *string
	err = tx.QueryRow(ctx, `SELECT id::text FROM host_config_approvals
		WHERE host_id=$1::uuid AND state IN ('approved','offered','cancel_pending') ORDER BY created_at LIMIT 1`, hostID).
		Scan(&existingID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return empty, err
	}
	if existingID != nil {
		return empty, ErrIdleAttemptConflict
	}
	var started bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_config_attempts WHERE host_id=$1::uuid AND scope='restart' AND terminal_at IS NULL)`, hostID).Scan(&started); err != nil {
		return empty, err
	}
	if started {
		return empty, ErrIdleAttemptConflict
	}
	var protectedUncertain bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM host_config_attempts t
		JOIN host_admission_restrictions r ON r.host_id=t.host_id AND r.owner_id=t.id
		WHERE t.host_id=$1::uuid AND t.scope='restart' AND t.phase='uncertain'
		AND r.owner_kind IN ('idle_apply','recovery'))`, hostID).Scan(&protectedUncertain); err != nil {
		return empty, err
	}
	if protectedUncertain {
		return empty, ErrIdleAttemptConflict
	}
	var currentReviewID string
	if err := tx.QueryRow(ctx, `SELECT review_id::text FROM host_approval_review_tokens
		WHERE host_id=$1::uuid AND group_key=$2 FOR UPDATE`, hostID, group).Scan(&currentReviewID); errors.Is(err, pgx.ErrNoRows) {
		return empty, ErrApprovalSuperseded
	} else if err != nil {
		return empty, err
	}
	if currentReviewID != review.ApprovalReviewID {
		return empty, ErrApprovalSuperseded
	}
	preview, err := s.previewIdleApply(ctx, tx, hostID, group)
	if err != nil {
		return empty, err
	}
	if preview == nil || !preview.Available || preview.Revision != review.ExpectedRevision ||
		preview.ContentSHA256 != review.ContentSHA256 || preview.PrerequisitesSHA256 != review.PrerequisitesSHA256 ||
		preview.ApprovalReviewID != review.ApprovalReviewID || preview.ApprovalBootIncarnation != boot ||
		!reflect.DeepEqual(preview.Prerequisites, review.Prerequisites) {
		return empty, ErrApprovalSuperseded
	}
	// A partial hardware edit (including a fresh Automatic default) has no
	// durable desired digest until this reviewed, current-connection preview
	// resolves its deployment members. Bind the complete candidate now so the
	// eventual verified journal can advance the group to applied.
	cmd, err := tx.Exec(ctx, `UPDATE host_setting_groups SET desired_digest=$4
		WHERE host_id=$1::uuid AND group_key=$2 AND desired_revision=$3 AND scope='restart'
		AND status IN ('pending','failed')`, hostID, group, revision, preview.ContentSHA256)
	if err != nil {
		return empty, err
	}
	if cmd.RowsAffected() != 1 {
		return empty, ErrApprovalSuperseded
	}
	id := newPolicyAttemptID()
	if _, err := tx.Exec(ctx, `INSERT INTO host_config_approvals(id,host_id,group_key,revision,approved_digest,prerequisites_digest,boot_incarnation,review_id,expires_at,state)
		VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7::uuid,$8::uuid,$9,'approved')`, id, hostID, group, revision, review.ContentSHA256, review.PrerequisitesSHA256, boot, review.ApprovalReviewID, review.ExpiresAt); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "host_config_approvals_one_live_host" {
			return empty, ErrIdleAttemptConflict
		}
		return empty, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_admission_restrictions(host_id,owner_kind,owner_id,reason)
		VALUES($1::uuid,'idle_apply',$2::uuid,'idle_configuration')`, hostID, id); err != nil {
		return empty, err
	}
	if status == "online" {
		if _, err := tx.Exec(ctx, `UPDATE hosts SET status='draining' WHERE id=$1::uuid`, hostID); err != nil {
			return empty, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return s.GetIdleApply(ctx, hostID, id)
}
