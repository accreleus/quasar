package images

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/agentws"
	"github.com/accreleus/quasar/control-plane/internal/audit"
)

// CleanupTransport exposes only the authenticated, current-connection facts
// needed to make an exact cleanup decision. Its production implementation is
// agentws.Registry; tests can replace the daemon boundary deterministically.
type CleanupTransport interface {
	ImageCleanupSnapshot(hostID string) (agentws.ImageCleanupSnapshot, bool)
	SendImageCleanup(context.Context, string, agentws.ImageCleanupCmd) (agentws.AckResult, error)
	SendImageInventoryReconcile(string, agentws.ImageInventoryReconcileCmd) error
	SendImageCleanupJournalRequest(string, agentws.ImageCleanupJournalRequestCmd) error
	SendImageCleanupStateAck(string, agentws.ImageCleanupStateAckCmd) error
}

type CleanupService struct {
	pool    *pgxpool.Pool
	wire    CleanupTransport
	ensurer interface {
		EnsureHost(context.Context, string) error
		RetryHostImage(context.Context, string, string) error
	}
	auditor            audit.Recorder
	mu                 sync.Mutex
	journalRequests    map[string]pendingCleanupJournal
	inventorySnapshots map[string]trackedCleanupInventory
	freshReconcile     map[string]cleanupReconcileGate
}

type cleanupReconcileGate struct {
	connectionID string
	baseline     uint64
}

type trackedCleanupInventory struct {
	connectionID string
	complete     bool
	versions     map[string]string
}

type pendingCleanupJournal struct {
	hostID        string
	connectionID  string
	requested     map[string]bool
	createdAt     time.Time
	freshRevision uint64
}

func NewCleanupService(pool *pgxpool.Pool, wire CleanupTransport) *CleanupService {
	return &CleanupService{pool: pool, wire: wire, journalRequests: make(map[string]pendingCleanupJournal),
		inventorySnapshots: make(map[string]trackedCleanupInventory), freshReconcile: make(map[string]cleanupReconcileGate)}
}

func (s *CleanupService) SetEnsurer(e interface {
	EnsureHost(context.Context, string) error
	RetryHostImage(context.Context, string, string) error
}) {
	s.ensurer = e
}
func (s *CleanupService) SetAuditor(a audit.Recorder) { s.auditor = a }

func newCleanupAttemptID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

type CleanupCandidate struct {
	ImageID        string   `json:"image_id"`
	Version        string   `json:"version"`
	ImageRef       string   `json:"image_ref"`
	RuntimeImageID string   `json:"runtime_image_id"`
	Eligible       bool     `json:"eligible"`
	Reasons        []string `json:"reasons"`
	Remedy         *string  `json:"remedy"`
	Generation     string   `json:"generation"`
}

type CleanupView struct {
	HostID          string             `json:"host_id"`
	InventoryStatus string             `json:"inventory_status"`
	ObservedAt      *time.Time         `json:"observed_at"`
	Remedy          *string            `json:"remedy"`
	Images          []CleanupCandidate `json:"images"`
}

type CleanupRequest struct {
	ImageID            string `json:"image_id"`
	Version            string `json:"version"`
	ImageRef           string `json:"image_ref"`
	RuntimeImageID     string `json:"runtime_image_id"`
	ExpectedGeneration string `json:"expected_generation"`
}

type CleanupAttempt struct {
	AttemptID      string  `json:"attempt_id"`
	ImageID        string  `json:"image_id"`
	Version        string  `json:"version"`
	ImageRef       string  `json:"image_ref"`
	RuntimeImageID string  `json:"runtime_image_id"`
	Generation     string  `json:"generation"`
	State          string  `json:"state"`
	Reason         *string `json:"reason"`
}

type CleanupConflict struct {
	Code    string
	Message string
	Remedy  string
	Current *CleanupCandidate
}

var errCleanupNotFound = errors.New("host or managed image not found")
var errCleanupValidation = errors.New("invalid cleanup request")

func strptr(s string) *string { return &s }

func (s *CleanupService) Preview(ctx context.Context, hostID string) (CleanupView, error) {
	view := CleanupView{HostID: hostID, Images: []CleanupCandidate{}}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts WHERE id=$1::uuid)`, hostID).Scan(&exists); err != nil {
		return view, err
	}
	if !exists {
		return view, errCleanupNotFound
	}
	snap, connected := s.wire.ImageCleanupSnapshot(hostID)
	if !connected {
		view.InventoryStatus, view.Remedy = "offline", strptr("Reconnect the host and refresh image inventory")
		return view, nil
	}
	if !snap.Capable || !snap.Complete {
		view.InventoryStatus, view.Remedy = "unknown", strptr("Reconnect a cleanup-capable agent and repair its managed-image inventory")
		return view, nil
	}
	view.InventoryStatus = "current"
	if !snap.ObservedAt.IsZero() {
		t := snap.ObservedAt
		view.ObservedAt = &t
	}
	for _, v := range snap.Versions {
		if v.State != "present" {
			continue
		}
		candidate, err := s.candidate(ctx, s.pool, hostID, v)
		if err != nil {
			return view, err
		}
		view.Images = append(view.Images, candidate)
	}
	return view, nil
}

// Attempt reads only the durable control-plane record. It never infers a
// physical outcome from preview inventory or contacts the agent.
func (s *CleanupService) Attempt(ctx context.Context, hostID, attemptID string) (CleanupAttempt, error) {
	var a CleanupAttempt
	var generation int64
	err := s.pool.QueryRow(ctx, `SELECT id::text,image_id,version,image_ref,runtime_image_id,generation,state,reason
		FROM host_image_cleanup_attempts WHERE id=$1::uuid AND host_id=$2::uuid`, attemptID, hostID).Scan(
		&a.AttemptID, &a.ImageID, &a.Version, &a.ImageRef, &a.RuntimeImageID, &generation, &a.State, &a.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, errCleanupNotFound
	}
	if err != nil {
		return a, err
	}
	a.Generation = strconv.FormatInt(generation, 10)
	a.Reason = safeStoredCleanupReason(a.Reason)
	return a, nil
}

func safeStoredCleanupReason(reason *string) *string {
	if reason == nil {
		return nil
	}
	switch *reason {
	case "identity_mismatch", "inventory_unknown", "reference_in_use", "operation_busy", "unsupported", "image_still_present":
		return reason
	default:
		return nil
	}
}

// candidate is intentionally the same blocker computation for GET and POST.
// POST calls it with a transaction holding the target fence FOR UPDATE.
func (s *CleanupService) candidate(ctx context.Context, db dbExecutor, hostID string, v agentws.ImageVersionEntry) (CleanupCandidate, error) {
	c := CleanupCandidate{ImageID: v.ImageID, Version: v.Version, ImageRef: v.ImageRef,
		RuntimeImageID: v.RuntimeImageID, Reasons: []string{}, Generation: "0"}
	var generation int64
	var fenceState string
	err := db.QueryRow(ctx, `SELECT generation,state FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id=$2`, hostID, v.ImageID).Scan(&generation, &fenceState)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return c, err
	}
	if err == nil {
		c.Generation = strconv.FormatInt(generation, 10)
		if fenceState == "removing" {
			c.Reasons = append(c.Reasons, "removing")
		}
	}
	// The selected effective app reference is the requirement, including a
	// catalog-pruned image that still has a selected app. Exact ref comparison
	// protects other managed IDs that adopted the same daemon object.
	var required, containerSession, pendingLaunch, pendingImage, pendingTemplate, previous bool
	err = db.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM apps a JOIN apps effective ON effective.id=COALESCE(a.parent_app_id,a.id)
		JOIN app_placement ap ON ap.app_id=effective.id
		LEFT JOIN runtime_presets rp ON rp.id=effective.runtime_preset_id
		WHERE a.enabled AND effective.enabled
		AND COALESCE(NULLIF(effective.runtime_spec->>'image',''),NULLIF(rp.image,''))=$2
		AND (ap.mode='all_eligible' OR EXISTS(
			SELECT 1 FROM app_placement_hosts aph WHERE aph.app_id=ap.app_id AND aph.host_id=$1::uuid
		))
	), EXISTS(
		SELECT 1 FROM sessions se JOIN apps a ON a.id=se.app_id
		JOIN apps effective ON effective.id=COALESCE(a.parent_app_id,a.id)
		LEFT JOIN runtime_presets rp ON rp.id=effective.runtime_preset_id
		WHERE se.host_id=$1::uuid AND se.state IN ('running','stopping')
		AND COALESCE(NULLIF(effective.runtime_spec->>'image',''),NULLIF(rp.image,''))=$2
	), EXISTS(
		SELECT 1 FROM sessions se JOIN apps a ON a.id=se.app_id
		JOIN apps effective ON effective.id=COALESCE(a.parent_app_id,a.id)
		LEFT JOIN runtime_presets rp ON rp.id=effective.runtime_preset_id
		WHERE se.host_id=$1::uuid AND se.state IN ('pending','assigned','starting')
		AND COALESCE(NULLIF(effective.runtime_spec->>'image',''),NULLIF(rp.image,''))=$2
	), EXISTS(
		SELECT 1 FROM host_images hi WHERE hi.host_id=$1::uuid AND hi.image_id=$3
		AND hi.state IN ('pulling','building')
	), EXISTS(
		SELECT 1 FROM job_runs jr WHERE jr.host_id=$1::uuid AND jr.state IN ('pending','running')
		AND jr.job_id LIKE 'template.%' AND (jr.params->>'image_id'=$3 OR jr.params->>'image_id' IS NULL)
	), EXISTS(
		SELECT 1 FROM host_image_success_history h WHERE h.host_id=$1::uuid AND
		COALESCE(NULLIF(h.previous_identity->>'registry_ref',''),NULLIF(h.previous_identity->>'local_tag',''))=$2
	)`, hostID, v.ImageRef, v.ImageID).Scan(&required, &containerSession, &pendingLaunch, &pendingImage, &pendingTemplate, &previous)
	if err != nil {
		return c, err
	}
	if required {
		c.Reasons = append(c.Reasons, "required")
	}
	if containerSession {
		c.Reasons = append(c.Reasons, "container_reference")
	}
	if pendingLaunch {
		c.Reasons = append(c.Reasons, "pending_launch")
	}
	if pendingImage {
		c.Reasons = append(c.Reasons, "pending_image_operation")
	}
	if pendingTemplate {
		c.Reasons = append(c.Reasons, "pending_template_work")
	}
	if previous {
		c.Reasons = append(c.Reasons, "retained_previous_success")
	}
	c.Eligible = len(c.Reasons) == 0
	if !c.Eligible {
		c.Remedy = strptr(cleanupReasonRemedy(c.Reasons[0]))
	}
	return c, nil
}

func cleanupReasonRemedy(reason string) string {
	switch reason {
	case "required":
		return "Remove the app requirement before cleaning this version"
	case "container_reference":
		return "Stop and remove containers using this image before retrying"
	case "pending_launch":
		return "Wait for the session to finish before retrying"
	case "pending_image_operation", "pending_template_work":
		return "Wait for image work to finish before retrying"
	case "retained_previous_success":
		return "Keep this recovery version until a newer successful version replaces it"
	case "removing":
		return "Wait for the current cleanup attempt to reconcile"
	case "offline":
		return "Reconnect the host and refresh the preview"
	default:
		return "Reconnect the host and refresh image inventory"
	}
}

func cleanupReasonMessage(reason string) string {
	switch reason {
	case "required":
		return "Image is required by an app"
	case "container_reference":
		return "Image is used by a container"
	case "pending_launch", "pending_image_operation", "pending_template_work":
		return "Image has pending launch, image or template work"
	case "retained_previous_success":
		return "Image is retained for recovery"
	case "removing":
		return "Image cleanup is already in progress"
	case "offline":
		return "Host is offline; retry after reconnect"
	default:
		return "Image inventory is unknown; reconnect the host"
	}
}

func staleCleanupConflict(current *CleanupCandidate) *CleanupConflict {
	return &CleanupConflict{Code: "stale_preview", Message: "Image cleanup preview is stale; refresh and try again",
		Remedy: "Refresh the preview and confirm the exact version again", Current: current}
}

func reasonCleanupConflict(reason string, current *CleanupCandidate) *CleanupConflict {
	return &CleanupConflict{Code: reason, Message: cleanupReasonMessage(reason),
		Remedy: cleanupReasonRemedy(reason), Current: current}
}

func (s *CleanupService) Request(ctx context.Context, hostID string, req CleanupRequest) (CleanupAttempt, int, *CleanupConflict, error) {
	var zero CleanupAttempt
	if req.ImageID == "" || len(req.ImageID) > 128 || req.Version == "" || len(req.Version) > 128 ||
		req.ImageRef == "" || len(req.ImageRef) > 1024 || req.RuntimeImageID == "" || len(req.RuntimeImageID) > 256 {
		return zero, 0, nil, errCleanupValidation
	}
	expected, err := strconv.ParseInt(req.ExpectedGeneration, 10, 64)
	if err != nil || expected < 0 || strconv.FormatInt(expected, 10) != req.ExpectedGeneration || expected == int64(^uint64(0)>>1) {
		return zero, 0, nil, errCleanupValidation
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hosts WHERE id=$1::uuid)`, hostID).Scan(&exists); err != nil {
		return zero, 0, nil, err
	}
	if !exists {
		return zero, 0, nil, errCleanupNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, 0, nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// App/placement writers take this same ref-specific transaction lock after
	// their app row. It also covers catalog-pruned versions that have no DB ref
	// mapping until this first exact cleanup request creates a fence.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(4,hashtext($1::text))`, req.ImageRef); err != nil {
		return zero, 0, nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO host_image_operation_fences(host_id,image_id,state)
		VALUES($1::uuid,$2,'idle') ON CONFLICT DO NOTHING`, hostID, req.ImageID); err != nil {
		return zero, 0, nil, err
	}
	var currentGen int64
	if err = tx.QueryRow(ctx, `SELECT generation FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id=$2 FOR UPDATE`, hostID, req.ImageID).Scan(&currentGen); err != nil {
		return zero, 0, nil, err
	}
	// An HTTP retry after a lost 202 observes the same persisted attempt even
	// when its preview generation is now stale or a new requirement arrived.
	var oldGen int64
	err = tx.QueryRow(ctx, `SELECT id::text,generation,state,reason FROM host_image_cleanup_attempts
		WHERE host_id=$1::uuid AND image_id=$2 AND version=$3 AND image_ref=$4 AND runtime_image_id=$5
		AND generation=$6 AND state IN ('removing','unknown','removed')
		ORDER BY updated_at DESC LIMIT 1`, hostID, req.ImageID, req.Version, req.ImageRef,
		req.RuntimeImageID, expected+1).Scan(&zero.AttemptID, &oldGen, &zero.State, &zero.Reason)
	if err == nil {
		zero.ImageID, zero.Version, zero.ImageRef, zero.RuntimeImageID = req.ImageID, req.Version, req.ImageRef, req.RuntimeImageID
		zero.Generation = strconv.FormatInt(oldGen, 10)
		zero.Reason = safeStoredCleanupReason(zero.Reason)
		status := 202
		if zero.State == "removed" {
			status = 200
		} else {
			// This may be a lost dispatch or lost acknowledgement on the same
			// connection. Release the fence row before asking the agent for its
			// durable journal and fresh inventory; never create a new attempt or
			// send a second physical deletion from this HTTP retry.
			if err := tx.Rollback(ctx); err != nil {
				return CleanupAttempt{}, 0, nil, err
			}
			s.ImageCleanupRegistered(ctx, hostID)
		}
		return zero, status, nil, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return zero, 0, nil, err
	}
	snap, connected := s.wire.ImageCleanupSnapshot(hostID)
	if !connected {
		return zero, 0, reasonCleanupConflict("offline", nil), nil
	}
	if !snap.Capable || !snap.Complete {
		return zero, 0, reasonCleanupConflict("unknown_inventory", nil), nil
	}
	var entry *agentws.ImageVersionEntry
	for i := range snap.Versions {
		v := &snap.Versions[i]
		if v.ImageID == req.ImageID && v.Version == req.Version && v.State == "present" {
			entry = v
			break
		}
	}
	if entry == nil {
		// A known catalog/fence/history identity is stale. An arbitrary unknown
		// image ID stays 404 rather than revealing current inventory details.
		var known bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_catalog WHERE id=$1)
			OR EXISTS(SELECT 1 FROM host_image_success_history WHERE host_id=$2::uuid AND image_id=$1)
			OR EXISTS(SELECT 1 FROM host_image_cleanup_attempts WHERE host_id=$2::uuid AND image_id=$1)`, req.ImageID, hostID).Scan(&known); err != nil {
			return zero, 0, nil, err
		}
		if !known {
			return zero, 0, nil, errCleanupNotFound
		}
		return zero, 0, staleCleanupConflict(nil), nil
	}
	candidate, err := s.candidate(ctx, tx, hostID, *entry)
	if err != nil {
		return zero, 0, nil, err
	}
	if currentGen != expected || entry.ImageRef != req.ImageRef || entry.RuntimeImageID != req.RuntimeImageID {
		return zero, 0, staleCleanupConflict(&candidate), nil
	}
	if !candidate.Eligible {
		return zero, 0, reasonCleanupConflict(candidate.Reasons[0], &candidate), nil
	}
	if latest, online := s.wire.ImageCleanupSnapshot(hostID); !online || latest.ConnectionID != snap.ConnectionID || !latest.Complete || !sameImageVersion(latest.Versions, *entry) {
		return zero, 0, staleCleanupConflict(&candidate), nil
	}
	attemptID, err := newCleanupAttemptID()
	if err != nil {
		return zero, 0, nil, err
	}
	newGeneration := currentGen + 1
	if _, err := tx.Exec(ctx, `UPDATE host_image_operation_fences SET generation=$3,state='removing',attempt_id=$4::uuid
		WHERE host_id=$1::uuid AND image_id=$2`, hostID, req.ImageID, newGeneration, attemptID); err != nil {
		return zero, 0, nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO host_image_cleanup_attempts
		(id,host_id,image_id,version,image_ref,runtime_image_id,generation,state,created_at,updated_at)
		VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7,'removing',now(),now())`,
		attemptID, hostID, req.ImageID, req.Version, req.ImageRef, req.RuntimeImageID, newGeneration); err != nil {
		return zero, 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, 0, nil, err
	}
	attempt := CleanupAttempt{AttemptID: attemptID, ImageID: req.ImageID, Version: req.Version,
		ImageRef: req.ImageRef, RuntimeImageID: req.RuntimeImageID, Generation: strconv.FormatInt(newGeneration, 10), State: "removing"}
	// A lost dispatch is recovered from this durable attempt after reconnect.
	// No database lock is held during the agent call.
	go s.dispatch(attempt, hostID)
	return attempt, 202, nil, nil
}

func sameImageVersion(versions []agentws.ImageVersionEntry, expected agentws.ImageVersionEntry) bool {
	for _, v := range versions {
		if v == expected {
			return true
		}
	}
	return false
}

func (s *CleanupService) dispatch(a CleanupAttempt, hostID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	id, err := newCmdID()
	if err != nil {
		return
	}
	result, err := s.wire.SendImageCleanup(ctx, hostID, agentws.ImageCleanupCmd{
		ID: id, AttemptID: a.AttemptID, ImageID: a.ImageID, Version: a.Version,
		ImageRef: a.ImageRef, RuntimeImageID: a.RuntimeImageID, ExpectedGeneration: a.Generation,
	})
	if err != nil {
		persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer persistCancel()
		_, _ = s.pool.Exec(persistCtx, `UPDATE host_image_cleanup_attempts SET state='unknown',updated_at=now()
			WHERE id=$1::uuid AND state='removing'`, a.AttemptID)
		return
	}
	if !result.OK && result.Error == "retired_attempt" {
		// This only describes the duplicate receipt. The original physical
		// outcome still needs journal retirement plus fresh inventory proof.
		s.ImageCleanupRegistered(ctx, hostID)
		return
	}
	if !result.OK && result.Error != "retired_attempt" {
		if safeCleanupRefusal(result.Error) {
			// The capable agent durably journaled this first refusal before its
			// ack. Process it through the same exact-identity CAS as a report.
			reason := result.Error
			persistCtx, persistCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer persistCancel()
			s.ImageCleanupState(persistCtx, hostID, agentws.ImageCleanupStateMsg{
				AttemptID: a.AttemptID, ImageID: a.ImageID, Version: a.Version, ImageRef: a.ImageRef,
				RuntimeImageID: a.RuntimeImageID, Generation: a.Generation, State: "failed", Reason: &reason,
			})
		}
	}
}

func (s *CleanupService) changedInventoryIDs(hostID string) []string {
	snap, online := s.wire.ImageCleanupSnapshot(hostID)
	if !online {
		return nil
	}
	parts := map[string][]string{}
	for _, v := range snap.Versions {
		parts[v.ImageID] = append(parts[v.ImageID], v.Version+"\x00"+v.ImageRef+"\x00"+v.RuntimeImageID+"\x00"+v.State)
	}
	current := trackedCleanupInventory{connectionID: snap.ConnectionID, complete: snap.Complete, versions: map[string]string{}}
	for id, items := range parts {
		sort.Strings(items)
		current.versions[id] = fmt.Sprint(items)
	}
	s.mu.Lock()
	previous, had := s.inventorySnapshots[hostID]
	s.inventorySnapshots[hostID] = current
	s.mu.Unlock()
	changed := map[string]bool{}
	for id, fingerprint := range current.versions {
		if !had || previous.connectionID != current.connectionID || previous.complete != current.complete || previous.versions[id] != fingerprint {
			changed[id] = true
		}
	}
	for id := range previous.versions {
		if _, ok := current.versions[id]; !ok {
			changed[id] = true
		}
	}
	ids := make([]string, 0, len(changed))
	for id := range changed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *CleanupService) bumpInventoryFences(ctx context.Context, hostID string) {
	ids := s.changedInventoryIDs(hostID)
	if len(ids) == 0 {
		return
	}
	if _, err := s.pool.Exec(ctx, `UPDATE host_image_operation_fences SET generation=generation+1
		WHERE host_id=$1::uuid AND image_id=ANY($2::text[]) AND state='idle'`, hostID, ids); err != nil {
		s.mu.Lock()
		delete(s.inventorySnapshots, hostID)
		s.mu.Unlock()
	}
}

func (s *CleanupService) ImageCleanupRegistered(ctx context.Context, hostID string) {
	s.bumpInventoryFences(ctx, hostID)
	snap, connected := s.wire.ImageCleanupSnapshot(hostID)
	if !connected || !snap.Capable {
		return
	}
	// Reconcile frozen managed identities, including unresolved attempts that
	// survived catalog pruning, before trusting upgraded inventory.
	rows, err := s.pool.Query(ctx, `SELECT ii.image_id,ii.version,COALESCE(NULLIF(ii.registry_ref,''),NULLIF(ii.local_tag,''))
		FROM installed_images ii WHERE ii.registry_ref IS NOT NULL OR ii.local_tag IS NOT NULL
		UNION SELECT h.image_id,h.current_version,COALESCE(NULLIF(h.current_identity->>'registry_ref',''),NULLIF(h.current_identity->>'local_tag',''))
		FROM host_image_success_history h WHERE h.host_id=$1::uuid
		UNION SELECT h.image_id,h.previous_version,COALESCE(NULLIF(h.previous_identity->>'registry_ref',''),NULLIF(h.previous_identity->>'local_tag',''))
		FROM host_image_success_history h WHERE h.host_id=$1::uuid AND h.previous_version IS NOT NULL
		UNION SELECT a.image_id,a.version,a.image_ref FROM host_image_cleanup_attempts a
		WHERE a.host_id=$1::uuid AND a.state IN ('removing','unknown')`, hostID)
	if err != nil {
		return
	}
	var identities []agentws.ImageInventoryIdentity
	for rows.Next() {
		var identity agentws.ImageInventoryIdentity
		if rows.Scan(&identity.ImageID, &identity.Version, &identity.ImageRef) == nil && identity.ImageRef != "" {
			identities = append(identities, identity)
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return
	}
	id, err := newCmdID()
	if err != nil {
		return
	}
	s.mu.Lock()
	// A newer reconcile supersedes any journal snapshot requested against an
	// older daemon scan on this same connection.
	for oldID, old := range s.journalRequests {
		if old.hostID == hostID {
			delete(s.journalRequests, oldID)
		}
	}
	s.freshReconcile[hostID] = cleanupReconcileGate{connectionID: snap.ConnectionID, baseline: snap.ReconciledRevision}
	s.mu.Unlock()
	if s.wire.SendImageInventoryReconcile(hostID, agentws.ImageInventoryReconcileCmd{ID: id, Identities: identities}) != nil {
		s.mu.Lock()
		delete(s.freshReconcile, hostID)
		s.mu.Unlock()
		return
	}
	s.requestCleanupJournal(ctx, hostID)
}

// Every unresolved attempt must be reconciled from the complete durable
// journal, including missing-ID retirement proof before an absent result.
func (s *CleanupService) requestCleanupJournal(ctx context.Context, hostID string) {
	snap, connected := s.wire.ImageCleanupSnapshot(hostID)
	if !connected || !snap.Capable {
		return
	}
	s.mu.Lock()
	gate, hasGate := s.freshReconcile[hostID]
	s.mu.Unlock()
	if hasGate && (gate.connectionID != snap.ConnectionID || snap.ReconciledRevision <= gate.baseline) {
		// A journal can race ahead of the daemon scan. Do not ask for a
		// retirement proof until the reconcile ack's later inventory revision.
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM host_image_cleanup_attempts
		WHERE host_id=$1::uuid AND state IN ('removing','unknown')`, hostID)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) > 0 {
		if id, err := newCmdID(); err == nil {
			requested := make(map[string]bool, len(ids))
			for _, attemptID := range ids {
				requested[attemptID] = true
			}
			s.mu.Lock()
			currentGate, stillHasGate := s.freshReconcile[hostID]
			if stillHasGate != hasGate || hasGate && currentGate != gate {
				s.mu.Unlock()
				return // a newer daemon scan superseded this journal request
			}
			for oldID, old := range s.journalRequests {
				if old.hostID == hostID || time.Since(old.createdAt) > 10*time.Minute {
					delete(s.journalRequests, oldID)
				}
			}
			freshRevision := uint64(0)
			if hasGate {
				freshRevision = snap.ReconciledRevision
			}
			s.journalRequests[id] = pendingCleanupJournal{hostID: hostID, connectionID: snap.ConnectionID,
				requested: requested, createdAt: time.Now(), freshRevision: freshRevision}
			s.mu.Unlock()
			if s.wire.SendImageCleanupJournalRequest(hostID, agentws.ImageCleanupJournalRequestCmd{ID: id, AttemptIDs: ids}) != nil {
				s.mu.Lock()
				delete(s.journalRequests, id)
				s.mu.Unlock()
			}
		}
	}
}

func (s *CleanupService) ImageVersionsChanged(ctx context.Context, hostID string) {
	// The changed exact identity invalidates its preview; an unrelated image
	// keeps its generation. The agent still rechecks its daemon binding at rmi.
	s.bumpInventoryFences(ctx, hostID)
	s.requestCleanupJournal(ctx, hostID)
}

func (s *CleanupService) ImageCleanupState(ctx context.Context, hostID string, state agentws.ImageCleanupStateMsg) {
	if state.AttemptID == "" || state.Generation == "" || state.State != "removed" && state.State != "failed" && state.State != "unknown" {
		return
	}
	gen, err := strconv.ParseInt(state.Generation, 10, 64)
	if err != nil || gen < 0 {
		return
	}
	// A lost terminal acknowledgement is replayed after the fence has already
	// returned to idle and cleared attempt_id. Validate the durable identity
	// before re-acking; the fence owner check applies only to live transitions.
	var previous CleanupAttempt
	var previousGen int64
	if err := s.pool.QueryRow(ctx, `SELECT image_id,version,image_ref,runtime_image_id,generation,state
		FROM host_image_cleanup_attempts WHERE id=$1::uuid AND host_id=$2::uuid`, state.AttemptID, hostID).Scan(
		&previous.ImageID, &previous.Version, &previous.ImageRef, &previous.RuntimeImageID, &previousGen, &previous.State); err != nil {
		return
	}
	if previousGen != gen || previous.ImageID != state.ImageID || previous.Version != state.Version ||
		previous.ImageRef != state.ImageRef || previous.RuntimeImageID != state.RuntimeImageID {
		return
	}
	if previous.State == "removed" || previous.State == "failed" {
		_ = s.ackTerminal(hostID, state.AttemptID, state.Generation)
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var a CleanupAttempt
	var persistedGen int64
	var fenceAttempt *string
	// The image identity is immutable. Resolve it without a row lock, then
	// acquire fence before attempt: POST takes the same order.
	if err := tx.QueryRow(ctx, `SELECT image_id FROM host_image_cleanup_attempts
		WHERE id=$1::uuid AND host_id=$2::uuid`, state.AttemptID, hostID).Scan(&a.ImageID); err != nil {
		return
	}
	if err := tx.QueryRow(ctx, `SELECT attempt_id::text FROM host_image_operation_fences
		WHERE host_id=$1::uuid AND image_id=$2 FOR UPDATE`, hostID, a.ImageID).Scan(&fenceAttempt); err != nil {
		return
	}
	if err := tx.QueryRow(ctx, `SELECT version,image_ref,runtime_image_id,generation,state,reason
		FROM host_image_cleanup_attempts WHERE id=$1::uuid AND host_id=$2::uuid FOR UPDATE`, state.AttemptID, hostID).Scan(
		&a.Version, &a.ImageRef, &a.RuntimeImageID, &persistedGen, &a.State, &a.Reason); err != nil {
		return
	}
	if persistedGen != gen || a.ImageID != state.ImageID || a.Version != state.Version ||
		a.ImageRef != state.ImageRef || a.RuntimeImageID != state.RuntimeImageID {
		return
	}
	if a.State == "removed" || a.State == "failed" {
		_ = tx.Rollback(ctx)
		_ = s.ackTerminal(hostID, state.AttemptID, state.Generation)
		return
	}
	if fenceAttempt == nil || *fenceAttempt != state.AttemptID {
		return
	}
	newState := "unknown"
	firstRefusal := state.Reason != nil && safeCleanupRefusal(*state.Reason)
	present := false
	if state.State == "removed" && s.currentVersionState(hostID, a, "absent") {
		newState = "removed"
	} else if state.State == "failed" {
		present = s.currentVersionState(hostID, a, "present")
		if firstRefusal || present {
			newState = "failed"
		}
	}
	var reason *string
	if newState == "failed" {
		if firstRefusal {
			reason = state.Reason
		} else {
			reason = strptr("image_still_present")
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE host_image_cleanup_attempts SET state=$2,reason=$3,updated_at=now()
		WHERE id=$1::uuid AND state IN ('removing','unknown')`, state.AttemptID, newState, reason); err != nil {
		return
	}
	if newState != "unknown" {
		if _, err := tx.Exec(ctx, `UPDATE host_image_operation_fences SET state='idle',attempt_id=NULL
			WHERE host_id=$1::uuid AND image_id=$2 AND attempt_id=$3::uuid AND state='removing'`, hostID, a.ImageID, state.AttemptID); err != nil {
			return
		}
		// host_images has a version but no ref column. Demote only a ready
		// version whose frozen current adoption/history proves this exact ref;
		// an older cached version must never erase a different ready version.
		hostState, hostError := "absent", ""
		if newState == "failed" {
			hostState, hostError = "failed", "Image cleanup failed; re-ensure required"
		}
		if _, err := tx.Exec(ctx, `UPDATE host_images hi SET state=$5,error=$6,updated_at=now()
			WHERE hi.host_id=$1::uuid AND hi.image_id=$2 AND hi.version IN ('',$3) AND hi.state='ready'
			AND (EXISTS(SELECT 1 FROM installed_images ii WHERE ii.image_id=$2 AND ii.version=$3
				AND COALESCE(NULLIF(ii.registry_ref,''),NULLIF(ii.local_tag,''))=$4)
			OR EXISTS(SELECT 1 FROM host_image_success_history h WHERE h.host_id=$1::uuid AND h.image_id=$2
				AND h.current_version=$3
				AND COALESCE(NULLIF(h.current_identity->>'registry_ref',''),NULLIF(h.current_identity->>'local_tag',''))=$4))`,
			hostID, a.ImageID, a.Version, a.ImageRef, hostState, hostError); err != nil {
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return
	}
	if newState == "unknown" {
		return
	}
	if newState == "removed" {
		audit.TryRecord(ctx, s.auditor, "", "image.cleanup.removed", "image", a.ImageID,
			map[string]any{"host_id": hostID, "attempt_id": state.AttemptID, "version": a.Version})
	}
	_ = s.ackTerminal(hostID, state.AttemptID, state.Generation)
	if s.ensurer != nil {
		_ = s.ensurer.EnsureHost(ctx, hostID)
		if newState == "failed" {
			// EnsureHost intentionally suppresses an already failed current
			// version. A definite cleanup failure demoted ready to failed, so
			// explicitly re-arm that one current required adoption.
			_ = s.ensurer.RetryHostImage(ctx, hostID, a.ImageID)
		}
	}
}

func (s *CleanupService) ImageCleanupJournal(ctx context.Context, hostID string, journal agentws.ImageCleanupJournalMsg) {
	snap, connected := s.wire.ImageCleanupSnapshot(hostID)
	s.mu.Lock()
	pending, found := s.journalRequests[journal.RequestID]
	if found && pending.hostID == hostID && connected && snap.ConnectionID == pending.connectionID {
		delete(s.journalRequests, journal.RequestID)
	} else {
		found = false
	}
	s.mu.Unlock()
	if !found {
		return
	}
	requested := pending.requested
	seen := map[string]bool{}
	for _, entry := range journal.Attempts {
		if !requested[entry.AttemptID] {
			continue
		}
		seen[entry.AttemptID] = true
		if entry.State == "removed" || entry.State == "failed" || entry.State == "unknown" {
			s.ImageCleanupState(ctx, hostID, entry)
		}
	}
	retired := map[string]bool{}
	for _, id := range journal.RetiredAttemptIDs {
		retired[id] = true
	}
	for id := range requested {
		if !retired[id] || seen[id] || pending.freshRevision == 0 || snap.ReconciledRevision < pending.freshRevision {
			continue
		}
		s.mu.Lock()
		gate, stillCurrent := s.freshReconcile[hostID]
		fresh := stillCurrent && gate.connectionID == pending.connectionID && gate.baseline < pending.freshRevision
		s.mu.Unlock()
		if !fresh {
			continue // a later reconcile superseded this journal proof
		}
		// The agent's durable tombstone closes every old handler epoch. A
		// complete current inventory can now reconcile a lost command. The
		// agent's absent state must also rule out all-container references.
		var a CleanupAttempt
		var gen int64
		err := s.pool.QueryRow(ctx, `SELECT image_id,version,image_ref,runtime_image_id,generation
			FROM host_image_cleanup_attempts WHERE id=$1::uuid AND host_id=$2::uuid
			AND state IN ('removing','unknown')`, id, hostID).Scan(&a.ImageID, &a.Version, &a.ImageRef, &a.RuntimeImageID, &gen)
		if err != nil {
			continue
		}
		a.AttemptID = id
		state := ""
		if s.currentVersionStateOnConnection(hostID, pending.connectionID, a, "absent") {
			state = "removed"
		}
		if s.currentVersionStateOnConnection(hostID, pending.connectionID, a, "present") {
			state = "failed"
		}
		if state == "" {
			continue
		}
		// A synthetic report is tied to the retired journal ID and the
		// current connection's exact inventory, then passes the same CAS.
		s.ImageCleanupState(ctx, hostID, agentws.ImageCleanupStateMsg{AttemptID: id, ImageID: a.ImageID,
			Version: a.Version, ImageRef: a.ImageRef, RuntimeImageID: a.RuntimeImageID,
			Generation: strconv.FormatInt(gen, 10), State: state})
		if state == "removed" {
			var persisted string
			if s.pool.QueryRow(ctx, `SELECT state FROM host_image_cleanup_attempts WHERE id=$1::uuid`, id).Scan(&persisted) == nil && persisted == "removed" {
				audit.TryRecord(ctx, s.auditor, "", "image.cleanup.reconciled", "image", a.ImageID,
					map[string]any{"host_id": hostID, "attempt_id": id, "version": a.Version})
			}
		}
	}
}

func (s *CleanupService) currentVersionState(hostID string, a CleanupAttempt, want string) bool {
	return s.currentVersionStateOnConnection(hostID, "", a, want)
}

func (s *CleanupService) currentVersionStateOnConnection(hostID, connectionID string, a CleanupAttempt, want string) bool {
	snap, online := s.wire.ImageCleanupSnapshot(hostID)
	if !online || !snap.Capable || !snap.Complete || connectionID != "" && snap.ConnectionID != connectionID {
		return false
	}
	for _, v := range snap.Versions {
		if v.ImageID == a.ImageID && v.Version == a.Version && v.ImageRef == a.ImageRef &&
			v.RuntimeImageID == a.RuntimeImageID && v.State == want {
			return true
		}
	}
	return false
}

func safeCleanupRefusal(reason string) bool {
	switch reason {
	case "identity_mismatch", "inventory_unknown", "reference_in_use", "operation_busy", "unsupported":
		return true
	default:
		return false
	}
}

func (s *CleanupService) ackTerminal(hostID, attemptID, generation string) error {
	id, err := newCmdID()
	if err != nil {
		return err
	}
	return s.wire.SendImageCleanupStateAck(hostID, agentws.ImageCleanupStateAckCmd{
		ID: id, AttemptID: attemptID, Generation: generation,
	})
}

// RunRetention prunes only old terminal rows superseded by a newer terminal
// result for the same exact identity. The latest result remains indefinitely
// for idempotent POST, and removing/unknown rows are never age-pruned.
func (s *CleanupService) RunRetention(ctx context.Context) {
	prune := func() {
		_, _ = s.pool.Exec(ctx, `DELETE FROM host_image_cleanup_attempts a
			WHERE a.state IN ('removed','failed') AND a.updated_at < now()-interval '90 days'
			AND EXISTS(SELECT 1 FROM host_image_cleanup_attempts newer
				WHERE newer.host_id=a.host_id AND newer.image_id=a.image_id
				AND newer.version=a.version AND newer.image_ref=a.image_ref
				AND newer.runtime_image_id=a.runtime_image_id AND newer.state IN ('removed','failed')
				AND (newer.updated_at,newer.id)>(a.updated_at,a.id))`)
	}
	prune()
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
