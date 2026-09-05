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
	CreateHostAttempt(ctx context.Context, in NewHostAttempt) (Attempt, error)
	CreateControlPlaneAttempt(ctx context.Context, in NewControlPlaneAttempt) (Attempt, error)
	LastSucceededDigests(ctx context.Context, hostID string) ([]ComponentDigest, error)
	LastSucceededControlPlaneDigests(ctx context.Context) ([]ComponentDigest, error)
	Release(ctx context.Context, id string) (Release, error)
	NonTerminalSessions(ctx context.Context, hostID string) (int, error)
	SetWaitingSessions(ctx context.Context, attemptID string, remaining int) error
}

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
	view     func(ctx context.Context) (View, error)
	log      logger
	PollWait time.Duration

	mu sync.Mutex
	// run id → cancel, bounded at one by the active-run index.
	running map[string]context.CancelFunc
	// run id → hosts passed over. No column holds these (apply.go says why).
	skips map[string][]RunSkip

	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// NewFleetRunner builds the sequencer.
func NewFleetRunner(store fleetStore, hosts hostDriver, self selfDriver, resolve componentResolver,
	view func(ctx context.Context) (View, error), log logger) *FleetRunner {
	ctx, cancel := context.WithCancel(context.Background())
	return &FleetRunner{
		store: store, hosts: hosts, self: self, resolve: resolve, view: view, log: log,
		PollWait: DefaultApplyPoll,
		running:  make(map[string]context.CancelFunc),
		skips:    make(map[string][]RunSkip),
		baseCtx:  ctx,
		stop:     cancel,
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
		"run_id", run.ID, "state", run.State, "current_target", run.CurrentTarget)
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
		// Normally never returns: the updater recreates this container partway
		// through, and the next boot's Adopt resolves the row.
		f.self.Apply(ctx, a)
	} else if !TerminalAttemptState(cp.State) {
		if err := f.store.SetRunTarget(ctx, run.ID, TargetControlPlane, nil); err != nil {
			f.log.Warn("fleet apply: could not record the current target", "run_id", run.ID, "err", err)
		}
		if !f.self.Adopt(ctx, *cp, f.releaseCommit(ctx, run.ReleaseID)) {
			f.self.Apply(ctx, *cp) // never sent; re-drive it
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
			"run_id", run.ID, "host_id", a.HostID, "reason", final.Reason)
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
