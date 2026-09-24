package agentws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

// ImageVersionEntry is one exact managed version from the authenticated agent.
// A present entry is useful only while its connection's complete snapshot is
// current; the single legacy register.images row cannot establish this fact.
type ImageVersionEntry struct {
	ImageID        string `json:"image_id"`
	Version        string `json:"version"`
	ImageRef       string `json:"image_ref"`
	RuntimeImageID string `json:"runtime_image_id"`
	State          string `json:"state"`
	// Nil means an older agent did not supply all-container reference evidence.
	ContainerReferenced *bool `json:"container_referenced,omitempty"`
}

func (v *ImageVersionEntry) UnmarshalJSON(data []byte) error {
	type plain ImageVersionEntry
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var field struct {
		ContainerReferenced json.RawMessage `json:"container_referenced"`
	}
	if err := json.Unmarshal(data, &field); err != nil {
		return err
	}
	if bytes.Equal(bytes.TrimSpace(field.ContainerReferenced), []byte("null")) {
		return errors.New("container_referenced must be Boolean when present")
	}
	*v = ImageVersionEntry(decoded)
	return nil
}

type ImageVersionsStateMsg struct {
	Type                  string              `json:"type"`
	InventoryRevision     string              `json:"inventory_revision"`
	ImageVersionsComplete bool                `json:"image_versions_complete"`
	ImageVersions         []ImageVersionEntry `json:"image_versions"`
}

type ImageCleanupRegister struct {
	Capable  bool
	Complete bool
	Versions []ImageVersionEntry
}

// ImageCleanupEvents receives only authenticated current-connection reports.
// The registry holds their volatile inventory; persistence belongs to images.
type ImageCleanupEvents interface {
	ImageCleanupRegistered(context.Context, string)
	ImageVersionsChanged(context.Context, string)
	ImageCleanupState(context.Context, string, ImageCleanupStateMsg)
	ImageCleanupJournal(context.Context, string, ImageCleanupJournalMsg)
}

type ImageCleanupStateMsg struct {
	Type           string  `json:"type"`
	AttemptID      string  `json:"attempt_id"`
	ImageID        string  `json:"image_id"`
	Version        string  `json:"version"`
	ImageRef       string  `json:"image_ref"`
	RuntimeImageID string  `json:"runtime_image_id"`
	Generation     string  `json:"generation"`
	State          string  `json:"state"`
	Reason         *string `json:"reason"`
}

type ImageCleanupJournalMsg struct {
	Type              string                 `json:"type"`
	RequestID         string                 `json:"request_id"`
	RetiredAttemptIDs []string               `json:"retired_attempt_ids"`
	Attempts          []ImageCleanupStateMsg `json:"attempts"`
}

type ImageCleanupCmd struct {
	Type               string `json:"type"`
	ID                 string `json:"id"`
	AttemptID          string `json:"attempt_id"`
	ImageID            string `json:"image_id"`
	Version            string `json:"version"`
	ImageRef           string `json:"image_ref"`
	RuntimeImageID     string `json:"runtime_image_id"`
	ExpectedGeneration string `json:"expected_generation"`
}

type ImageInventoryIdentity struct {
	ImageID  string `json:"image_id"`
	Version  string `json:"version"`
	ImageRef string `json:"image_ref"`
}

type ImageInventoryReconcileCmd struct {
	Type       string                   `json:"type"`
	ID         string                   `json:"id"`
	Identities []ImageInventoryIdentity `json:"identities"`
}

type ImageCleanupJournalRequestCmd struct {
	Type       string   `json:"type"`
	ID         string   `json:"id"`
	AttemptIDs []string `json:"attempt_ids"`
}

type ImageCleanupStateAckCmd struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	AttemptID  string `json:"attempt_id"`
	Generation string `json:"generation"`
}

// ImageCleanupSnapshot is copied from one current authenticated connection.
type ImageCleanupSnapshot struct {
	ConnectionID string
	Capable      bool
	Complete     bool
	ObservedAt   time.Time
	// ReconciledRevision is the first accepted inventory revision after the
	// latest acknowledged image_inventory_reconcile on this connection.
	ReconciledRevision uint64
	// ReconciledRequestID binds that revision to the exact reconcile command.
	ReconciledRequestID string
	Versions            []ImageVersionEntry
}

func validImageVersions(versions []ImageVersionEntry, complete bool) bool {
	if len(versions) > 1024 || complete && versions == nil {
		return false
	}
	seen := make(map[string]bool, len(versions))
	for _, v := range versions {
		if v.ImageID == "" || len(v.ImageID) > 128 || v.Version == "" || len(v.Version) > 128 ||
			v.ImageRef == "" || len(v.ImageRef) > 1024 || v.RuntimeImageID == "" || len(v.RuntimeImageID) > 256 ||
			v.State != "present" && v.State != "absent" && v.State != "unknown" ||
			v.State != "present" && v.ContainerReferenced != nil {
			return false
		}
		key := v.ImageID + "\x00" + v.Version + "\x00" + v.ImageRef + "\x00" + v.RuntimeImageID
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

// ImageCleanupSnapshot returns no stale rows after disconnect or connection
// displacement. Missing support remains distinguishable from an offline host.
func (r *Registry) ImageCleanupSnapshot(hostID string) (ImageCleanupSnapshot, bool) {
	r.mu.Lock()
	c := r.conns[hostID]
	if c == nil {
		r.mu.Unlock()
		return ImageCleanupSnapshot{}, false
	}
	c.mu.Lock()
	s := ImageCleanupSnapshot{ConnectionID: c.connectionIncarnation, Capable: c.imageCleanupV1,
		Complete: c.imageVersionsComplete, ObservedAt: c.imageVersionsObservedAt,
		ReconciledRevision: c.imageReconciledRevision, ReconciledRequestID: c.imageReconciledRequestID,
		Versions: append([]ImageVersionEntry(nil), c.imageVersions...)}
	c.mu.Unlock()
	r.mu.Unlock()
	return s, true
}

func (r *Registry) updateImageVersions(c *conn, m ImageVersionsStateMsg) bool {
	revision, err := strconv.ParseUint(m.InventoryRevision, 10, 64)
	if err != nil || revision == 0 || !validImageVersions(m.ImageVersions, m.ImageVersionsComplete) {
		return r.invalidateImageVersions(c, m.InventoryRevision)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.hostID] != c || !c.imageCleanupV1 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision <= c.imageVersionsRevision {
		return false
	}
	c.imageVersionsRevision = revision
	c.imageVersionsComplete = m.ImageVersionsComplete
	c.imageVersions = append([]ImageVersionEntry(nil), m.ImageVersions...)
	c.imageVersionsObservedAt = time.Now().UTC()
	if c.imageReconcileAwaiting {
		c.imageReconciledRevision = revision
		c.imageReconciledRequestID = c.imageReconcileAwaitingID
		c.imageReconcileAwaiting = false
		c.imageReconcileAwaitingID = ""
	}
	return true
}

// A malformed newer report cannot leave a prior complete scan authoritative.
// Older out-of-order reports do not replace a newer current snapshot.
func (r *Registry) invalidateImageVersions(c *conn, revisionText string) bool {
	revision, err := strconv.ParseUint(revisionText, 10, 64)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.hostID] != c || !c.imageCleanupV1 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil && revision != 0 && revision <= c.imageVersionsRevision {
		return false
	}
	if err == nil && revision != 0 {
		c.imageVersionsRevision = revision
	}
	c.imageVersionsComplete = false
	c.imageVersions = nil
	c.imageVersionsObservedAt = time.Now().UTC()
	return true
}

func (r *Registry) SendImageCleanup(ctx context.Context, hostID string, cmd ImageCleanupCmd) (AckResult, error) {
	cmd.Type = "image_cleanup"
	return r.SendWithAck(ctx, hostID, cmd.ID, cmd)
}

func (r *Registry) SendImageInventoryReconcile(hostID string, cmd ImageInventoryReconcileCmd) error {
	cmd.Type = "image_inventory_reconcile"
	// The agent's wire contract requires an array even before any managed image
	// has been adopted. A nil Go slice would encode as JSON null and prevent
	// the initial inventory scan from ever being accepted.
	if cmd.Identities == nil {
		cmd.Identities = []ImageInventoryIdentity{}
	}
	c, ok := r.get(hostID)
	if !ok {
		return ErrAgentNotConnected
	}
	c.mu.Lock()
	c.imageReconcilePending = map[string]bool{cmd.ID: true}
	c.imageReconcileAwaiting = false
	c.imageReconcileAwaitingID = ""
	c.mu.Unlock()
	if err := c.enqueue(cmd); err != nil {
		c.mu.Lock()
		delete(c.imageReconcilePending, cmd.ID)
		c.mu.Unlock()
		return err
	}
	return nil
}

func (r *Registry) SendImageCleanupJournalRequest(hostID string, cmd ImageCleanupJournalRequestCmd) error {
	cmd.Type = "image_cleanup_journal_request"
	return r.Send(hostID, cmd)
}

func (r *Registry) SendImageCleanupStateAck(hostID string, cmd ImageCleanupStateAckCmd) error {
	cmd.Type = "image_cleanup_state_ack"
	return r.Send(hostID, cmd)
}
