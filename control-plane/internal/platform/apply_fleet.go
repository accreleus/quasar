package platform

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The fleet sequencer: one release across the instance, the control plane first
// and then every eligible host in sequence (ADR 0002).
// semantics: control-api.md §"Platform-release apply"
//
// Three rules a plausible edit breaks:
//   - A run STOPS at its first failed target. Past a failed control plane,
//     continuing would move agents onto a release the control plane is not on;
//     past a failed host, it would march a known-bad digest set across the fleet.
//   - An ineligible host is SKIPPED, not failed: a run must not go `failed`
//     because a host happened to be offline.
//   - The cancel flag is read BETWEEN targets and never mid-attempt.
//   - Recreating the CONTROL PLANE ends every session on the instance: an agent
//     stops its sessions the moment its control-plane connection drops. So the
//     control-plane target drains the whole fleet first, and the fleet stays
//     cordoned until the run is terminal.
//
// A fleet run survives the restart it causes: everything durable is in
// Postgres, so the control plane that boots on the new image re-adopts the run
// and resumes at the next target.

// fleetStore is the persistence the sequencer needs, as an interface so the
// ordering is testable with no database.
type fleetStore interface {
	Run(ctx context.Context, id string) (ApplyRun, error)
	ActiveRun(ctx context.Context) (*ApplyRun, error)
	RunAttempts(ctx context.Context, runID string) ([]Attempt, error)
	SetRunTarget(ctx context.Context, runID, target string, hostID *string) error
	FinishRun(ctx context.Context, runID, state, errText string) error
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	Hosts(ctx context.Context) ([]HostIdentity, error)
	HostStatus(ctx context.Context, hostID string) (string, error)
	SetCordonedHosts(ctx context.Context, runID string, states []HostCordon) error
	CordonedHosts(ctx context.Context, runID string) ([]HostCordon, error)
	FleetNonTerminalSessions(ctx context.Context) (int, error)
	CreateHostAttempt(ctx context.Context, in NewHostAttempt) (Attempt, error)
	CreateControlPlaneAttempt(ctx context.Context, in NewControlPlaneAttempt) (Attempt, error)
	LastSucceededDigests(ctx context.Context, hostID string) ([]ComponentDigest, error)
	LastSucceededControlPlaneDigests(ctx context.Context) ([]ComponentDigest, error)
	Release(ctx context.Context, id string) (Release, error)
	NonTerminalSessions(ctx context.Context, hostID string) (int, error)
	SetWaitingSessions(ctx context.Context, attemptID string, remaining int) error
}

// FleetCordons are the scheduling effects the run needs. Function fields as in
// ApplyDeps: internal/session sits above this package.
type FleetCordons struct {
	Cordon   func(ctx context.Context, hostID string) error
	Uncordon func(ctx context.Context, hostID string) error
}

// HostCordon is what the run found for one host before it cordoned, persisted
// on the run row (migration 0076).
type HostCordon struct {
	HostID string `json:"host_id"`
	// The host was ALREADY out of scheduling, so the run must leave it that way.
	WasCordoned bool `json:"was_cordoned"`
}

// DefaultAdoptSettle is how long a re-adopted run waits before its first host.
// Agents re-register within a couple of seconds of the new control plane
// listening, and a run that moves first sees a fleet that is briefly all
// disconnected.
const DefaultAdoptSettle = 10 * time.Second

// hostDriver is the per-host attempt machine (apply_runner.go).
type hostDriver interface {
	Start(a Attempt)
}

// selfDriver is the control-plane attempt machine (apply_self.go).
type selfDriver interface {
	UpdaterPresent() bool
	Apply(ctx context.Context, a Attempt)
	Adopt(ctx context.Context, a Attempt, wantCommit string) bool
}

// componentResolver resolves what a release moves each target to: the manifest
// when there is one, the registry when there is not (an edge release).
type componentResolver interface {
	HostComponents(ctx context.Context, r Release) ([]ComponentDigest, error)
	ControlPlaneComponents(ctx context.Context, r Release) ([]ComponentDigest, error)
}

// ManifestOrEdge resolves what a release moves a target to: the pinned manifest
// when the release has one, the registry when it does not (an edge release
// stores `manifest` NULL — apply_edge.go). A nil edge resolver refuses an edge
// release rather than guessing at a digest (ADR 0001).
type ManifestOrEdge struct{ Edge ApplyComponentResolver }

func (m ManifestOrEdge) HostComponents(ctx context.Context, r Release) ([]ComponentDigest, error) {
	if c := releaseComponents(r); len(c) > 0 {
		return c, nil
	}
	if m.Edge == nil {
		return nil, errors.New("this release carries no manifest and this control plane cannot reach the registry to resolve one")
	}
	c, err := m.Edge.NodeAgentComponent(ctx, r)
	if err != nil {
		return nil, err
	}
	return []ComponentDigest{c}, nil
}

func (m ManifestOrEdge) ControlPlaneComponents(ctx context.Context, r Release) ([]ComponentDigest, error) {
	if len(r.Manifest) > 0 {
		parsed, err := ParseManifest(r.Manifest)
		if err != nil {
			return nil, err
		}
		if c := ControlPlaneComponents(parsed); len(c) > 0 {
			return c, nil
		}
	}
	if m.Edge == nil {
		return nil, errors.New("this release carries no manifest and this control plane cannot reach the registry to resolve one")
	}
	c, err := m.Edge.ControlPlaneComponent(ctx, r)
	if err != nil {
		return nil, err
	}
	return []ComponentDigest{c}, nil
}

// FleetRunner drives fleet runs. Its only in-process state is the goroutine set
// and the skip list; everything a resume needs is in Postgres.
type FleetRunner struct {
	store    fleetStore
	hosts    hostDriver
	self     selfDriver
	resolve  componentResolver
	cordons  FleetCordons
	view     func(ctx context.Context) (View, error)
	log      logger
	PollWait time.Duration
	// The control-plane drain's ceiling, from the attempt's created_at.
	Deadline time.Duration
	// How long a re-adopted run waits for its agents to come back.
	AdoptSettle time.Duration

	mu sync.Mutex
	// run id → cancel, bounded at one by the active-run index.
	running map[string]context.CancelFunc
	// run ids this process re-adopted rather than started, which is what the
	// settle window keys on.
	adopted map[string]bool
	// run id → hosts passed over. No column holds these (apply.go says why).
	skips map[string][]RunSkip

	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// NewFleetRunner builds the sequencer.
func NewFleetRunner(store fleetStore, hosts hostDriver, self selfDriver, resolve componentResolver,
	cordons FleetCordons, view func(ctx context.Context) (View, error), log logger) *FleetRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &FleetRunner{
		store: store, hosts: hosts, self: self, resolve: resolve, cordons: cordons,
		view: view, log: log,
		PollWait:    DefaultApplyPoll,
		Deadline:    DefaultApplyDeadline,
		AdoptSettle: DefaultAdoptSettle,
		running:     make(map[string]context.CancelFunc),
		adopted:     make(map[string]bool),
		skips:       make(map[string][]RunSkip),
		baseCtx:     ctx,
		stop:        cancel,
	}
}

// Start drives one run. Idempotent per run: a second Start for a run already
// being driven is dropped, so Adopt and the endpoint cannot double-drive one.
func (f *FleetRunner) Start(run ApplyRun) {
	ctx, cancel := context.WithCancel(f.baseCtx)
	f.mu.Lock()
	if _, busy := f.running[run.ID]; busy {
		f.mu.Unlock()
		cancel()
		return
	}
	f.running[run.ID] = cancel
	f.mu.Unlock()

	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer func() {
			f.mu.Lock()
			delete(f.running, run.ID)
			f.mu.Unlock()
			cancel()
		}()
		f.drive(ctx, run.ID)
	}()
}

// Adopt resumes the active run after a restart — including the restart the run
// itself caused, which is the normal case for its first target.
func (f *FleetRunner) Adopt(ctx context.Context) {
	run, err := f.store.ActiveRun(ctx)
	if err != nil {
		f.log.Error("could not re-adopt the active fleet run", "err", err)
		return
	}
	if run == nil {
		return
	}
	f.log.Warn("re-adopting a fleet apply left in flight by a restart",
		"run_id", run.ID, "state", run.State, "current_target", orEmpty(run.CurrentTarget))
	f.mu.Lock()
	f.adopted[run.ID] = true
	f.mu.Unlock()
	f.Start(*run)
}

// Close cancels the driving goroutine and waits. The run row stays non-terminal
// on purpose: the next boot's Adopt resumes it.
func (f *FleetRunner) Close() {
	f.stop()
	f.wg.Wait()
}

// Skips is what a run passed over, for the run's `skipped` field.
func (f *FleetRunner) Skips(runID string) []RunSkip {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RunSkip, len(f.skips[runID]))
	copy(out, f.skips[runID])
	return out
}

func (f *FleetRunner) recordSkip(runID string, skip RunSkip) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.skips[runID] {
		if s.HostID == skip.HostID {
			return
		}
	}
	f.skips[runID] = append(f.skips[runID], skip)
}

func (f *FleetRunner) drive(ctx context.Context, runID string) {
	run, err := f.store.Run(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not read the run", "run_id", runID, "err", err)
		return
	}
	if TerminalRunState(run.State) {
		return
	}
	if !f.controlPlanePhase(ctx, run) {
		return
	}
	if !f.hostPhase(ctx, run) {
		return
	}
	f.finish(runID, RunSucceeded, "")
}

// controlPlanePhase moves the control plane, or establishes that it needs no
// move. False means the run is finished (or this process is shutting down and
// the next boot resumes).
func (f *FleetRunner) controlPlanePhase(ctx context.Context, run ApplyRun) bool {
	attempts, err := f.store.RunAttempts(ctx, run.ID)
	if err != nil {
		f.finish(run.ID, RunFailed, "could not read this run's attempts: "+err.Error())
		return false
	}
	var cp *Attempt
	for i := range attempts {
		if attempts[i].Target == TargetControlPlane {
			cp = &attempts[i]
		}
	}

	if cp == nil {
		if f.cancelRequested(ctx, run.ID) {
			f.finish(run.ID, RunCancelled, "")
			return false
		}
		view, err := f.view(ctx)
		if err != nil {
			f.finish(run.ID, RunFailed, "could not read the release view: "+err.Error())
			return false
		}
		switch reason := fleetTargetReason(view, nil); reason {
		case "":
		case ReasonUpToDate:
			return true // already on it; the hosts are next
		default:
			// ADR 0002: nothing may move past a control plane that cannot.
			f.finish(run.ID, RunFailed, "the control plane cannot take this release: "+reason)
			return false
		}
		a, err := f.createControlPlaneAttempt(ctx, run)
		if err != nil {
			f.finish(run.ID, RunFailed, "could not start the control-plane update: "+err.Error())
			return false
		}
		if err := f.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
			f.log.Warn("fleet apply: could not record the current target", "run_id", run.ID, "err", err)
		}
		cp = &a
		if f.prepareFleet(ctx, run, a) {
			// Normally never returns: the updater recreates this container
			// partway through, and the next boot's Adopt resolves the row.
			f.self.Apply(ctx, a)
		}
	} else if !TerminalAttemptState(cp.State) {
		if err := f.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
			f.log.Warn("fleet apply: could not record the current target", "run_id", run.ID, "err", err)
		}
		// The fleet is re-cordoned on adoption: the run holds it for the rest of
		// its life, and the restart wiped the in-memory record of it.
		f.cordonFleet(ctx, run.ID)
		if !f.self.Adopt(ctx, *cp, f.releaseCommit(ctx, run.ReleaseID)) {
			if f.prepareFleet(ctx, run, *cp) {
				f.self.Apply(ctx, *cp) // never sent; re-drive it
			}
		}
	}

	final, err := f.store.Attempt(ctx, cp.ID)
	if err != nil {
		f.log.Error("fleet apply: could not re-read the control-plane attempt", "run_id", run.ID, "err", err)
		return false
	}
	switch final.State {
	case AttemptSucceeded:
		return true
	case AttemptCancelled:
		f.finish(run.ID, RunCancelled, "")
		return false
	case AttemptFailed:
		f.finish(run.ID, RunFailed, "")
		return false
	default:
		// Still open with the process alive: shutting down. Adopt resumes.
		return false
	}
}

// prepareFleet cordons the whole instance and waits for it to be empty, because
// recreating the control plane ends every session on it. False means the attempt
// resolved underneath (a cancel, a timeout) or the process is shutting down, and
// there is nothing to send.
func (f *FleetRunner) prepareFleet(ctx context.Context, run ApplyRun, a Attempt) bool {
	f.cordonFleet(ctx, run.ID)

	remaining, err := f.store.FleetNonTerminalSessions(ctx)
	if err != nil {
		f.log.Error("fleet apply: could not count sessions", "run_id", run.ID, "err", err)
		return true // the count is advisory; refusing to update over it would be worse
	}
	// The N the operator agreed to lose is recorded before the apply is sent,
	// forced or not.
	if err := f.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
		f.log.Warn("fleet apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
	}
	if run.Force {
		return true
	}

	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	deadline := started.Add(f.Deadline)
	for remaining > 0 {
		if time.Now().After(deadline) {
			f.log.Warn("fleet apply: the fleet did not drain before the deadline", "run_id", run.ID)
			f.failAttempt(a.ID, ReasonTimeout)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(f.PollWait):
		}
		// A cancel caught the attempt before it was sent, so it is already
		// resolved and the caller finishes the run.
		if cur, err := f.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
			return false
		}
		remaining, err = f.store.FleetNonTerminalSessions(ctx)
		if err != nil {
			f.log.Warn("fleet apply: session count failed while draining", "run_id", run.ID, "err", err)
			continue
		}
		if err := f.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
			f.log.Warn("fleet apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
		}
	}
	return true
}

// cordonFleet takes every online host out of scheduling for the rest of the run,
// remembering the ones it found already cordoned. Nothing must land on a host
// that is about to lose its agent.
func (f *FleetRunner) cordonFleet(ctx context.Context, runID string) {
	// Written BEFORE the first cordon and never overwritten: the run's own
	// control-plane step restarts this process, and a re-read afterwards would
	// record the run's own cordons as the operator's.
	if existing, err := f.store.CordonedHosts(ctx, runID); err == nil && len(existing) > 0 {
		return
	}
	hosts, err := f.store.Hosts(ctx)
	if err != nil {
		f.log.Error("fleet apply: could not read the host list to cordon it", "run_id", runID, "err", err)
		return
	}
	states := make([]HostCordon, 0, len(hosts))
	for _, h := range hosts {
		states = append(states, HostCordon{HostID: h.HostID, WasCordoned: h.Status != "online"})
	}
	if err := f.store.SetCordonedHosts(ctx, runID, states); err != nil {
		// Cordoning without a record of what to undo is how a fleet is left out
		// of scheduling with nothing left that knows to lift it.
		f.log.Error("fleet apply: could not record the fleet's scheduling state; not cordoning",
			"run_id", runID, "err", err)
		return
	}
	for _, st := range states {
		if st.WasCordoned {
			continue // an admin's cordon, restored rather than lifted
		}
		if err := f.cordons.Cordon(ctx, st.HostID); err != nil {
			f.log.Warn("fleet apply: could not cordon a host", "run_id", runID, "host_id", st.HostID, "err", err)
		}
	}
}

// restoreCordons puts every host back to the scheduling state the run found.
// Runs on every terminal path, including a failed one: a fleet left draining by
// a failed run would silently drop out of scheduling entirely.
func (f *FleetRunner) restoreCordons(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	states, err := f.store.CordonedHosts(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not read what to restore; hosts may be left out of scheduling",
			"run_id", runID, "err", err)
		return
	}
	for _, st := range states {
		if st.WasCordoned {
			if err := f.cordons.Cordon(ctx, st.HostID); err != nil {
				f.log.Warn("fleet apply: could not restore an admin cordon", "host_id", st.HostID, "err", err)
			}
			continue
		}
		if err := f.cordons.Uncordon(ctx, st.HostID); err != nil {
			// An offline host cannot be uncordoned; it returns online on its
			// agent's reconnect.
			f.log.Info("fleet apply: host not uncordoned (it will return online on its agent's reconnect)",
				"host_id", st.HostID, "err", err)
		}
	}
	// Loud, because a fleet silently out of scheduling is the failure mode this
	// whole path exists to avoid.
	for _, st := range states {
		if st.WasCordoned {
			continue
		}
		if status, err := f.store.HostStatus(ctx, st.HostID); err == nil && status != "online" {
			f.log.Error("fleet apply: a host this run cordoned is still out of scheduling; uncordon it by hand",
				"run_id", runID, "host_id", st.HostID, "status", status)
		}
	}
}

func (f *FleetRunner) failAttempt(attemptID, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.store.FailAttempt(ctx, attemptID, reason, ""); err != nil {
		f.log.Error("fleet apply: could not record the failure", "attempt_id", attemptID, "err", err)
	}
}

// hostPhase applies each eligible host in the view's order.
func (f *FleetRunner) hostPhase(ctx context.Context, run ApplyRun) bool {
	view, err := f.view(ctx)
	if err != nil {
		f.finish(run.ID, RunFailed, "could not read the release view: "+err.Error())
		return false
	}
	order := make([]Target, 0, len(view.Targets))
	for _, t := range view.Targets {
		if t.Kind == TargetHost && t.HostID != nil {
			order = append(order, t)
		}
	}

	// A re-adopted run boots seconds after the control plane started listening,
	// and its agents reconnect a beat later. Moving straight to the first host
	// sends into a registry that is still empty.
	f.mu.Lock()
	adopted := f.adopted[run.ID]
	delete(f.adopted, run.ID)
	f.mu.Unlock()
	if adopted && len(order) > 0 {
		f.log.Info("fleet apply: letting the fleet reconnect before the first host",
			"run_id", run.ID, "settle", f.AdoptSettle)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(f.AdoptSettle):
		}
	}

	for _, t := range order {
		hostID := *t.HostID
		attempts, err := f.store.RunAttempts(ctx, run.ID)
		if err != nil {
			f.finish(run.ID, RunFailed, "could not read this run's attempts: "+err.Error())
			return false
		}
		if a := attemptForHost(attempts, hostID); a != nil {
			if !f.resumeHost(ctx, run, *a) {
				return false
			}
			continue
		}
		// Between targets, and only here: a cancel never interrupts an attempt
		// that has been sent.
		if f.cancelRequested(ctx, run.ID) {
			f.finish(run.ID, RunCancelled, "")
			return false
		}
		view, err := f.view(ctx)
		if err != nil {
			f.finish(run.ID, RunFailed, "could not read the release view: "+err.Error())
			return false
		}
		if reason := fleetTargetReason(view, &hostID); reason != "" {
			f.recordSkip(run.ID, RunSkip{HostID: hostID, NodeName: nodeName(t), Reason: reason})
			f.log.Info("fleet apply: host skipped", "run_id", run.ID, "host_id", hostID, "reason", reason)
			continue
		}
		attempt, err := f.createHostAttempt(ctx, run, hostID)
		if errors.Is(err, ErrAttemptInFlight) {
			f.recordSkip(run.ID, RunSkip{HostID: hostID, NodeName: nodeName(t), Reason: ReasonAttemptInFlight})
			continue
		}
		if err != nil {
			f.finish(run.ID, RunFailed, "could not start the update on "+nodeName(t)+": "+err.Error())
			return false
		}
		if err := f.store.SetRunTarget(ctx, run.ID, TargetHost, &hostID); err != nil {
			f.log.Warn("fleet apply: could not record the current target", "run_id", run.ID, "err", err)
		}
		// The N the operator agreed to lose, recorded before the apply is sent.
		if n, err := f.store.NonTerminalSessions(ctx, hostID); err == nil {
			_ = f.store.SetWaitingSessions(ctx, attempt.ID, n)
		}
		f.hosts.Start(attempt)
		if !f.resumeHost(ctx, run, attempt) {
			return false
		}
	}
	return true
}

// resumeHost waits for one host attempt to resolve and applies the run's stop
// rule to the outcome. False means the run is finished or this process is
// shutting down.
func (f *FleetRunner) resumeHost(ctx context.Context, run ApplyRun, a Attempt) bool {
	final, ok := f.waitForAttempt(ctx, a.ID)
	if !ok {
		return false // shutting down; Adopt resumes
	}
	switch final.State {
	case AttemptSucceeded:
		return true
	case AttemptCancelled:
		f.finish(run.ID, RunCancelled, "")
		return false
	default:
		f.log.Warn("fleet apply stopped at a failed target",
			"run_id", run.ID, "host_id", orEmpty(a.HostID), "reason", orEmpty(final.Reason))
		f.finish(run.ID, RunFailed, "")
		return false
	}
}

func (f *FleetRunner) waitForAttempt(ctx context.Context, attemptID string) (Attempt, bool) {
	for {
		a, err := f.store.Attempt(ctx, attemptID)
		if err == nil && TerminalAttemptState(a.State) {
			return a, true
		}
		select {
		case <-ctx.Done():
			return Attempt{}, false
		case <-time.After(f.PollWait):
		}
	}
}

func (f *FleetRunner) createControlPlaneAttempt(ctx context.Context, run ApplyRun) (Attempt, error) {
	release, err := f.store.Release(ctx, run.ReleaseID)
	if err != nil {
		return Attempt{}, err
	}
	components, err := f.resolve.ControlPlaneComponents(ctx, release)
	if err != nil {
		return Attempt{}, err
	}
	if len(components) == 0 {
		return Attempt{}, fmt.Errorf("release %s names no control-plane image", releaseLabel(release))
	}
	last, err := f.store.LastSucceededControlPlaneDigests(ctx)
	if err != nil {
		return Attempt{}, err
	}
	return f.store.CreateControlPlaneAttempt(ctx, NewControlPlaneAttempt{
		RunID:     &run.ID,
		ReleaseID: &release.ID,
		Requested: components,
		Previous:  previousOrUnknown(last, components),
		Actor:     run.RequestedBy,
	})
}

func (f *FleetRunner) createHostAttempt(ctx context.Context, run ApplyRun, hostID string) (Attempt, error) {
	release, err := f.store.Release(ctx, run.ReleaseID)
	if err != nil {
		return Attempt{}, err
	}
	components, err := f.resolve.HostComponents(ctx, release)
	if err != nil {
		return Attempt{}, err
	}
	if len(components) == 0 {
		return Attempt{}, fmt.Errorf("release %s names no node-agent image", releaseLabel(release))
	}
	last, err := f.store.LastSucceededDigests(ctx, hostID)
	if err != nil {
		return Attempt{}, err
	}
	return f.store.CreateHostAttempt(ctx, NewHostAttempt{
		Kind:      KindApply,
		RunID:     &run.ID,
		HostID:    hostID,
		ReleaseID: &release.ID,
		Requested: components,
		Previous:  previousOrUnknown(last, components),
		Force:     run.Force,
		Actor:     run.RequestedBy,
	})
}

func (f *FleetRunner) releaseCommit(ctx context.Context, releaseID string) string {
	r, err := f.store.Release(ctx, releaseID)
	if err != nil {
		return ""
	}
	return r.SourceCommit
}

func (f *FleetRunner) cancelRequested(ctx context.Context, runID string) bool {
	run, err := f.store.Run(ctx, runID)
	if err != nil {
		return false
	}
	return run.CancelRequested
}

func (f *FleetRunner) finish(runID, state, errText string) {
	// Every terminal transition comes through here, which is what makes the
	// fleet cordon impossible to leak.
	defer f.restoreCordons(runID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.store.FinishRun(ctx, runID, state, errText); err != nil {
		f.log.Error("fleet apply: could not resolve the run", "run_id", runID, "state", state, "err", err)
		return
	}
	f.log.Info("fleet apply finished", "run_id", runID, "state", state)
}

// fleetTargetReason is the view's eligibility, seen from inside the run that
// holds the fleet: `run_active` is this run, so it is not an ineligibility
// here. hostID nil is the control-plane target.
func fleetTargetReason(v View, hostID *string) string {
	for _, t := range v.Targets {
		switch {
		case hostID == nil && t.Kind != TargetControlPlane:
			continue
		case hostID != nil && (t.Kind != TargetHost || t.HostID == nil || *t.HostID != *hostID):
			continue
		}
		if t.Eligible {
			return ""
		}
		if t.Reason == nil {
			return ReasonIdentityUnknown
		}
		if *t.Reason == ReasonRunActive {
			return ""
		}
		return *t.Reason
	}
	return ReasonIdentityUnknown
}

func attemptForHost(attempts []Attempt, hostID string) *Attempt {
	for i := range attempts {
		if attempts[i].HostID != nil && *attempts[i].HostID == hostID {
			return &attempts[i]
		}
	}
	return nil
}

// orEmpty renders a nullable log field as its value, not its address.
func orEmpty(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func nodeName(t Target) string {
	if t.NodeName != nil {
		return *t.NodeName
	}
	return ""
}

// previousOrUnknown is what a target is demonstrably on: the digests of its
// last succeeded attempt, or the requested component names with null digests —
// "nobody looked", which a client can tell from "there was nothing there".
func previousOrUnknown(last, requested []ComponentDigest) []PreviousDigest {
	if len(last) == 0 {
		return unknownPrevious(requested)
	}
	out := make([]PreviousDigest, 0, len(last))
	for _, c := range last {
		digest := c.Digest
		out = append(out, PreviousDigest{Name: c.Name, Digest: &digest})
	}
	return out
}
