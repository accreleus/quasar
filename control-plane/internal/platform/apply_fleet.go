package platform

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The fleet sequencer: one release across the instance, the control plane first
// and then every eligible host in sequence (ADR 0002).
// semantics: control-api.md §"Platform-release apply"
//
// Rules a plausible edit breaks:
//   - A run STOPS at its first failed target. Past a failed control plane,
//     continuing would move agents onto a release the control plane is not on;
//     past a failed host, it would march a known-bad digest set across the fleet.
//   - An ineligible host is SKIPPED, not failed: a run must not go `failed`
//     because a host happened to be offline.
//   - The cancel flag is read BETWEEN targets and never mid-attempt.
//   - The fleet stays cordoned until the run is terminal, whatever the
//     control-plane step does about sessions.
//   - EVERY cordon the run takes is recorded before it is taken, and restored
//     from that record when the run ends: the fleet-wide one before the
//     control-plane step, the per-host one before each host step. A run whose
//     control plane is already current takes no fleet cordon at all, and a
//     cordon with no record is one nothing will ever lift (#200).
//   - The control-plane target drains the fleet first only when the release
//     carries a migration; prepareFleet has the argument.
//   - Even a non-migrating control-plane step waits for IN-FLIGHT sessions
//     (non-terminal but not yet `running`) to settle: those are the rows the
//     reconnecting agent fails, and the old fleet-wide drain used to make the
//     set empty by accident.
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
	RecordSkip(ctx context.Context, runID string, skip RunSkip) error
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	Hosts(ctx context.Context) ([]HostIdentity, error)
	HostStatus(ctx context.Context, hostID string) (string, error)
	SetCordonedHosts(ctx context.Context, runID string, states []HostCordon) error
	CordonedHosts(ctx context.Context, runID string) ([]HostCordon, error)
	MarkCordonsRestored(ctx context.Context, runID string) error
	ClaimUnrestoredCordons(ctx context.Context, limit int) ([]string, error)
	FleetNonTerminalSessions(ctx context.Context) (int, error)
	FleetInFlightSessions(ctx context.Context) (int, error)
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
	// Drain STOPS a host's sessions as well as cordoning it. Only the migrating
	// control-plane step under `force` uses it: `force` is the operator agreeing
	// to end N live sessions, and since #128 the recreate no longer ends them as
	// a side effect, so something has to (#153). Optional — a nil Drain makes
	// that path behave as if force had not been sent, which is the safe default
	// for a caller that has not wired it.
	Drain func(ctx context.Context, hostID string) error
}

// HostCordon is what the run found for one host before it cordoned, persisted
// on the run row (migration 0076).
type HostCordon struct {
	HostID string `json:"host_id"`
	// The host was ALREADY out of scheduling, so the run must leave it that way.
	WasCordoned bool `json:"was_cordoned"`
}

// DefaultInFlightSettle bounds the non-migrating control-plane step's wait for
// in-flight launches to finish arriving (#153). Short on purpose: the fleet is
// already cordoned, so nothing new is being placed and the set converges in
// seconds. Expiry PROCEEDS rather than failing the attempt — the cost of being
// wrong is one launch that would have failed anyway, never a `running` session,
// and stalling a release on a wedged `starting` row would be worse.
const DefaultInFlightSettle = 45 * time.Second

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
	// How long a non-migrating control-plane step waits for in-flight launches.
	InFlightSettle time.Duration
	// SchemaVersion is the highest migration this control plane embeds, which
	// after boot is also the database's applied version. A field rather than a
	// buildinfo call at the point of use, so a test can put a release on either
	// side of it.
	SchemaVersion int

	mu sync.Mutex
	// run id → cancel, bounded at one by the active-run index.
	running map[string]context.CancelFunc
	// run ids this process re-adopted rather than started, which is what the
	// settle window keys on.
	adopted map[string]bool
	// run ids with a skip the store refused to record: fail towards partial.
	unrecordedSkips map[string]bool

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
		PollWait:        DefaultApplyPoll,
		Deadline:        DefaultApplyDeadline,
		AdoptSettle:     DefaultAdoptSettle,
		InFlightSettle:  DefaultInFlightSettle,
		SchemaVersion:   buildinfo.SchemaVersion(),
		running:         make(map[string]context.CancelFunc),
		adopted:         make(map[string]bool),
		unrecordedSkips: make(map[string]bool),
		baseCtx:         ctx,
		stop:            cancel,
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

// recordSkip persists one host the run passed over: the run's terminal state
// is decided from this list (RunOutcome), and it must survive the restart the
// run itself causes. A skip that could not be written is remembered so the
// outcome still reads partial rather than clean.
func (f *FleetRunner) recordSkip(ctx context.Context, runID string, skip RunSkip) {
	if err := f.store.RecordSkip(ctx, runID, skip); err != nil {
		f.log.Warn("fleet apply: could not record a skipped host", "run_id", runID,
			"host_id", skip.HostID, "err", err, "token", "fleet-apply-skip-unrecorded")
		f.mu.Lock()
		f.unrecordedSkips[runID] = true
		f.mu.Unlock()
	}
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
	// Re-read: the skips were written as the hosts were reached, and the
	// outcome is decided from what was persisted, not from what this process
	// remembers.
	final, err := f.store.Run(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not re-read the run to decide its outcome",
			"run_id", runID, "err", err, "token", "fleet-apply-outcome-unread")
		f.finish(runID, RunFailed, "could not re-read the run to decide its outcome: "+err.Error())
		return
	}
	outcome := RunOutcome(final.Skipped)
	f.mu.Lock()
	unrecorded := f.unrecordedSkips[runID]
	delete(f.unrecordedSkips, runID)
	f.mu.Unlock()
	if unrecorded {
		outcome = RunSucceededPartial
	}
	if outcome == RunSucceededPartial {
		f.log.Warn("fleet apply finished with hosts left behind", "run_id", runID,
			"skipped", len(final.Skipped), "token", "fleet-apply-partial")
	}
	f.finish(runID, outcome, "")
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
		// #122 decision 1, enforced HERE and not only where the run was
		// decided. An unattended run must never drain the instance, and the
		// drain decision below is re-made from the store row rather than from
		// the view the scheduler read — so a `schema_version` that moved under
		// us, or a release row that simply cannot be read (which
		// releaseRunsAMigration deliberately treats AS migrating), would
		// otherwise turn a run nobody is watching into a fleet-wide outage.
		// The trigger's check is necessary, not sufficient.
		if run.Unattended && f.releaseRunsAMigration(ctx, run) {
			f.log.Warn("fleet apply: refusing an unattended run whose release would migrate the database",
				"run_id", run.ID, "release_id", run.ReleaseID, "token", "unattended-refused-migrating")
			f.finish(run.ID, RunFailed,
				"unattended update refused: this release would migrate the database (or its row could not be read), "+
					"which drains every session on the instance — apply it yourself when you are watching")
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
		// its life, and the restart it caused may have lifted it underneath.
		f.adoptCordons(ctx, run.ID)
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

// prepareFleet cordons the whole instance and decides what the control-plane
// step owes its sessions. False means the attempt resolved underneath (a
// cancel, a timeout) or the process is shutting down.
//
// Since #128 a control-plane recreate no longer ends a RUNNING session: the
// agent holds it and the browser keeps its media path (live-gated 2026-09-08,
// 1080p60 at 60 fps through a 73 s outage —
// docs/reports/2026-09-08-128-session-survival-gate/). Two things still follow
// from that, and they are separate:
//
//   - A MIGRATING release drains the whole fleet, for a reason that was never
//     the restart: the held row is read back by a binary that has just migrated
//     the database under it, and every migration was authored assuming no
//     session was live — 0027 moved the signalling token out of `sessions` on
//     exactly that assumption. Nothing checks a migration for it and this code
//     cannot read the SQL, so it must not gamble. Under `force` the sessions are
//     STOPPED rather than waited for, because `force` is the operator agreeing
//     to end them and nothing else ends them any more; the wait to zero still
//     happens, it is just short.
//   - Either way the step waits for IN-FLIGHT sessions — non-terminal but not
//     yet `running` — to settle. Those are precisely the rows the reconnecting
//     agent fails (session.Store.ReapHostExceptRunning), and the old fleet-wide
//     drain made the set empty as a side effect. Without this a user who pressed
//     Play two seconds before the admin pressed Update loses their launch.
func (f *FleetRunner) prepareFleet(ctx context.Context, run ApplyRun, a Attempt) bool {
	// Cordon either way: every host in the run is about to be recreated, so a
	// session that lands mid-run is one the run would end at that host's step.
	f.cordonFleet(ctx, run.ID)

	if !f.releaseRunsAMigration(ctx, run) {
		// No SetWaitingSessions on this path: it would move the attempt into
		// `waiting_sessions`, and no `running` session is being waited on or
		// lost. The count is logged instead, because what rode across the
		// restart is this path's only evidence.
		held, err := f.store.FleetNonTerminalSessions(ctx)
		if err != nil {
			f.log.Warn("fleet apply: could not count sessions", "run_id", run.ID, "err", err)
			// Never log a count the read did not produce: this line is the
			// evidence, and "sessions=0" would read as "there were none".
			f.log.Info("fleet apply: the control-plane step is holding live sessions; this release runs no migration",
				"run_id", run.ID, "sessions", "unknown", "token", "cp-step-holds-sessions")
		} else {
			f.log.Info("fleet apply: the control-plane step is holding live sessions; this release runs no migration",
				"run_id", run.ID, "sessions", held, "token", "cp-step-holds-sessions")
		}
		return f.settleInFlight(ctx, run, a)
	}

	// From here the count is not advisory. Past this wait the database is
	// migrated, and only a read that SUCCEEDED and said zero is evidence that
	// nothing is live to migrate under (#175).
	remaining, known := f.countFleetSessions(ctx, run.ID, "before the control-plane step")
	if known {
		// The N the operator agreed to lose is recorded BEFORE anything ends it,
		// forced or not — on the forced path the count is about to be zero, and a
		// watcher seeing only that would never learn what the run cost. A count
		// that did not read is not recorded at all: `sessions_remaining` is a
		// number a client shows an operator, and there is no honest number here.
		if err := f.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
			f.log.Warn("fleet apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
		}
	}

	if run.Force && (!known || remaining != 0) {
		// Stop what the operator agreed to end. Pre-#128 the recreate did this
		// by itself and `force` only had to skip the wait; it no longer does, so
		// a force that merely skipped would run the migration under the very
		// sessions it claimed to end. The wait below still runs — it is just
		// short now, because something is actually ending them.
		//
		// Guarded on the count: with nothing to end, force has nothing to
		// discharge, and a fleet-wide session_stop is not a side effect to take
		// for the sake of symmetry. An UNKNOWN count drains rather than
		// skipping — `force` is consent to end the sessions, and the unknown
		// case is the one where they may still be there.
		f.stopFleetSessions(ctx, run)
		remaining, known = f.countFleetSessions(ctx, run.ID, "after the force drain")
	}

	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	deadline := started.Add(f.Deadline)
	// An unknown count keeps waiting exactly as a positive one does. The deadline
	// still bounds it: a store that never answers ends as a `timeout` failure,
	// not as a migration over live sessions.
	for !known || remaining != 0 {
		if time.Now().After(deadline) {
			if !known {
				f.log.Error("fleet apply: the fleet's session count never read before the deadline; refusing the migrating step",
					"run_id", run.ID, "token", "cp-step-count-unreadable")
			} else {
				f.log.Warn("fleet apply: the fleet did not drain before the deadline", "run_id", run.ID)
			}
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
		// Assign only on a read that worked. The old code assigned the store's
		// `(0, err)` zero and then looked at the error, so the `continue` below
		// re-tested the loop condition against a zero no read had produced.
		n, ok := f.countFleetSessions(ctx, run.ID, "while draining")
		if !ok {
			known = false
			continue
		}
		remaining, known = n, true
		if err := f.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
			f.log.Warn("fleet apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
		}
	}
	return true
}

// countFleetSessions reads the fleet's non-terminal session count and says
// whether it read one. The second return is the whole point: the store answers a
// failed read with `(0, error)`, and every wait on the migrating path is written
// against the count — so a count taken straight from a failed read says "the
// fleet has drained" at the one moment that claim is load-bearing (#175).
//
// Not a sentinel in the numeric domain. `-1` would still be an int the next
// edit could compare, add to, or hand to `SetWaitingSessions`; a separate
// boolean makes "there is no count" unrepresentable as one. `when` names the
// moment for the log, and a failed count here is an error rather than a warning
// because it is about to hold up a release.
func (f *FleetRunner) countFleetSessions(ctx context.Context, runID, when string) (int, bool) {
	n, err := f.store.FleetNonTerminalSessions(ctx)
	if err != nil {
		f.log.Error("fleet apply: could not count the fleet's sessions",
			"run_id", runID, "when", when, "err", err, "token", "cp-step-count-failed")
		return 0, false
	}
	return n, true
}

// releaseRunsAMigration answers what prepareFleet branches on. An unreadable
// release reads as migrating: an unnecessary drain costs sessions visibly, a
// missing one runs a migration under live sessions and nothing sees it.
func (f *FleetRunner) releaseRunsAMigration(ctx context.Context, run ApplyRun) bool {
	release, err := f.store.Release(ctx, run.ReleaseID)
	if err != nil {
		f.log.Warn("fleet apply: could not read the release to decide the control-plane drain; draining the fleet",
			"run_id", run.ID, "err", err)
		return true
	}
	return ReleaseRunsAMigration(release, f.SchemaVersion)
}

// stopFleetSessions ends every session the run is about to disturb, on the
// migrating-plus-`force` path only. It drains the hosts the run recorded rather
// than the live host list: that record is what the run owns, and it is the same
// set cordonFleet/adoptCordons act on, so a host an admin cordoned for their own
// reasons is not handed a session_stop by this run.
//
// A nil Drain (a caller that has not wired it) leaves the sessions alone; the
// wait below then behaves exactly as an unforced attempt, which is the safe
// reading of "we were asked to end them and cannot".
func (f *FleetRunner) stopFleetSessions(ctx context.Context, run ApplyRun) {
	if f.cordons.Drain == nil {
		f.log.Warn("fleet apply: force was requested but no drain is wired; waiting for the fleet to empty instead",
			"run_id", run.ID)
		return
	}
	states, err := f.store.CordonedHosts(ctx, run.ID)
	if err != nil {
		f.log.Error("fleet apply: could not read what this run cordoned, so nothing was force-drained",
			"run_id", run.ID, "err", err)
		return
	}
	for _, st := range states {
		if err := f.cordons.Drain(ctx, st.HostID); err != nil {
			f.log.Warn("fleet apply: could not force-drain a host", "run_id", run.ID, "host_id", st.HostID, "err", err)
		}
	}
	f.log.Info("fleet apply: force-drained the fleet ahead of a migrating control-plane step",
		"run_id", run.ID, "hosts", len(states), "token", "cp-step-force-drain")
}

// settleInFlight waits for the non-migrating control-plane step's in-flight
// sessions to reach zero. It never touches the attempt's state: nothing here is
// a drain the operator consented to, so `waiting_sessions` and
// `sessions_remaining` stay out of it, and expiry proceeds with a warning
// instead of failing the attempt (DefaultInFlightSettle explains why). `force`
// skips it, consistent with every other wait on this path.
//
// Returns false only when the attempt resolved underneath or the process is
// shutting down — the same contract as prepareFleet's own return.
func (f *FleetRunner) settleInFlight(ctx context.Context, run ApplyRun, a Attempt) bool {
	if run.Force {
		return true
	}
	deadline := time.Now().Add(f.InFlightSettle)
	for {
		inFlight, err := f.store.FleetInFlightSessions(ctx)
		if err != nil {
			f.log.Warn("fleet apply: could not count in-flight sessions", "run_id", run.ID, "err", err)
			return true // advisory; a failed count must not hold up the release
		}
		if inFlight == 0 {
			return true
		}
		if time.Now().After(deadline) {
			f.log.Warn("fleet apply: in-flight launches did not settle before the control-plane step; they will not survive it",
				"run_id", run.ID, "in_flight", inFlight, "token", "cp-step-inflight-timeout")
			return true
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
	}
}

// cordonFleet takes every online host out of scheduling for the rest of the run,
// remembering the ones it found already cordoned. Nothing must land on a host
// that is about to lose its agent. This is the FRESH-START half; adoptCordons is
// the other, and a record already present means the run has been here before.
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
		// `== "draining"`, not `!= "online"`. Only `draining` is a cordon. Treating
		// `offline` as one meant an offline-at-start host that reconnected mid-run
		// was recorded as the admin's: the run never cordoned it, so it could take
		// placements the run then destroyed at its step, and `restoreCordons`
		// CORDONED it at finish — leaving a host nobody cordoned `draining` with
		// nothing to lift it. The #140 shape, by a different path (#170).
		states = append(states, HostCordon{HostID: h.HostID, WasCordoned: h.Status == "draining"})
	}
	f.recordAndCordon(ctx, runID, states)
}

// adoptCordons re-establishes the fleet cordon on a run this process did not
// start. The record is authoritative — the live statuses are not, because the
// process that cordoned the fleet is the one that just went away.
func (f *FleetRunner) adoptCordons(ctx context.Context, runID string) {
	states, err := f.store.CordonedHosts(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not read what this run cordoned", "run_id", runID, "err", err)
		return
	}
	if len(states) == 0 {
		// #140: a run started before migration 0076 has no record, and every
		// host reads `draining` because THAT process cordoned it. Reading those
		// statuses as the operator's intent re-cordons the whole fleet at finish
		// with nothing left to lift it. An admin cordon predating such a run is
		// lifted instead — one-time, and logged here.
		f.log.Warn("fleet apply: this run predates the cordon record; treating every cordon as the run's own",
			"run_id", runID)
		hosts, err := f.store.Hosts(ctx)
		if err != nil {
			f.log.Error("fleet apply: could not read the host list to cordon it", "run_id", runID, "err", err)
			return
		}
		states = make([]HostCordon, 0, len(hosts))
		for _, h := range hosts {
			states = append(states, HostCordon{HostID: h.HostID, WasCordoned: false})
		}
		f.recordAndCordon(ctx, runID, states)
		return
	}
	// The run holds the fleet for the rest of its life, and between the two
	// processes an admin (or an agent's re-register, historically) may have
	// lifted a cordon. Cordon is idempotent on an already-draining host.
	for _, st := range states {
		if st.WasCordoned {
			continue // an admin's cordon, restored rather than lifted
		}
		if err := f.cordons.Cordon(ctx, st.HostID); err != nil {
			f.log.Warn("fleet apply: could not re-cordon a host", "run_id", runID, "host_id", st.HostID, "err", err)
		}
	}
}

// recordAndCordon persists what the run found, then cordons what it owns.
// Cordoning without a record of what to undo is how a fleet is left out of
// scheduling with nothing left that knows to lift it, so a failed write cordons
// nothing.
func (f *FleetRunner) recordAndCordon(ctx context.Context, runID string, states []HostCordon) {
	if err := f.store.SetCordonedHosts(ctx, runID, states); err != nil {
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

// cordonForHostStep records and takes the cordon for the ONE host a run is
// about to update, and answers whether the run may proceed to it.
//
// It exists because the fleet cordon is the control-plane step's (cordonFleet
// runs inside prepareFleet): a run whose control plane is already on the
// release never takes one, and recorded `cordoned_hosts: []` while its host
// steps cordoned hosts that only the returning agent's register put back. An
// attempt the updater refuses before any recreate — `updater_absent`, `busy`,
// a rejected namespace — has no returning agent, so that host stayed `draining`
// with nothing that knew to lift it (#200).
//
// It is deliberately NOT cordonFleet on this path. A host-only run recreates one
// host at a time; taking the whole instance out of scheduling for the life of
// the run — including hosts it will skip as up-to-date — would cost availability
// the run does not need. The control-plane step is the instance-wide event, and
// it keeps the instance-wide cordon.
//
// A host already in the record is left alone: re-reading its status now would
// read the RUN's own cordon as the operator's, which is the #140/#170
// conflation. The record is written BEFORE the cordon and a failed write takes
// no cordon at all — recordAndCordon's rule, for the same reason: a cordon with
// no record of what to undo is the leak this whole path exists to avoid. That
// is why a failed write stops the run here rather than applying to an
// unrecorded host.
//
// The resume path does not call this: the record is written before the attempt
// exists, so an attempt there is to resume already has one. A run adopted from a
// control plane older than this fix can hold a host attempt with no record, and
// nothing here can tell that attempt's cordon from an operator's — the same
// one-time window adoptCordons documents for #140.
func (f *FleetRunner) cordonForHostStep(ctx context.Context, runID, hostID string) bool {
	states, err := f.store.CordonedHosts(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not read what this run has cordoned",
			"run_id", runID, "host_id", hostID, "err", err)
		return false
	}
	for _, st := range states {
		if st.HostID == hostID {
			return true // already the run's to restore, whoever recorded it
		}
	}
	status, err := f.store.HostStatus(ctx, hostID)
	if err != nil {
		f.log.Error("fleet apply: could not read a host's scheduling state before updating it",
			"run_id", runID, "host_id", hostID, "err", err)
		return false
	}
	// `== "draining"`, not `!= "online"`: only draining is a cordon, and an
	// offline host recorded as the operator's would be CORDONED at restore
	// (#170).
	st := HostCordon{HostID: hostID, WasCordoned: status == "draining"}
	if err := f.store.SetCordonedHosts(ctx, runID, append(states, st)); err != nil {
		f.log.Error("fleet apply: could not record a host's scheduling state; not cordoning it",
			"run_id", runID, "host_id", hostID, "err", err)
		return false
	}
	if st.WasCordoned {
		return true // the operator's cordon: already out of scheduling, and restored rather than lifted
	}
	if err := f.cordons.Cordon(ctx, hostID); err != nil {
		// Not fatal here: the attempt cordons too, and fails itself if it
		// cannot (a host that cannot be cordoned cannot be drained). The record
		// is already written either way, so whatever ends up cordoned is lifted.
		f.log.Warn("fleet apply: could not cordon a host before its step",
			"run_id", runID, "host_id", hostID, "err", err)
	}
	return true
}

// MaxCordonRestoreSweep bounds ResumeCordonRestores. A boot sweep, not a backlog
// drain: a run that keeps failing to settle is retried on the NEXT boot rather
// than in a loop on this one.
const MaxCordonRestoreSweep = 20

// CordonRestoreSweepBudget bounds the whole sweep, not each run in it. Startup
// calls it synchronously, so an unreachable scheduling backend must not be able
// to hold the control plane down for the sum of every run's own timeout.
const CordonRestoreSweepBudget = 60 * time.Second

// ResumeCordonRestores finishes the scheduling cleanup of runs that ended
// without it. `finish` writes the terminal state before it restores cordons, so
// a restore that failed — or a process that died in that window — leaves a
// terminal run whose hosts are still `draining`, and `ActiveRun` selects only
// non-terminal runs, so nothing else on this path would ever look at it again
// (#176). Idempotent: a run whose hosts are already back settles on the first
// pass and is stamped.
//
// IT REFUSES TO RUN WHILE A FLEET RUN IS ACTIVE, and it must be called BEFORE
// Adopt. An old run's record says "this host was not cordoned when I found it",
// which is a claim about a moment that has passed: replaying it against a fleet
// a LIVE run has deliberately cordoned would put hosts back into scheduling in
// the middle of an update. Ordering plus the refusal is what closes that at
// startup — no run is active in the database and none has been started by this
// process, and the API is not serving yet, so no new run can appear underneath.
//
// It does NOT close the case where an ADMIN cordoned the host after the failed
// run: that record still reads as the run's to lift, because a cordon carries no
// owner. See #183.
func (f *FleetRunner) ResumeCordonRestores(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, CordonRestoreSweepBudget)
	defer cancel()

	active, err := f.store.ActiveRun(ctx)
	if err != nil {
		f.log.Error("fleet apply: could not check for an active run before scheduling cleanup", "err", err)
		return
	}
	if active != nil {
		f.log.Info("fleet apply: deferring unfinished scheduling cleanup while a fleet run is active",
			"run_id", active.ID, "token", "cordon-restore-deferred")
		return
	}

	ids, err := f.store.ClaimUnrestoredCordons(ctx, MaxCordonRestoreSweep)
	if err != nil {
		f.log.Error("fleet apply: could not look for unfinished scheduling cleanup", "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			f.log.Warn("fleet apply: the scheduling-cleanup sweep ran out of time; the rest wait for the next start",
				"token", "cordon-restore-budget-spent")
			return
		}
		f.log.Warn("fleet apply: resuming the scheduling cleanup of a run that finished without it",
			"run_id", id, "token", "cordon-restore-resumed")
		f.settleCordons(ctx, id)
	}
}

// settleCordons restores what a run cordoned and RECORDS whether that worked.
// The unfinished case is a recovery requirement rather than a log line: the
// marker stays unset, and the next start's ResumeCordonRestores retries it (#176).
func (f *FleetRunner) settleCordons(parent context.Context, runID string) {
	if !f.restoreCordons(parent, runID) {
		f.log.Error("fleet apply: this run's scheduling cleanup is UNFINISHED; it will be retried on the next control-plane start",
			"run_id", runID, "token", "cordon-restore-unfinished")
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := f.store.MarkCordonsRestored(ctx, runID); err != nil {
		// The work is done; only the record of it is missing, so the next start
		// re-does an idempotent restore rather than leaving a host cordoned.
		f.log.Error("fleet apply: scheduling was restored but recording it failed; the next start will re-check",
			"run_id", runID, "err", err)
	}
}

// restoreCordons puts back what the run changed. It reports whether EVERY change
// was proven undone — a false answer is what settleCordons turns into a durable
// recovery requirement, so "could not tell" counts as not restored (#176).
func (f *FleetRunner) restoreCordons(parent context.Context, runID string) bool {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	states, err := f.store.CordonedHosts(ctx, runID)
	if err != nil {
		f.log.Error("fleet apply: could not read what to restore; hosts may be left out of scheduling",
			"run_id", runID, "err", err)
		return false
	}
	restored := true
	for _, st := range states {
		if st.WasCordoned {
			if err := f.cordons.Cordon(ctx, st.HostID); err != nil {
				f.log.Warn("fleet apply: could not restore an admin cordon", "host_id", st.HostID, "err", err)
				// Confirm rather than conclude, the same way the uncordon half
				// does below: a request can fail after it took effect, and a
				// host can have been deleted since the run recorded it. Reading
				// the error as final would leave a requirement that can never be
				// discharged, retried on every start for the rest of the
				// instance's life.
				switch status, serr := f.store.HostStatus(ctx, st.HostID); {
				case errors.Is(serr, ErrHostNotFound):
					// Gone. There is no cordon left to put back.
				case serr == nil && status == "draining":
					// The cordon is in place; only the answer was lost.
				default:
					restored = false
				}
			}
			continue
		}
		if err := f.cordons.Uncordon(ctx, st.HostID); err != nil {
			// An offline host cannot be uncordoned; it returns online on its
			// agent's reconnect. Not counted against `restored` — the check
			// below decides that from the host's actual status.
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
		// `draining`, not `!= "online"`. UncordonHost lifts an agentless host's
		// cordon to `offline` and returns nil — documented behaviour, and the
		// cordon IS gone: a reconnect turns `offline` back into `online`. Reading
		// that success as "still out of scheduling" made every run with an absent
		// host end on an ERROR telling the operator to fix something that was
		// already fine (#170).
		status, err := f.store.HostStatus(ctx, st.HostID)
		switch {
		case errors.Is(err, ErrHostNotFound):
			// Deleted since the run cordoned it. There is nothing to put back,
			// and nothing to retry over for the rest of this instance's life.
		case err != nil:
			f.log.Warn("fleet apply: could not confirm a host is back in scheduling",
				"run_id", runID, "host_id", st.HostID, "err", err)
			restored = false
		case status == "draining":
			f.log.Error("fleet apply: a host this run cordoned is still out of scheduling",
				"run_id", runID, "host_id", st.HostID, "status", status)
			restored = false
		}
	}
	return restored
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
			f.recordSkip(ctx, run.ID, RunSkip{HostID: hostID, NodeName: nodeName(t), Reason: reason})
			f.log.Info("fleet apply: host skipped", "run_id", run.ID, "host_id", hostID, "reason", reason)
			continue
		}
		// Before the attempt, because the attempt cordons this host and leaves
		// the restore to the run (apply_runner.go `drive`, #140). A run that
		// skipped its control-plane step never went through prepareFleet, so
		// without this it holds no record for the host it is about to take out
		// of scheduling (#200). A host skipped below because an attempt is
		// already in flight keeps the cordon recorded here — recorded is
		// exactly what makes the run's own finish lift it.
		if !f.cordonForHostStep(ctx, run.ID, hostID) {
			f.finish(run.ID, RunFailed,
				"could not record the scheduling state of "+nodeName(t)+" before updating it")
			return false
		}
		attempt, err := f.createHostAttempt(ctx, run, hostID)
		if errors.Is(err, ErrAttemptInFlight) {
			f.recordSkip(ctx, run.ID, RunSkip{HostID: hostID, NodeName: nodeName(t), Reason: ReasonAttemptInFlight})
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
	defer f.settleCordons(context.Background(), runID)
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
