package session

import (
	"sync"
	"time"
)

// SPT-06 — in-memory cert-run tracking. Runs are not persisted: a control-
// plane restart drops any running bench (operator re-triggers). One run per
// host at a time; a second POST while one is in-flight gets 409.

// CertRun status values.
const (
	CertRunRunning   = "running"
	CertRunCompleted = "completed"
	CertRunFailed    = "failed"
)

// CertRun is the in-memory state of one certification run.
type CertRun struct {
	ID            string
	HostID        string
	Status        string
	StartedAt     time.Time
	EndedAt       *time.Time
	ErrorMessage  *string
	CompletedPts  int
	TotalPts      int
	SummaryOK     int
	SummaryCapped int
	SummaryUnsafe int
}

// certRunTTL bounds how long a terminal run is kept for lookup via Get before
// Start's sweep prunes it: long enough for a caller to poll shortly after
// completion, short enough that `runs` doesn't grow unbounded.
const certRunTTL = 1 * time.Hour

// certRunManager tracks in-flight runs, keyed by run ID, one per host at a
// time. Terminal runs are pruned lazily: a new run for a host evicts its prior
// terminal run, and every Start sweeps runs older than certRunTTL as a
// backstop for hosts that stop starting new ones.
type certRunManager struct {
	mu     sync.RWMutex
	runs   map[string]*CertRun // keyed by run ID
	byHost map[string]string   // hostID → active/most-recent run ID
}

func newCertRunManager() *certRunManager {
	return &certRunManager{
		runs:   make(map[string]*CertRun),
		byHost: make(map[string]string),
	}
}

// Start creates a new run for hostID if none is currently in-flight.
// Returns (run, true) on success or (nil, false) if one is already running.
func (m *certRunManager) Start(hostID string, totalPts int) (*CertRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existingID, ok := m.byHost[hostID]; ok {
		if r, exists := m.runs[existingID]; exists {
			if r.Status == CertRunRunning {
				return nil, false
			}
			delete(m.runs, existingID)
		}
	}
	m.pruneExpiredLocked()

	id := newCmdID() // hex-random helper from coordinator.go; avoids adding a uuid dep

	run := &CertRun{
		ID:        id,
		HostID:    hostID,
		Status:    CertRunRunning,
		StartedAt: time.Now(),
		TotalPts:  totalPts,
	}
	m.runs[id] = run
	m.byHost[hostID] = id
	return run, true
}

// pruneExpiredLocked deletes terminal runs older than certRunTTL. Caller must
// hold m.mu.
func (m *certRunManager) pruneExpiredLocked() {
	cutoff := time.Now().Add(-certRunTTL)
	for id, r := range m.runs {
		if r.Status != CertRunRunning && r.EndedAt != nil && r.EndedAt.Before(cutoff) {
			delete(m.runs, id)
			if m.byHost[r.HostID] == id {
				delete(m.byHost, r.HostID)
			}
		}
	}
}

// Get returns a copy of the run by ID, verifying it belongs to hostID.
func (m *certRunManager) Get(runID, hostID string) (*CertRun, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[runID]
	if !ok || r.HostID != hostID {
		return nil, false
	}
	cp := *r
	return &cp, true
}

func (m *certRunManager) Complete(runID string, ok, capped, unsafe int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, exists := m.runs[runID]
	if !exists {
		return
	}
	now := time.Now()
	r.Status = CertRunCompleted
	r.EndedAt = &now
	r.SummaryOK = ok
	r.SummaryCapped = capped
	r.SummaryUnsafe = unsafe
}

func (m *certRunManager) Fail(runID, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, exists := m.runs[runID]
	if !exists {
		return
	}
	now := time.Now()
	r.Status = CertRunFailed
	r.EndedAt = &now
	r.ErrorMessage = &msg
}

// Increment advances CompletedPts and tallies the verdict for one bench point.
func (m *certRunManager) Increment(runID, verdict string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, exists := m.runs[runID]
	if !exists {
		return
	}
	r.CompletedPts++
	switch verdict {
	case VerdictOK:
		r.SummaryOK++
	case VerdictCapped:
		r.SummaryCapped++
	default:
		r.SummaryUnsafe++
	}
}
