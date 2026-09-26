package platform

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A developer apply to the control-plane target of an owned machine: a
// standalone control-plane attempt (no run) that behaves as a fleet run's
// control-plane step (control-api.md §"Developer apply"). It cordons every host
// for its duration with an admission restriction owned by the attempt, and
// releases them once the attempt is terminal, which on the normal path is a
// later boot of this control plane. A digest that migrates drains the instance
// first, as a migrating release does (#352 decision 14); the recovery actor then
// takes the pre-update dump or holds the operator to their confirmed backup.

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
	FleetNonTerminalSessions(ctx context.Context) (int, error)
	SetWaitingSessions(ctx context.Context, attemptID string, remaining int) error
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
	// Deadline bounds a migrating attempt's drain, from its created_at.
	Deadline time.Duration
	// Migrates reads whether the attempt's control-plane image moves the
	// schema forward. Nil, or an unreadable answer, reads as migrating: an
	// unneeded drain costs sessions visibly, a missing one runs a migration
	// under live sessions.
	Migrates func(ctx context.Context, components []ComponentDigest) (bool, error)

	// migrates holds what admission decided per attempt (NoteDeveloperSchema),
	// so prepare reads no registry. In memory only, like the self-applier's.
	migrates sync.Map

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
		InFlightSettle: DefaultInFlightSettle, PollWait: DefaultApplyPoll, Deadline: DefaultApplyDeadline,
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
	// Shutting down is never a failure: a sent attempt's verdict is the actor's,
	// and the next boot reads it.
	hosts, err := r.store.Hosts(ctx)
	if ctx.Err() != nil {
		return
	}
	if err != nil && !adopted {
		r.fail(a.ID, fmt.Sprintf("could not read the hosts to cordon: %v", err))
		return
	}
	if err != nil {
		r.log.Warn("developer apply: could not read the hosts to re-take their holds on adoption; resolving the attempt anyway",
			"attempt_id", a.ID, "err", err)
	}
	// Re-taken on adoption too: the restart this attempt caused is exactly
	// when a hold's projection may have been lifted underneath it. An adopted
	// attempt may already be in the actor's hands, so it is never failed here.
	for _, h := range hosts {
		err := r.cordons.AcquireOwned(ctx, a.ID, h.HostID)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if adopted {
			r.log.Warn("developer apply: could not re-take a host hold on adoption", "attempt_id", a.ID, "host_id", h.HostID, "err", err)
			continue
		}
		r.fail(a.ID, fmt.Sprintf("could not cordon %s: %v", h.NodeName, err))
		r.releaseAll(a.ID)
		return
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
		// Never re-driven while shutting down (#363): that would fail a row
		// whose verdict the next boot reads.
		if !r.self.Adopt(ctx, a, commit) && ctx.Err() == nil && r.prepare(ctx, a) {
			r.self.Apply(ctx, a)
		}
	} else if r.prepare(ctx, a) {
		r.self.Apply(ctx, a)
	}
	// Normally unreached: the replacement ends this process mid-poll and the
	// next boot's Adopt resolves the row and lands here.
	if cur, err := r.store.Attempt(context.WithoutCancel(ctx), a.ID); err == nil && TerminalAttemptState(cur.State) {
		r.releaseAll(a.ID)
	}
}

// developerSchemaNoter is a self driver that keeps the schema admission read.
type developerSchemaNoter interface {
	NoteDeveloperSchema(attemptID string, schema int)
}

// NoteDeveloperSchema keeps what the admission read of the attempt's
// control-plane image: whether it migrates, for the drain, and its schema, for
// the send. Call it before Start: a registry that stops answering after the
// drain then cannot fail the attempt.
func (r *SelfDeveloperRunner) NoteDeveloperSchema(attemptID string, schema int, migrates bool) {
	r.migrates.Store(attemptID, migrates)
	if n, ok := r.self.(developerSchemaNoter); ok {
		n.NoteDeveloperSchema(attemptID, schema)
	}
}

// ConfirmExternalBackup hands the operator's confirmation of their own
// database's backup to the self-applier that sends the attempt.
func (r *SelfDeveloperRunner) ConfirmExternalBackup(attemptID string) {
	if c, ok := r.self.(backupConfirmer); ok {
		c.ConfirmExternalBackup(attemptID)
	}
}

// prepare is what the step owes the instance's sessions before it is sent, as
// the fleet's prepareFleet decides it: a migrating digest drains the instance
// (under force, stopping what runs), anything else lets in-flight launches
// settle. False means the attempt resolved (a deadline) or the process is
// stopping.
func (r *SelfDeveloperRunner) prepare(ctx context.Context, a Attempt) bool {
	migrates := true
	if noted, ok := r.migrates.Load(a.ID); ok {
		migrates = noted.(bool)
	} else if r.Migrates != nil {
		if m, err := r.Migrates(ctx, a.RequestedDigests); err == nil {
			migrates = m
		} else {
			r.log.Warn("developer apply: could not tell whether the image migrates; draining the instance",
				"attempt_id", a.ID, "err", err)
		}
	}
	if !migrates {
		r.settleInFlight(ctx)
		return true
	}
	return r.drainForMigration(ctx, a)
}

// drainForMigration waits for the instance to hold no session, from a count that
// was read (a failed read is never zero), stopping them first under force.
func (r *SelfDeveloperRunner) drainForMigration(ctx context.Context, a Attempt) bool {
	count := func() (int, bool) {
		n, err := r.store.FleetNonTerminalSessions(ctx)
		if err != nil {
			r.log.Error("developer apply: could not count the instance's sessions", "attempt_id", a.ID, "err", err)
			return 0, false
		}
		return n, true
	}
	remaining, known := count()
	if known {
		_ = r.store.SetWaitingSessions(ctx, a.ID, remaining)
	}
	if a.Force && (!known || remaining != 0) && r.cordons.DrainOwned != nil {
		hosts, err := r.store.Hosts(ctx)
		if err != nil {
			r.log.Warn("developer apply: could not read the hosts to drain", "attempt_id", a.ID, "err", err)
		}
		for _, h := range hosts {
			if err := r.cordons.DrainOwned(ctx, a.ID, h.HostID); err != nil {
				r.log.Warn("developer apply: could not drain a host", "attempt_id", a.ID, "host_id", h.HostID, "err", err)
			}
		}
		remaining, known = count()
	}
	deadline := a.CreatedAt.Add(r.Deadline)
	for !known || remaining != 0 {
		if time.Now().After(deadline) {
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := r.store.FailAttempt(fctx, a.ID, ReasonTimeout,
				"the instance did not drain before the deadline, so the migrating control plane was not sent"); err != nil {
				r.log.Error("developer apply: could not record the failure", "attempt_id", a.ID, "err", err)
			}
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(r.PollWait):
		}
		if cur, err := r.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
			return false
		}
		if n, ok := count(); ok {
			remaining, known = n, true
			_ = r.store.SetWaitingSessions(ctx, a.ID, remaining)
		} else {
			known = false
		}
	}
	return true
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
