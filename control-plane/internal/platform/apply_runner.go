package platform

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The per-host apply state machine. One goroutine per attempt, context-scoped,
// owning its state and mutex. semantics: control-api.md §"Platform-release apply"
//
//	queued → cordon (remembering the cordon found) → waiting_sessions, polling
//	the live session count → persist the request id → send release_apply →
//	relay release_state → resolved.
//
// Two rules a plausible edit breaks:
//   - Success is the new agent's `register`, never a `release_state`: the
//     recreate kills the process that would have reported it.
//   - This package must never stop a session. `force` records that the control
//     plane decided they may die; the recreate is what kills them.

// Contract timings, fields rather than consts so a test can compress them. No
// env knob: a deadline a client cannot see is not one it can render.
const (
	// The ack timeout. Silence means an agent that predates the amendment.
	DefaultAckTimeout = 10 * time.Second
	// The overall apply deadline. Generous: a cold pull of a platform image on
	// a slow link legitimately takes minutes, and this exists so a run can
	// never wedge forever on one target. On an owned control plane it bounds
	// only a silent recovery actor: one still answering is waited for, and its
	// 300 s verify default (ActorRequest sends no wait_timeout_s) is the budget.
	DefaultApplyDeadline = 15 * time.Minute
	// How often the drain is re-counted, and how often a sent attempt is
	// re-read for a terminal state written by the relay.
	DefaultApplyPoll = 2 * time.Second
	// How long an apply waits for its host's agent to be connected before it
	// gives up. A fleet run adopted after the control plane recreated itself
	// reaches its first host while every agent is still reconnecting.
	DefaultConnectWait = 60 * time.Second
)

// applyStore is the persistence the machine needs, as an interface so it is
// testable with no database.
type applyStore interface {
	HostStatus(ctx context.Context, hostID string) (string, error)
	NonTerminalSessions(ctx context.Context, hostID string) (int, error)
	SetWaitingSessions(ctx context.Context, attemptID string, remaining int) error
	MintRequestID(ctx context.Context, attemptID string) (string, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	SucceedAttempt(ctx context.Context, attemptID string) (bool, error)
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	AttemptByRequestID(ctx context.Context, requestID string) (Attempt, error)
	// Read on the deadline path, to name the request whose verdict is stranded
	// on a host whose agent never came back: apply_timeout.go.
	AttemptRequestID(ctx context.Context, attemptID string) (string, error)
	RecordReleaseState(ctx context.Context, attemptID, state string, previous []PreviousDigest, output string) error
	SetPreviousDigests(ctx context.Context, attemptID string, previous []PreviousDigest) error
	CreateAutoRevertAttempt(ctx context.Context, in NewAutoRevert) (Attempt, error)
	OpenHostAttempt(ctx context.Context, hostID string) (Attempt, string, error)
	HostActorCommit(ctx context.Context, hostID string) (*string, error)
	OpenAttempts(ctx context.Context) ([]Attempt, error)
	TerminalStandaloneAttemptsWithOwnedHolds(ctx context.Context) ([]Attempt, error)
	Release(ctx context.Context, id string) (Release, error)
}

// ApplyCommand is one `release_apply` in this package's vocabulary; the wiring
// seam turns it into the websocket message, so nothing here imports agentws.
type ApplyCommand struct {
	RequestID  string
	Release    ReleaseRef
	Components []ComponentDigest
	Force      bool
}

// Ack is the agent's `ack{ok, error?}`. OK means ACCEPTED, never done.
type Ack struct {
	OK    bool
	Error string
}

// ApplyDeps are the machine's effects. Function fields as in Deps:
// internal/session sits above this package.
type ApplyDeps struct {
	// Owned callbacks are the RH05 path. They acquire/release this attempt's
	// restriction even when another owner already has the host draining.
	AcquireOwned func(ctx context.Context, attemptID, hostID string) error
	ReleaseOwned func(ctx context.Context, attemptID, hostID string) error
	// Cordon takes the host out of scheduling (the existing drain, force=false:
	// this never stops a session).
	Cordon func(ctx context.Context, hostID string) error
	// Uncordon lifts a cordon this apply imposed.
	Uncordon func(ctx context.Context, hostID string) error
	// Send dispatches release_apply and waits for the ack. ctx carries the ack
	// timeout, so a context.DeadlineExceeded IS the "old agent" signal.
	Send func(ctx context.Context, hostID string, cmd ApplyCommand) (Ack, error)
	// Connected reports whether this host's agent is on the wire right now.
	// Nil reads as connected, which sends and lets the send's own error decide.
	Connected func(hostID string) bool
	// DeveloperCommit reads the commit a developer apply's images carry, for an
	// attempt this process did not create (a restart re-adopted it). A
	// developer_apply row has no release to name its commit, and its digests
	// name it immutably. Nil fails such an attempt before the send.
	DeveloperCommit func(ctx context.Context, components []ComponentDigest) (string, error)
}

// Runner drives every in-flight attempt. Its state is the goroutine set and the
// unsupported-hosts set; everything durable is in Postgres, so a restart
// re-adopts rather than orphans.
type Runner struct {
	store applyStore
	deps  ApplyDeps
	log   *slog.Logger

	AckTimeout   time.Duration
	Deadline     time.Duration
	PollInterval time.Duration
	ConnectWait  time.Duration

	mu sync.Mutex
	// attempt id → cancel. Bounded by the open attempts, which the database
	// bounds at one per target.
	running map[string]context.CancelFunc
	// host id → this host's agent never acked, so it predates the amendment.
	// Cleared ONLY by a register: a register is the only evidence the build
	// changed (agent-api.md).
	unsupported map[string]bool
	// attempt id → the commit a developer apply's images carry: its success
	// evidence and its release_apply provenance.
	developerCommits map[string]developerEvidence

	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

// NewRunner builds a Runner with the contract's timings.
func NewRunner(store applyStore, deps ApplyDeps, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runner{
		store:            store,
		deps:             deps,
		log:              log,
		AckTimeout:       DefaultAckTimeout,
		Deadline:         DefaultApplyDeadline,
		PollInterval:     DefaultApplyPoll,
		ConnectWait:      DefaultConnectWait,
		running:          make(map[string]context.CancelFunc),
		unsupported:      make(map[string]bool),
		developerCommits: make(map[string]developerEvidence),
		baseCtx:          ctx,
		stop:             cancel,
	}
}

// Adopt re-attaches to every attempt left non-terminal by a restart, re-arming
// its deadline from started_at. An orphaned open attempt would block its target
// forever through the single-flight index.
func (r *Runner) Adopt(ctx context.Context) {
	if r.deps.ReleaseOwned != nil {
		stranded, err := r.store.TerminalStandaloneAttemptsWithOwnedHolds(ctx)
		if err != nil {
			r.log.Error("could not find terminal platform holds awaiting release", "err", err)
		} else {
			for _, a := range stranded {
				if a.HostID == nil {
					continue
				}
				if err := r.deps.ReleaseOwned(ctx, a.ID, *a.HostID); err != nil {
					r.log.Warn("terminal platform hold remains for next boot retry",
						"attempt_id", a.ID, "host_id", *a.HostID, "err", err)
				}
			}
		}
	}
	open, err := r.store.OpenAttempts(ctx)
	if err != nil {
		r.log.Error("could not re-adopt in-flight applies", "err", err)
		return
	}
	for _, a := range open {
		if a.Target != TargetHost || a.HostID == nil {
			continue // the control-plane target is #117's, and it adopts by polling
		}
		r.log.Warn("re-adopting a platform apply left in flight by a restart",
			"attempt_id", a.ID, "host_id", *a.HostID, "state", a.State)
		r.Start(a)
	}
}

// Start drives one host attempt. Idempotent per attempt: a second Start for an
// attempt already being driven is dropped, so Adopt and the endpoint cannot
// double-drive one row.
func (r *Runner) Start(a Attempt) {
	if a.HostID == nil {
		return
	}
	ctx, cancel := context.WithCancel(r.baseCtx)
	r.mu.Lock()
	if _, busy := r.running[a.ID]; busy {
		r.mu.Unlock()
		cancel()
		return
	}
	r.running[a.ID] = cancel
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.running, a.ID)
			r.mu.Unlock()
			cancel()
		}()
		r.drive(ctx, a)
		if a.Kind == KindDeveloperApply {
			rctx, done := context.WithTimeout(context.Background(), 5*time.Second)
			if cur, err := r.store.Attempt(rctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
				r.forgetDeveloperCommit(a.ID)
			}
			done()
		}
	}()
}

// Close cancels every in-flight attempt goroutine and waits for them. The rows
// stay non-terminal on purpose: the next boot's Adopt picks them up, which is
// the whole point of persisting the request id before sending.
func (r *Runner) Close() {
	r.stop()
	r.wg.Wait()
}

// Supported reports whether this host may be sent an apply. False means its
// agent went silent on a previous one, so it predates the amendment: the admin
// surface answers 501 apply_unsupported until it registers again.
func (r *Runner) Supported(hostID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.unsupported[hostID]
}

func (r *Runner) markUnsupported(hostID string) {
	r.mu.Lock()
	r.unsupported[hostID] = true
	r.mu.Unlock()
}

// developerEvidence is what a developer_apply attempt's success rule needs.
type developerEvidence struct {
	commit string
	// A register on `commit` proves nothing when the host already ran it: the old
	// agent a failed attempt restored registers the same commit, possibly before its
	// relayed failure (agent-api.md §release_state, "A successful node-agent apply
	// is usually never reported…"). Then only the relayed outcome decides.
	registerIsEvidence bool
}

// RememberDeveloperCommit records the commit the developer apply endpoint read
// off the attempt's images and the commit the host ran before it, so neither the
// send nor the register hook reads the registry again.
func (r *Runner) RememberDeveloperCommit(attemptID, commit string, hostCommitBefore *string) {
	r.mu.Lock()
	r.developerCommits[attemptID] = developerEvidence{
		commit:             commit,
		registerIsEvidence: hostCommitBefore != nil && !commitsMatch(*hostCommitBefore, commit),
	}
	r.mu.Unlock()
}

// developerCommit is the commit a developer_apply attempt carries, and whether a
// register on it is success evidence. An attempt re-adopted after a restart does
// not know the host's earlier commit, so for it only the relayed outcome decides.
func (r *Runner) developerCommit(ctx context.Context, a Attempt) (developerEvidence, error) {
	r.mu.Lock()
	ev, ok := r.developerCommits[a.ID]
	r.mu.Unlock()
	if ok {
		return ev, nil
	}
	if r.deps.DeveloperCommit == nil {
		return developerEvidence{}, errors.New("no registry reader is wired to read the images' commit")
	}
	commit, err := r.deps.DeveloperCommit(ctx, a.RequestedDigests)
	if err != nil {
		return developerEvidence{}, err
	}
	r.RememberDeveloperCommit(a.ID, commit, nil)
	return developerEvidence{commit: commit}, nil
}

func (r *Runner) forgetDeveloperCommit(attemptID string) {
	r.mu.Lock()
	delete(r.developerCommits, attemptID)
	r.mu.Unlock()
}

// drive is one attempt, start to terminal.
func (r *Runner) drive(ctx context.Context, a Attempt) {
	hostID := *a.HostID

	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	dctx, cancelDeadline := context.WithDeadline(ctx, started.Add(r.Deadline))
	defer cancelDeadline()

	// The cordon found is the one restored, whatever the outcome. Read first,
	// before anything can change it.
	status, err := r.store.HostStatus(dctx, hostID)
	if err != nil {
		r.log.Error("apply: could not read host status", "attempt_id", a.ID, "host_id", hostID, "err", err)
		r.fail(a.ID, ReasonUpdaterUnreachable, "")
		return
	}
	// Only `draining` is a cordon. `offline` is not one, and recording it as the
	// admin's meant this attempt never cordoned the host and then CORDONED it on
	// restore — leaving a host nobody cordoned out of scheduling (#170, the same
	// conflation as the fleet run's).
	wasCordoned := status == "draining"
	if r.deps.AcquireOwned != nil {
		ownerID := a.ID
		if a.RunID != nil {
			ownerID = *a.RunID
		}
		if err := r.deps.AcquireOwned(dctx, ownerID, hostID); err != nil {
			r.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
	} else if !wasCordoned {
		if err := r.deps.Cordon(dctx, hostID); err != nil {
			// A host that cannot be cordoned cannot be drained, and applying
			// without draining is the thing this whole path exists to avoid.
			r.log.Error("apply: could not cordon host", "attempt_id", a.ID, "host_id", hostID, "err", err)
			r.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
	}
	// A fleet run owns the scheduling state of every host it touches: it
	// records this host's cordon before the attempt exists — fleet-wide before
	// its control-plane step, per host before each host step when it had no
	// control-plane step to take one (#200) — and restores every cordon from
	// that record when it finishes. This attempt still cordons a host that is
	// serving (a disconnect/register cycle can have lifted the run's cordon),
	// but the restore is the run's: done here too, it read the run's cordon as
	// an admin's and re-applied it milliseconds after the run had lifted it
	// (#140).
	if a.RunID == nil {
		defer r.restoreCordon(a.ID, hostID, wasCordoned)
	}

	// A re-adopted attempt that was already sent skips straight to watching:
	// its request id is persisted, so the relay can still resolve it.
	if a.State == AttemptQueued || a.State == AttemptWaitingSessions {
		if !r.prepareAndSend(dctx, a, hostID) {
			return
		}
	}
	r.watch(dctx, a.ID)
}

// prepareAndSend drains, mints, persists and sends. False means the attempt is
// already resolved (failed) and there is nothing to watch.
func (r *Runner) prepareAndSend(ctx context.Context, a Attempt, hostID string) bool {
	// The N the operator agreed to lose is recorded before the apply is sent,
	// forced or not.
	remaining, err := r.store.NonTerminalSessions(ctx, hostID)
	if err != nil {
		r.log.Error("apply: could not count sessions", "attempt_id", a.ID, "host_id", hostID, "err", err)
		r.fail(a.ID, ReasonUpdaterUnreachable, "")
		return false
	}
	if err := r.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
		r.log.Warn("apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
	}
	if !a.Force {
		for remaining > 0 {
			select {
			case <-ctx.Done():
				return r.deadlineOrCancel(ctx, a.ID)
			case <-time.After(r.PollInterval):
			}
			// A cancel resolves an unsent attempt at once; without this the
			// drain would keep waiting for sessions until the deadline.
			if cur, err := r.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
				return false
			}
			remaining, err = r.store.NonTerminalSessions(ctx, hostID)
			if err != nil {
				r.log.Warn("apply: session count failed while draining", "attempt_id", a.ID, "err", err)
				continue
			}
			if err := r.store.SetWaitingSessions(ctx, a.ID, remaining); err != nil {
				r.log.Warn("apply: could not record sessions_remaining", "attempt_id", a.ID, "err", err)
			}
		}
	}

	// An agent that is not on the wire yet is not an agent that failed: after a
	// control-plane recreate every agent reconnects a beat later, and sending
	// into that gap is what made a whole fleet run fail on its first host.
	//
	// BEFORE the mint, not after: nothing is persisted while this waits, so a
	// shutdown here leaves the row `waiting_sessions` for the next boot's Adopt
	// to re-drive from the top — and the `pending`-with-no-send window that
	// apply_timeout.go can only call `reachUnknown` shrinks from a minute of
	// connect wait to the milliseconds between the mint and the send.
	if !r.waitConnected(ctx, hostID) {
		if errors.Is(ctx.Err(), context.Canceled) {
			// A shutdown, not a host that never came back. Services.Stop
			// cancels in-flight applies, it does not fail them: failing here
			// wrote a terminal `timeout` no Adopt could resume, and in the
			// unattended lane that suppressed the release for good.
			r.log.Info("apply: shutting down while waiting for the host's agent; leaving the attempt to the next boot",
				"attempt_id", a.ID, "host_id", hostID)
			return false
		}
		r.log.Warn("apply: the host's agent did not reconnect in time", "attempt_id", a.ID, "host_id", hostID)
		// Nothing was minted or sent, so no updater has a result: apply_timeout.go.
		r.fail(a.ID, ReasonTimeout, applyNotSentOutput)
		return false
	}

	// Persisted before the send, because the agent that receives the command is
	// normally destroyed by carrying it out.
	requestID, err := r.store.MintRequestID(ctx, a.ID)
	if errors.Is(err, ErrAttemptNotFound) {
		return false // resolved underneath us; nothing to send and nothing to watch
	}
	if err != nil {
		r.log.Error("apply: could not persist the request id", "attempt_id", a.ID, "err", err)
		r.fail(a.ID, ReasonUpdaterUnreachable, "")
		return false
	}

	release := ReleaseRef{SourceCommit: ""}
	if a.Kind == KindDeveloperApply {
		// No release: `id` "", `version` null, and the images' own commit
		// (agent-api.md §release_apply, amendment 14).
		ev, err := r.developerCommit(ctx, a)
		if err != nil {
			r.log.Error("apply: could not read the developer apply's commit", "attempt_id", a.ID, "err", err)
			r.fail(a.ID, ReasonInvalid, "the images' build identity could not be read: "+err.Error())
			return false
		}
		release = ReleaseRef{SourceCommit: ev.commit}
	}
	if a.ReleaseID != nil {
		rel, err := r.store.Release(ctx, *a.ReleaseID)
		if err != nil {
			r.log.Error("apply: could not read the release", "attempt_id", a.ID, "err", err)
			r.fail(a.ID, ReasonInvalid, "")
			return false
		}
		release = ReleaseRef{ID: rel.ID, Version: rel.Version, SourceCommit: rel.SourceCommit}
	}

	ackCtx, cancel := context.WithTimeout(ctx, r.AckTimeout)
	ack, err := r.deps.Send(ackCtx, hostID, ApplyCommand{
		RequestID:  requestID,
		Release:    release,
		Components: a.RequestedDigests,
		Force:      a.Force,
	})
	cancel()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// An unknown downstream type is wire-silent, so silence is the only
		// signal that this agent predates the amendment. Never retried.
		r.markUnsupported(hostID)
		r.log.Warn("apply: no ack within the ack timeout; treating this agent as predating the amendment",
			"attempt_id", a.ID, "host_id", hostID)
		r.fail(a.ID, ReasonUnsupported, "")
		return false
	case err != nil:
		// Never updater_unreachable: that is the AGENT's word for its own
		// socket, and this is the control plane failing to reach the agent.
		r.log.Error("apply: could not deliver release_apply", "attempt_id", a.ID, "host_id", hostID, "err", err)
		r.fail(a.ID, ReasonTimeout, err.Error())
		return false
	case !ack.OK:
		reason := ack.Error
		if reason == "" {
			reason = ReasonInvalid
		}
		if !KnownFailureReason(reason) {
			// Stored verbatim: a future agent's new identifier must not read
			// as a success.
			r.log.Warn("apply: agent rejected with an unrecognised reason",
				"attempt_id", a.ID, "host_id", hostID, "reason", reason)
		}
		r.fail(a.ID, reason, "")
		return false
	}
	r.log.Info("apply: accepted by the agent", "attempt_id", a.ID, "host_id", hostID,
		"request_id", requestID, "force", a.Force)
	return true
}

// waitConnected blocks until the host's agent is on the wire, the connect wait
// runs out, the apply deadline passes, or the process shuts down. False is all
// three of those, so the caller must read ctx.Err() to tell them apart: a
// cancel is a shutdown and must leave the attempt alone, while a nil or expired
// ctx.Err() is a host that never came back and fails the attempt.
func (r *Runner) waitConnected(ctx context.Context, hostID string) bool {
	if r.deps.Connected == nil {
		return true
	}
	deadline := time.Now().Add(r.ConnectWait)
	for {
		if r.deps.Connected(hostID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			// A shutdown or the apply deadline; the caller's own handling is
			// what decides, so this is not a connection failure.
			return false
		case <-time.After(r.PollInterval):
		}
	}
}

// watch waits for the relay or the register hook to resolve the attempt, and
// writes the deadline's verdict if neither does.
func (r *Runner) watch(ctx context.Context, attemptID string) {
	for {
		select {
		case <-ctx.Done():
			r.deadlineOrCancel(ctx, attemptID)
			return
		case <-time.After(r.PollInterval):
		}
		a, err := r.store.Attempt(ctx, attemptID)
		if err != nil {
			if ctx.Err() != nil {
				r.deadlineOrCancel(ctx, attemptID)
				return
			}
			r.log.Warn("apply: could not re-read the attempt", "attempt_id", attemptID, "err", err)
			continue
		}
		if TerminalAttemptState(a.State) {
			return
		}
	}
}

// deadlineOrCancel separates "ran out of time" (a failure) from "shutting
// down" (Adopt resumes it). Always false, so a caller can return it.
func (r *Runner) deadlineOrCancel(ctx context.Context, attemptID string) bool {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		r.log.Warn("apply: deadline expired with no terminal state", "attempt_id", attemptID)
		// An expiry with no agent on the wire is a verdict stranded on the
		// host, not a mystery: apply_timeout.go.
		r.fail(attemptID, ReasonTimeout, r.timeoutOutput(attemptID))
	}
	return false
}

// fail writes a terminal failure on a context of its own: the attempt's context
// is usually the thing that just expired.
func (r *Runner) fail(attemptID, reason, output string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.store.FailAttempt(ctx, attemptID, reason, output); err != nil {
		r.log.Error("apply: could not record the failure", "attempt_id", attemptID, "reason", reason, "err", err)
	}
}

// restoreCordon puts the host's scheduling status back to what this apply
// found. Runs on every terminal path, including a failed one: a host left
// draining by a failed apply would silently drop out of scheduling.
func (r *Runner) restoreCordon(attemptID, hostID string, wasCordoned bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if r.deps.ReleaseOwned != nil {
		// A control-plane restart cancels this goroutine while the durable
		// attempt stays open. Keep its admission protection for Adopt.
		attempt, err := r.store.Attempt(ctx, attemptID)
		if err != nil || !TerminalAttemptState(attempt.State) {
			r.log.Info("apply: retaining admission restriction for open attempt", "attempt_id", attemptID, "host_id", hostID, "err", err)
			return
		}
		if err := r.deps.ReleaseOwned(ctx, attemptID, hostID); err != nil {
			r.log.Warn("apply: could not release own admission restriction", "attempt_id", attemptID, "host_id", hostID, "err", err)
		}
		return
	}
	if wasCordoned {
		// An admin's cordon is restored, not lifted. The new agent's register
		// keeps a draining row draining (#140), so this is usually a no-op —
		// it is real when the agent's disconnect was observed and flipped the
		// row offline before it came back online.
		if err := r.deps.Cordon(ctx, hostID); err != nil {
			r.log.Warn("apply: could not restore the admin cordon", "host_id", hostID, "err", err)
		}
		return
	}
	if err := r.deps.Uncordon(ctx, hostID); err != nil {
		// An offline host cannot be uncordoned; it returns online on its
		// agent's reconnect, which is the normal end of a successful apply.
		r.log.Info("apply: host not uncordoned (it will return online on its agent's reconnect)",
			"host_id", hostID, "err", err)
	}
}

// HandleReleaseState relays one `release_state` onto its attempt. Fire and
// forget: an unknown request id is DROPPED, not stored, and a late message for
// a resolved attempt is a no-op rather than a conflict.
func (r *Runner) HandleReleaseState(ctx context.Context, hostID string, rep ReleaseStateReport) {
	a, err := r.store.AttemptByRequestID(ctx, rep.RequestID)
	if errors.Is(err, ErrAttemptNotFound) {
		r.log.Debug("release_state for an unknown request id; dropped",
			"host_id", hostID, "request_id", rep.RequestID)
		return
	}
	if err != nil {
		r.log.Warn("release_state: could not resolve the attempt", "host_id", hostID, "err", err)
		return
	}
	// Trust boundary: a host may only speak about its own attempt.
	if a.HostID == nil || *a.HostID != hostID {
		r.log.Warn("release_state names another host's attempt; dropped",
			"host_id", hostID, "attempt_id", a.ID)
		return
	}
	if TerminalAttemptState(a.State) {
		r.log.Debug("release_state for a resolved attempt; no-op",
			"attempt_id", a.ID, "state", rep.State)
		return
	}
	if !wireAttemptStates[rep.State] {
		r.log.Warn("release_state carries a state no agent may report; dropped",
			"attempt_id", a.ID, "state", rep.State)
		return
	}

	// The previous digests are recorded from EVERY message, in every state:
	// they are what restores a half-failed stack, and something has to have
	// written them down before the restore is needed.
	if len(rep.Previous) > 0 {
		if err := r.store.SetPreviousDigests(ctx, a.ID, rep.Previous); err != nil {
			r.log.Warn("release_state: could not record previous digests", "attempt_id", a.ID, "err", err)
		}
	}

	switch rep.State {
	case AttemptSucceeded:
		// Corroboration, not the gate — but an apply that somehow reported its
		// own success is still resolved by it.
		if done, err := r.store.SucceedAttempt(ctx, a.ID); err != nil {
			r.log.Warn("release_state: could not record success", "attempt_id", a.ID, "err", err)
		} else if done {
			r.log.Info("apply succeeded (reported by the agent)", "attempt_id", a.ID, "host_id", hostID)
		}
	case AttemptFailed:
		reason := ReasonInvalid
		if rep.Reason != nil && *rep.Reason != "" {
			reason = *rep.Reason
		} else {
			r.log.Warn("release_state failed with no reason; recorded as invalid", "attempt_id", a.ID)
		}
		if err := r.store.FailAttempt(ctx, a.ID, reason, rep.Output); err != nil {
			r.log.Warn("release_state: could not record failure", "attempt_id", a.ID, "err", err)
		} else {
			r.log.Warn("apply failed", "attempt_id", a.ID, "host_id", hostID, "reason", reason)
			if rep.Restored {
				r.recordAutoRevert(ctx, a, rep)
			}
		}
	default:
		if err := r.store.RecordReleaseState(ctx, a.ID, rep.State, rep.Previous, rep.Output); err != nil {
			r.log.Warn("release_state: could not record progress", "attempt_id", a.ID, "err", err)
		}
	}
}

// recordAutoRevert writes the history row for a restore the updater did itself
// (ADR 0004). Only after the apply is terminal, so the open-target index is
// free; only for an apply, never for a revert that failed.
func (r *Runner) recordAutoRevert(ctx context.Context, failed Attempt, rep ReleaseStateReport) {
	if failed.Kind != KindApply && failed.Kind != KindDeveloperApply {
		return
	}
	restored := failed.RequestedDigests
	if len(restored) > 1 && failed.HostID != nil {
		// The restored agent registered before relaying this, so the host's
		// recorded actor is the one serving now.
		// Neither commit known: which component was restored cannot be told, and a
		// guessed row would offer the wrong revert.
		want := r.attemptCommit(ctx, failed)
		actor, err := r.store.HostActorCommit(ctx, *failed.HostID)
		if want == "" || err != nil || actor == nil {
			r.log.Warn("release_state says restored, but which component was put back cannot be told; no auto_revert recorded",
				"attempt_id", failed.ID, "host_id", *failed.HostID, "token", "apply-auto-revert-component-unknown")
			return
		}
		restored = restoredComponents(restored, commitsMatch(want, *actor))
	}
	requested := restoredDigests(restored, rep.Previous)
	if len(requested) == 0 {
		r.log.Warn("release_state says restored but named no previous digest; no auto_revert recorded",
			"attempt_id", failed.ID, "host_id", orEmpty(failed.HostID), "token", "apply-auto-revert-unrecorded")
		return
	}
	previous := make([]PreviousDigest, 0, len(restored))
	for _, c := range restored {
		d := c.Digest
		previous = append(previous, PreviousDigest{Name: c.Name, Digest: &d})
	}
	row, err := r.store.CreateAutoRevertAttempt(ctx, NewAutoRevert{
		Failed: failed, Requested: requested, Previous: previous,
		Output: "restored by the recovery actor after the apply failed (" + orEmpty(rep.Reason) + ")",
	})
	if err != nil {
		r.log.Error("could not record the updater's automatic restore", "attempt_id", failed.ID,
			"err", err, "token", "apply-auto-revert-unrecorded")
		return
	}
	r.log.Warn("apply automatically reverted by the updater", "attempt_id", failed.ID,
		"auto_revert_id", row.ID, "host_id", orEmpty(failed.HostID), "token", "apply-auto-reverted")
}

// attemptCommit is the commit an attempt moves its host to: its release's, or a
// developer apply's images'. Empty when neither can be named.
func (r *Runner) attemptCommit(ctx context.Context, a Attempt) string {
	if a.ReleaseID != nil {
		if rel, err := r.store.Release(ctx, *a.ReleaseID); err == nil {
			return rel.SourceCommit
		}
	}
	if a.Kind == KindDeveloperApply {
		if ev, err := r.developerCommit(ctx, a); err == nil {
			return ev.commit
		}
	}
	return ""
}

// names reports whether an attempt's components include `name`.
func names(components []ComponentDigest, name string) bool {
	for _, c := range components {
		if c.Name == name {
			return true
		}
	}
	return false
}

// namesOnly reports whether `name` is the attempt's only component.
func namesOnly(components []ComponentDigest, name string) bool {
	return len(components) == 1 && components[0].Name == name
}

// restoredComponents is the one component a failed, restored attempt put back
// (ADR 0004 amendment, "one service per failure"): components are replaced in
// order and the first failure stops the sequence, so it is the first one not on
// the requested commit. `actorOnWant` is whether the host's recovery actor reports
// the attempt's commit now; the wire names no per-component outcome.
func restoredComponents(requested []ComponentDigest, actorOnWant bool) []ComponentDigest {
	if len(requested) > 1 && requested[0].Name == ComponentRecovery && actorOnWant {
		return requested[1:2]
	}
	if len(requested) > 1 {
		return requested[:1]
	}
	return requested
}

// restoredDigests pairs the failed apply's components with the previous digests
// the updater reported it went back to; a component whose previous digest was
// unknown is left out.
func restoredDigests(requested []ComponentDigest, previous []PreviousDigest) []ComponentDigest {
	out := make([]ComponentDigest, 0, len(requested))
	for _, c := range requested {
		for _, p := range previous {
			if p.Name == c.Name && p.Digest != nil && *p.Digest != "" {
				out = append(out, ComponentDigest{Name: c.Name, Image: c.Image, Digest: *p.Digest})
			}
		}
	}
	return out
}

// HandleRegister is the success-evidence hook, on every agent register. A
// register is the only thing that clears the apply-unsupported flag, and one
// carrying the requested source_commit resolves that host's open attempt.
func (r *Runner) HandleRegister(ctx context.Context, hostID string, sourceCommit *string) {
	r.mu.Lock()
	delete(r.unsupported, hostID)
	r.mu.Unlock()

	a, wantCommit, err := r.store.OpenHostAttempt(ctx, hostID)
	if errors.Is(err, ErrAttemptNotFound) {
		return
	}
	if err != nil {
		r.log.Warn("register: could not check for an in-flight apply", "host_id", hostID, "err", err)
		return
	}
	if sourceCommit == nil || *sourceCommit == "" {
		return
	}
	// Amendment 14: a request naming only the recovery actor never replaced the
	// agent, so its register proves nothing; the relayed terminal state decides.
	if namesOnly(a.RequestedDigests, ComponentRecovery) {
		return
	}
	if wantCommit == "" && a.Kind == KindDeveloperApply {
		ev, err := r.developerCommit(ctx, a)
		if err != nil {
			// The actor's relayed outcome or the deadline decides.
			r.log.Warn("register: could not read the developer apply's commit", "attempt_id", a.ID, "err", err)
			return
		}
		if !ev.registerIsEvidence {
			r.log.Info("register during a developer apply of the commit the host already ran; the relayed outcome decides",
				"host_id", hostID, "attempt_id", a.ID)
			return
		}
		wantCommit = ev.commit
	}
	if wantCommit == "" {
		// A revert to a build this instance can no longer name has no commit
		// to match; its evidence rule is in apply_revert.go. One that also moves
		// the recovery actor has no commit to check the actor against either, so
		// the relayed outcome decides.
		if names(a.RequestedDigests, ComponentRecovery) {
			return
		}
		r.revertRegisterEvidence(ctx, a, *sourceCommit)
		return
	}
	if !commitsMatch(wantCommit, *sourceCommit) {
		// Not a failure: an agent may reconnect mid-apply on the old build,
		// and the deadline decides.
		if a.State == AttemptRecreating || a.State == AttemptVerifying {
			r.log.Info("register during an apply reports a different commit; leaving the deadline to decide",
				"host_id", hostID, "attempt_id", a.ID, "reported", *sourceCommit, "wanted", wantCommit)
		}
		return
	}
	// ...and one that named the recovery actor too succeeds only once the actor
	// serving the host reports the same commit (amendment 14).
	if names(a.RequestedDigests, ComponentRecovery) {
		actor, err := r.store.HostActorCommit(ctx, hostID)
		if err != nil {
			r.log.Warn("register: could not read the host's recovery actor commit", "host_id", hostID, "attempt_id", a.ID, "err", err)
			return
		}
		if actor == nil || !commitsMatch(wantCommit, *actor) {
			r.log.Info("register reports the requested agent but not the requested recovery actor; the relayed outcome decides",
				"host_id", hostID, "attempt_id", a.ID)
			return
		}
	}
	done, err := r.store.SucceedAttempt(ctx, a.ID)
	if err != nil {
		r.log.Warn("register: could not resolve the apply", "host_id", hostID, "attempt_id", a.ID, "err", err)
		return
	}
	if done {
		r.log.Info("apply succeeded: the host registered on the requested release",
			"host_id", hostID, "attempt_id", a.ID, "source_commit", *sourceCommit)
	}
}
