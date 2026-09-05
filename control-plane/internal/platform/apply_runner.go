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
	// never wedge forever on one target.
	DefaultApplyDeadline = 15 * time.Minute
	// How often the drain is re-counted, and how often a sent attempt is
	// re-read for a terminal state written by the relay.
	DefaultApplyPoll = 2 * time.Second
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
	RecordReleaseState(ctx context.Context, attemptID, state string, previous []PreviousDigest, output string) error
	SetPreviousDigests(ctx context.Context, attemptID string, previous []PreviousDigest) error
	OpenHostAttempt(ctx context.Context, hostID string) (Attempt, string, error)
	OpenAttempts(ctx context.Context) ([]Attempt, error)
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
	// Cordon takes the host out of scheduling (the existing drain, force=false:
	// this never stops a session).
	Cordon func(ctx context.Context, hostID string) error
	// Uncordon lifts a cordon this apply imposed.
	Uncordon func(ctx context.Context, hostID string) error
	// Send dispatches release_apply and waits for the ack. ctx carries the ack
	// timeout, so a context.DeadlineExceeded IS the "old agent" signal.
	Send func(ctx context.Context, hostID string, cmd ApplyCommand) (Ack, error)
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

	mu sync.Mutex
	// attempt id → cancel. Bounded by the open attempts, which the database
	// bounds at one per target.
	running map[string]context.CancelFunc
	// host id → this host's agent never acked, so it predates the amendment.
	// Cleared ONLY by a register: a register is the only evidence the build
	// changed (agent-api.md).
	unsupported map[string]bool

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
		store:        store,
		deps:         deps,
		log:          log,
		AckTimeout:   DefaultAckTimeout,
		Deadline:     DefaultApplyDeadline,
		PollInterval: DefaultApplyPoll,
		running:      make(map[string]context.CancelFunc),
		unsupported:  make(map[string]bool),
		baseCtx:      ctx,
		stop:         cancel,
	}
}

// Adopt re-attaches to every attempt left non-terminal by a restart, re-arming
// its deadline from started_at. An orphaned open attempt would block its target
// forever through the single-flight index.
func (r *Runner) Adopt(ctx context.Context) {
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
	wasCordoned := status != "online"
	if !wasCordoned {
		if err := r.deps.Cordon(dctx, hostID); err != nil {
			// A host that cannot be cordoned cannot be drained, and applying
			// without draining is the thing this whole path exists to avoid.
			r.log.Error("apply: could not cordon host", "attempt_id", a.ID, "host_id", hostID, "err", err)
			r.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
	}
	defer r.restoreCordon(hostID, wasCordoned)

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
		r.log.Error("apply: could not deliver release_apply", "attempt_id", a.ID, "host_id", hostID, "err", err)
		r.fail(a.ID, ReasonUpdaterUnreachable, "")
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
		r.fail(attemptID, ReasonTimeout, "")
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
func (r *Runner) restoreCordon(hostID string, wasCordoned bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if wasCordoned {
		// An admin's cordon is restored, not lifted — and the new agent's
		// register flips the row back to online, so this is a real re-cordon
		// rather than a no-op.
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
		}
	default:
		if err := r.store.RecordReleaseState(ctx, a.ID, rep.State, rep.Previous, rep.Output); err != nil {
			r.log.Warn("release_state: could not record progress", "attempt_id", a.ID, "err", err)
		}
	}
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
	if wantCommit == "" {
		// A revert to a build this instance can no longer name has no commit
		// to match; its evidence rule is in apply_revert.go.
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
