package platform

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A developer apply to the control-plane target of an owned machine: a
// standalone control-plane attempt (no run) that behaves as a fleet run's
// non-migrating control-plane step (control-api.md §"Developer apply"). It
// cordons every host for its duration with an admission restriction owned by
// the attempt, and releases them once the attempt is terminal, which on the
// normal path is a later boot of this control plane.

// ownedMigratingRefusal is what every path that meets a migrating control-plane
// step on an owned machine answers until the pre-update dump exists.
const ownedMigratingRefusal = "this control plane migrates the database, and updating a Quasar-owned control plane " +
	"across a migration (with its pre-update dump) arrives with RH06-12 (#364); nothing was changed"

// PlatformHold is one host's admission restriction owned by an attempt.
type PlatformHold struct {
	AttemptID string
	HostID    string
}

type selfDeveloperStore interface {
	Hosts(ctx context.Context) ([]HostIdentity, error)
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	OpenAttempts(ctx context.Context) ([]Attempt, error)
	FleetInFlightSessions(ctx context.Context) (int, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	TerminalStandaloneControlPlaneHolds(ctx context.Context) ([]PlatformHold, error)
}

// SelfDeveloperRunner drives standalone control-plane developer applies.
type SelfDeveloperRunner struct {
	store   selfDeveloperStore
	self    selfDriver
	cordons FleetCordons
	// commit reads the images' commit: the success evidence of an attempt this
	// process re-adopts after the restart the attempt caused.
	commit func(ctx context.Context, components []ComponentDigest) (string, error)
	log    logger

	InFlightSettle time.Duration
	PollWait       time.Duration

	mu      sync.Mutex
	running map[string]bool
	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// NewSelfDeveloperRunner builds the driver; cordons needs AcquireOwned and
// ReleaseOwned.
func NewSelfDeveloperRunner(store selfDeveloperStore, self selfDriver, cordons FleetCordons,
	commit func(ctx context.Context, components []ComponentDigest) (string, error), log logger) *SelfDeveloperRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &SelfDeveloperRunner{
		store: store, self: self, cordons: cordons, commit: commit, log: log,
		InFlightSettle: DefaultInFlightSettle, PollWait: DefaultApplyPoll,
		running: map[string]bool{}, baseCtx: ctx, stop: cancel,
	}
}

// Start drives one attempt the endpoint created. Idempotent per attempt.
func (r *SelfDeveloperRunner) Start(a Attempt) { r.start(a, false) }

// Adopt releases the holds of attempts that ended while no process was left to
// release them, then re-drives every open standalone control-plane attempt:
// normally the one whose replacement restarted this process.
func (r *SelfDeveloperRunner) Adopt(ctx context.Context) {
	if holds, err := r.store.TerminalStandaloneControlPlaneHolds(ctx); err != nil {
		r.log.Error("could not find control-plane developer apply holds awaiting release", "err", err)
	} else {
		for _, h := range holds {
			r.release(ctx, h.AttemptID, h.HostID)
		}
	}
	open, err := r.store.OpenAttempts(ctx)
	if err != nil {
		r.log.Error("could not re-adopt a control-plane developer apply", "err", err)
		return
	}
	for _, a := range open {
		if a.Target == TargetControlPlane && a.RunID == nil {
			r.log.Warn("re-adopting a control-plane developer apply left in flight by a restart",
				"attempt_id", a.ID, "state", a.State)
			r.start(a, true)
		}
	}
}

// Close cancels the driving goroutines; the rows stay open for the next Adopt.
func (r *SelfDeveloperRunner) Close() {
	r.stop()
	r.wg.Wait()
}

func (r *SelfDeveloperRunner) start(a Attempt, adopted bool) {
	r.mu.Lock()
	if r.running[a.ID] {
		r.mu.Unlock()
		return
	}
	r.running[a.ID] = true
	r.mu.Unlock()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.running, a.ID)
			r.mu.Unlock()
		}()
		r.drive(r.baseCtx, a, adopted)
	}()
}

func (r *SelfDeveloperRunner) drive(ctx context.Context, a Attempt, adopted bool) {
	hosts, err := r.store.Hosts(ctx)
	if err != nil {
		r.fail(a.ID, fmt.Sprintf("could not read the hosts to cordon: %v", err))
		return
	}
	// Re-taken on adoption too: the restart this attempt caused is exactly
	// when a hold's projection may have been lifted underneath it.
	for _, h := range hosts {
		if err := r.cordons.AcquireOwned(ctx, a.ID, h.HostID); err != nil {
			r.fail(a.ID, fmt.Sprintf("could not cordon %s: %v", h.NodeName, err))
			r.releaseAll(a.ID)
			return
		}
	}
	if adopted {
		commit := ""
		if r.commit != nil {
			if c, err := r.commit(ctx, a.RequestedDigests); err == nil {
				commit = c
			} else {
				r.log.Warn("developer apply: the images' commit is unreadable; only the recovery actor's result can resolve the attempt",
					"attempt_id", a.ID, "err", err)
			}
		}
		if !r.self.Adopt(ctx, a, commit) {
			r.settleInFlight(ctx)
			r.self.Apply(ctx, a)
		}
	} else {
		r.settleInFlight(ctx)
		r.self.Apply(ctx, a)
	}
	// Normally unreached: the replacement ends this process mid-poll and the
	// next boot's Adopt resolves the row and lands here.
	if cur, err := r.store.Attempt(context.WithoutCancel(ctx), a.ID); err == nil && TerminalAttemptState(cur.State) {
		r.releaseAll(a.ID)
	}
}

// settleInFlight lets launches already placed finish arriving, as the fleet's
// non-migrating control-plane step does; expiry proceeds (DefaultInFlightSettle).
func (r *SelfDeveloperRunner) settleInFlight(ctx context.Context) {
	deadline := time.Now().Add(r.InFlightSettle)
	for time.Now().Before(deadline) {
		n, err := r.store.FleetInFlightSessions(ctx)
		if err == nil && n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.PollWait):
		}
	}
}

func (r *SelfDeveloperRunner) releaseAll(attemptID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hosts, err := r.store.Hosts(ctx)
	if err != nil {
		r.log.Warn("developer apply: could not read the hosts to release; the next boot releases them",
			"attempt_id", attemptID, "err", err)
		return
	}
	for _, h := range hosts {
		r.release(ctx, attemptID, h.HostID)
	}
}

func (r *SelfDeveloperRunner) release(ctx context.Context, attemptID, hostID string) {
	if err := r.cordons.ReleaseOwned(ctx, attemptID, hostID); err != nil {
		r.log.Warn("developer apply: a host hold remains for the next boot to release",
			"attempt_id", attemptID, "host_id", hostID, "err", err)
	}
}

func (r *SelfDeveloperRunner) fail(attemptID, output string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.FailAttempt(ctx, attemptID, ReasonUpdaterUnreachable, output); err != nil {
		r.log.Error("developer apply: could not record the failure", "attempt_id", attemptID, "err", err)
	}
}

// TerminalStandaloneControlPlaneHolds finds the host holds a standalone
// control-plane attempt still owns after it ended: the release a restart
// interrupted. The restriction row is the durable retry marker.
func (s *Store) TerminalStandaloneControlPlaneHolds(ctx context.Context) ([]PlatformHold, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id::text, ar.host_id::text
		  FROM platform_apply_attempts a
		  JOIN host_admission_restrictions ar
		    ON ar.owner_kind = 'platform' AND ar.owner_id = a.id
		 WHERE a.run_id IS NULL AND a.target = 'control_plane' AND a.state IN `+terminalStatesSQL+`
		 ORDER BY a.created_at, a.id, ar.host_id`)
	if err != nil {
		return nil, fmt.Errorf("query control-plane attempt holds: %w", err)
	}
	defer rows.Close()
	var out []PlatformHold
	for rows.Next() {
		var h PlatformHold
		if err := rows.Scan(&h.AttemptID, &h.HostID); err != nil {
			return nil, fmt.Errorf("scan control-plane attempt hold: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
