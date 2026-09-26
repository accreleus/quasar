package platform

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The control plane applying ITSELF, through the recovery actor on its own
// machine over the control socket, never over an agent connection (agent-api.md
// §release_apply: "the control plane NEVER asks an agent to update the control
// plane"). A control plane with no recovery actor has nothing that can replace
// it: its target reads `updater_absent`.
//
// This process cannot report its own success, because carrying the apply out
// destroys it. So the request id is persisted BEFORE the socket call, and the
// actor's terminal result, read again by the next boot, is the verdict
// (control-api.md §"The shape of an apply, once"; ADR 0004 amendment).

// SelfRequest is one control-plane apply as the self-applier hands it to the
// recovery actor (actor_client.go). Components are in replacement order.
type SelfRequest struct {
	RequestID  string
	Components []ComponentDigest
	Release    ReleaseRef
	// Migrates is the release's schema above this binary's; SchemaVersion is the
	// schema it moves to, 0 when unknown.
	Migrates                bool
	SchemaVersion           int
	ExternalBackupConfirmed bool
	// FromVersion is this control plane's own version: what a migrating
	// failure's restore command returns to.
	FromVersion string
}

// SelfAccepted is the actor's 202 for a control-plane step.
type SelfAccepted struct {
	RequestID string
	Previous  []PreviousDigest
}

// SelfResult is one control-plane attempt as the actor reports it, in
// `release_state`'s spellings.
type SelfResult struct {
	RequestID  string
	State      string
	Reason     *string
	Components []ComponentDigest
	Previous   []PreviousDigest
	Output     string
	StartedAt  string
	UpdatedAt  string
	FinishedAt *string
	// Restored: the actor put the previous control plane back.
	Restored bool
	Release  ReleaseRef
	// PreUpdateDump is the dump a migrating attempt took, when it took one.
	PreUpdateDump *string
}

// UpdaterAPI is the sliver of the control socket this package uses, as an
// interface so the self-apply is testable over a temp socket or a fake. The
// contract keeps "updater" in `updater_absent` / `updater_present`; it means the
// recovery actor.
type UpdaterAPI interface {
	// Present reports whether the actor's socket is there. False makes the
	// control-plane target ineligible (`updater_absent`) rather than an apply
	// that fails halfway.
	Present() bool
	// SocketState is the #184 diagnosis behind Present.
	SocketState() SocketState
	Apply(ctx context.Context, req SelfRequest) (SelfAccepted, error)
	Result(ctx context.Context, requestID string) (SelfResult, error)
	// SocketPath names the socket in an operator-facing failure.
	SocketPath() string
}

// actorRefusal carries the socket's rejection identifier, which is already the
// closed `release_state.reason` vocabulary.
type actorRefusal struct {
	Reason  string
	Message string
}

func (e *actorRefusal) Error() string {
	if e.Message == "" {
		return e.Reason
	}
	return e.Reason + ": " + e.Message
}

// ErrNoResult is a request id the actor has no result for yet: normal for the
// first seconds of an apply, and on boot before the actor has answered.
var ErrNoResult = errors.New("no result for this request id")

// selfStore is the persistence the self-apply needs.
type selfStore interface {
	MintRequestID(ctx context.Context, attemptID string) (string, error)
	FailAttempt(ctx context.Context, attemptID, reason, output string) error
	SucceedAttempt(ctx context.Context, attemptID string) (bool, error)
	Attempt(ctx context.Context, attemptID string) (Attempt, error)
	AttemptRequestID(ctx context.Context, attemptID string) (string, error)
	RecordReleaseState(ctx context.Context, attemptID, state string, previous []PreviousDigest, output string) error
	SetPreviousDigests(ctx context.Context, attemptID string, previous []PreviousDigest) error
	Release(ctx context.Context, id string) (Release, error)
}

// SelfApplier drives the control-plane target of a fleet run.
type SelfApplier struct {
	store   selfStore
	updater UpdaterAPI
	log     logger

	// Identity is this binary's own build stamps; a field so a test can supply
	// the "booted on the new release" case a test binary cannot have.
	Identity     func() buildinfo.Identity
	Deadline     time.Duration
	PollInterval time.Duration
	// DeveloperCommit reads the commit a developer apply's images carry: its
	// release_apply provenance and its success evidence, since the row names no
	// release. Nil fails such an attempt before the send.
	DeveloperCommit func(ctx context.Context, components []ComponentDigest) (string, error)
	// VerdictSilence is how long, from the apply deadline on, the actor may give
	// no answer for a request before the attempt times out.
	VerdictSilence time.Duration

	// DeveloperSchema reads the schema a developer apply's control-plane image
	// declares, which decides whether it migrates. Nil sends it as not
	// migrating, and the recovery actor still reads the image's own label.
	DeveloperSchema func(ctx context.Context, components []ComponentDigest) (int, error)

	// confirmed holds the attempts whose operator confirmed a current backup of
	// an external database. In memory only: control-api.md stores it on no row,
	// so a control plane restarted before the send fails the step
	// backup_unconfirmed and the operator applies again.
	confirmed sync.Map
	// schemas holds a developer apply's control-plane schema as its admission
	// read it (NoteDeveloperSchema), so the send reads no registry after the
	// drain. In memory only: a restarted control plane reads the image again.
	schemas sync.Map
}

// NewSelfApplier builds the control-plane applier with the contract's timings.
// A nil executor is a control plane with no recovery actor.
func NewSelfApplier(store selfStore, up UpdaterAPI, log logger) *SelfApplier {
	return &SelfApplier{
		store:          store,
		updater:        up,
		log:            log,
		Identity:       buildinfo.Get,
		Deadline:       DefaultApplyDeadline,
		PollInterval:   DefaultApplyPoll,
		VerdictSilence: DefaultVerdictSilence,
	}
}

// DefaultVerdictSilence outlasts a hand-over's control-socket gap (the successor
// re-binds it) with room to spare.
const DefaultVerdictSilence = 2 * time.Minute

// UpdaterPresent reports whether this control plane could apply itself at all.
func (s *SelfApplier) UpdaterPresent() bool {
	return s.updater != nil && s.updater.Present()
}

// Apply drives one control-plane attempt to terminal or, in the normal case,
// until the actor replaces this container and the process dies mid-poll. The
// next boot's Adopt resolves the row then.
func (s *SelfApplier) Apply(ctx context.Context, a Attempt) {
	if !s.UpdaterPresent() {
		// Refused rather than attempted: an apply with nothing to carry it out
		// is a failure with a name, not a timeout fifteen minutes later.
		s.fail(a.ID, ReasonUpdaterAbsentFailure, s.absentOutput())
		return
	}

	started := a.CreatedAt
	if a.StartedAt != nil {
		started = *a.StartedAt
	}
	dctx, cancel := context.WithDeadline(ctx, started.Add(s.Deadline))
	defer cancel()

	if a.State == AttemptQueued || a.State == AttemptWaitingSessions {
		// Persisted before the call, because the process that made it is
		// normally destroyed by carrying it out.
		requestID, err := s.store.MintRequestID(dctx, a.ID)
		if errors.Is(err, ErrAttemptNotFound) {
			return // resolved underneath us: a cancel, or a boot-adopted terminal state
		}
		if err != nil && ctx.Err() != nil {
			return // shutting down, not a failure of the apply
		}
		if err != nil {
			s.log.Error("self-apply: could not persist the request id", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonUpdaterUnreachable, "")
			return
		}
		if !s.send(dctx, a, requestID) {
			return
		}
		s.poll(ctx, a, requestID)
		return
	}
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if ctx.Err() != nil {
		return // shutting down: the next boot's Adopt resolves the row
	}
	if err != nil || requestID == "" {
		s.log.Error("self-apply: a sent attempt carries no request id", "attempt_id", a.ID, "err", err)
		s.fail(a.ID, ReasonUpdaterUnreachable, "")
		return
	}
	s.poll(ctx, a, requestID)
}

func (s *SelfApplier) send(ctx context.Context, a Attempt, requestID string) bool {
	id := s.Identity()
	req := SelfRequest{RequestID: requestID, Components: a.RequestedDigests, FromVersion: id.Version}
	_, req.ExternalBackupConfirmed = s.confirmed.Load(a.ID)
	if a.ReleaseID != nil {
		rel, err := s.store.Release(ctx, *a.ReleaseID)
		if err != nil {
			s.log.Error("self-apply: could not read the release", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonInvalid, "")
			return false
		}
		req.Release = ReleaseRef{ID: rel.ID, Version: rel.Version, SourceCommit: rel.SourceCommit}
		req.SchemaVersion = rel.SchemaVersion
		req.Migrates = rel.SchemaVersion > id.SchemaVersion
	} else {
		// A developer apply: its provenance is the commit its images carry
		// (control-api.md §"Developer apply").
		commit, err := s.developerCommit(ctx, a)
		if err != nil {
			s.log.Error("self-apply: could not read the developer apply's commit", "attempt_id", a.ID, "err", err)
			s.fail(a.ID, ReasonInvalid, "the images' build identity could not be read: "+err.Error())
			return false
		}
		req.Release = ReleaseRef{SourceCommit: commit}
		if noted, ok := s.schemas.Load(a.ID); ok {
			req.SchemaVersion = noted.(int)
			req.Migrates = req.SchemaVersion > id.SchemaVersion
		} else if s.DeveloperSchema != nil {
			schema, err := s.DeveloperSchema(ctx, a.RequestedDigests)
			if err != nil {
				s.log.Error("self-apply: could not read the developer apply's schema", "attempt_id", a.ID, "err", err)
				s.fail(a.ID, ReasonInvalid, "the control-plane image's schema could not be read: "+err.Error())
				return false
			}
			req.SchemaVersion = schema
			req.Migrates = schema > id.SchemaVersion
		}
	}
	accepted, err := s.updater.Apply(ctx, req)
	if err != nil {
		var rej *actorRefusal
		if errors.As(err, &rej) {
			// The socket's identifiers are already the closed reason
			// vocabulary; an unrecognised one is stored verbatim.
			s.log.Warn("self-apply: the recovery actor refused", "attempt_id", a.ID, "reason", rej.Reason)
			s.fail(a.ID, rej.Reason, rej.Message)
			return false
		}
		s.log.Error("self-apply: could not reach the recovery actor", "attempt_id", a.ID, "err", err)
		s.fail(a.ID, ReasonUpdaterUnreachable, err.Error())
		return false
	}
	// The 202 already knows what this control plane was on; recording it now
	// means a failure's manual restore is copy-paste even if nothing else is
	// ever reported.
	if len(accepted.Previous) > 0 {
		if err := s.store.SetPreviousDigests(ctx, a.ID, accepted.Previous); err != nil {
			s.log.Warn("self-apply: could not record previous digests", "attempt_id", a.ID, "err", err)
		}
	}
	s.log.Info("self-apply: accepted by the recovery actor", "attempt_id", a.ID, "request_id", requestID)
	return true
}

// poll relays the actor's result onto the attempt until it is terminal. It
// normally does not return: the replacement stops this process partway through,
// and Adopt finishes the row on the next boot. A cancelled ctx is this process
// shutting down, which leaves the row open for the next boot.
func (s *SelfApplier) poll(ctx context.Context, a Attempt, requestID string) {
	if s.updater == nil {
		s.fail(a.ID, ReasonUpdaterAbsentFailure, s.absentOutput())
		return
	}
	if s.followVerdict(ctx, a, requestID) == verdictTimeout {
		s.fail(a.ID, ReasonTimeout, "the recovery actor did not answer for this request within the apply deadline")
	}
}

// verdict is how following the actor's result ended.
type verdict int

const (
	// verdictResolved: the attempt is terminal.
	verdictResolved verdict = iota
	// verdictShutdown: this process is stopping (the actor stops the control
	// plane it replaces or restores). Nothing was written, so the row stays
	// open and the next boot's Adopt records the actor's real verdict.
	verdictShutdown
	// verdictTimeout: from the apply deadline on, the actor gave no answer for
	// this request for VerdictSilence. Fail closed, never success.
	verdictTimeout
)

func attemptStart(a Attempt) time.Time {
	if a.StartedAt != nil {
		return *a.StartedAt
	}
	return a.CreatedAt
}

// followVerdict relays the recovery actor's result for requestID until it is
// terminal. While the actor answers a non-terminal state it is waited for past
// the apply deadline: its own verify and restore timeouts bound the attempt, and
// a control plane on a slow link may boot after the deadline and still be put
// back. Only an actor that has not answered for this request, from the deadline
// on, for VerdictSilence times the attempt out.
func (s *SelfApplier) followVerdict(ctx context.Context, a Attempt, requestID string) verdict {
	deadline := attemptStart(a).Add(s.Deadline)
	var answered time.Time
	for {
		if cur, err := s.store.Attempt(ctx, a.ID); err == nil && TerminalAttemptState(cur.State) {
			return verdictResolved
		}
		if res, err := s.updater.Result(ctx, requestID); err == nil {
			if s.record(ctx, a.ID, res) {
				return verdictResolved
			}
			if res.State != AttemptSucceeded && res.State != AttemptFailed {
				answered = time.Now()
			}
		}
		if ctx.Err() != nil {
			return verdictShutdown
		}
		if now := time.Now(); !now.Before(deadline) && now.Sub(answered) >= s.VerdictSilence {
			s.log.Warn("self-apply: the recovery actor gave no verdict by the apply deadline",
				"attempt_id", a.ID, "request_id", requestID)
			return verdictTimeout
		}
		select {
		case <-ctx.Done():
			return verdictShutdown
		case <-time.After(s.PollInterval):
		}
	}
}

// record writes one result onto the attempt and reports whether it resolved it.
func (s *SelfApplier) record(ctx context.Context, attemptID string, res SelfResult) bool {
	// Before the terminal write: a restore command in the output names this dump.
	if res.PreUpdateDump != nil {
		if d, ok := s.store.(dumpRecorder); ok {
			if err := d.SetPreUpdateDump(ctx, attemptID, *res.PreUpdateDump); err != nil {
				s.log.Warn("self-apply: could not record the pre-update dump", "attempt_id", attemptID, "err", err)
			}
		}
	}
	switch res.State {
	case AttemptSucceeded:
		if _, err := s.store.SucceedAttempt(ctx, attemptID); err != nil {
			s.log.Warn("self-apply: could not record success", "attempt_id", attemptID, "err", err)
			return false
		}
		return true
	case AttemptFailed:
		reason := ReasonInvalid
		if res.Reason != nil && *res.Reason != "" {
			reason = *res.Reason
		}
		if len(res.Previous) > 0 {
			_ = s.store.SetPreviousDigests(ctx, attemptID, res.Previous)
		}
		if err := s.store.FailAttempt(ctx, attemptID, reason, res.Output); err != nil {
			s.log.Warn("self-apply: could not record failure", "attempt_id", attemptID, "err", err)
			return false
		}
		s.log.Warn("control-plane apply failed", "attempt_id", attemptID, "reason", reason, "restored", res.Restored)
		return true
	default:
		if !wireAttemptStates[res.State] {
			return false
		}
		var prev []PreviousDigest
		if len(res.Previous) > 0 {
			prev = res.Previous
		}
		if err := s.store.RecordReleaseState(ctx, attemptID, res.State, prev, res.Output); err != nil {
			s.log.Warn("self-apply: could not record progress", "attempt_id", attemptID, "err", err)
		}
		return false
	}
}

// Adopt resolves a control-plane attempt left non-terminal by the restart it
// caused. The recovery actor's terminal result decides it, even when this
// binary is serving on the release's commit: the actor may still put the old
// control plane back, stopping this one first.
//
// Returns false when the attempt is still open: it was never sent, and the
// caller re-drives it. A cancelled ctx (this process shutting down) returns
// true with nothing written; the caller must not re-drive then either.
//
// A restart caused by an operator's `quasar-recovery reconfigure` has no row to
// adopt, and the control socket never answers with that attempt, so no row
// takes it for its verdict (guarded by reconfigure_boot_test.go).
func (s *SelfApplier) Adopt(ctx context.Context, a Attempt, wantCommit string) bool {
	requestID, err := s.store.AttemptRequestID(ctx, a.ID)
	if ctx.Err() != nil {
		return true // shutting down: nothing is decided, the next boot adopts
	}
	if err != nil {
		// Fail closed: without a request id there is no verdict to wait for, so
		// nothing is recorded; the caller re-drives, which fails the row with a
		// name rather than calling an unverified build a success.
		s.log.Warn("self-apply: an attempt's request id is unreadable; not deciding it", "attempt_id", a.ID, "err", err)
		return false
	}
	if requestID == "" {
		return false // never sent; the run re-drives it
	}
	if s.updater == nil {
		s.fail(a.ID, ReasonUpdaterAbsentFailure, s.absentOutput())
		return true
	}
	id := s.Identity()
	if wantCommit != "" && id.SourceCommit != nil && commitsMatch(*id.SourceCommit, wantCommit) {
		if s.followVerdict(ctx, a, requestID) == verdictTimeout {
			s.fail(a.ID, ReasonTimeout, "the recovery actor gave no verdict within the apply deadline; this build is serving but was never verified")
		}
		return true
	}
	s.log.Warn("re-adopting a control-plane apply left in flight by a restart",
		"attempt_id", a.ID, "state", a.State)
	s.poll(ctx, a, requestID)
	return true
}

// dumpRecorder is a selfStore that keeps an attempt's pre_update_dump (*Store).
type dumpRecorder interface {
	SetPreUpdateDump(ctx context.Context, attemptID, dump string) error
}

// ConfirmExternalBackup carries the operator's confirmation of a current backup
// of an external database to this attempt's request (control-api.md
// external_backup_confirmed). Call it before Apply.
func (s *SelfApplier) ConfirmExternalBackup(attemptID string) {
	s.confirmed.Store(attemptID, true)
}

// NoteDeveloperSchema keeps the schema a developer apply's admission read from
// its control-plane image, for the send. Call it before Apply.
func (s *SelfApplier) NoteDeveloperSchema(attemptID string, schema int) {
	s.schemas.Store(attemptID, schema)
}

// developerCommit is the commit a developer apply's images carry.
func (s *SelfApplier) developerCommit(ctx context.Context, a Attempt) (string, error) {
	if s.DeveloperCommit == nil {
		return "", errors.New("no registry reader is wired to read the images' commit")
	}
	return s.DeveloperCommit(ctx, a.RequestedDigests)
}

// absentOutput names what is missing: no actor at all, or its socket.
func (s *SelfApplier) absentOutput() string {
	if s.updater == nil {
		return "this control plane has no recovery actor to replace it: it was not installed with the seed"
	}
	return "no recovery actor socket to apply the control plane over at " + s.updater.SocketPath()
}

func (s *SelfApplier) fail(attemptID, reason, output string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.FailAttempt(ctx, attemptID, reason, output); err != nil {
		s.log.Error("self-apply: could not record the failure", "attempt_id", attemptID, "reason", reason, "err", err)
	}
}
