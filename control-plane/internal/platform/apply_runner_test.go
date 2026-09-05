package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// The state machine, with a fake agent and a fake store: no database, no
// websocket, no sleeping for fifteen minutes. Every case here is one sentence
// of the contract (control-api.md §"Platform-release apply").

const (
	testHostID    = "11111111-1111-4111-8111-111111111111"
	testReleaseID = "22222222-2222-4222-8222-222222222222"
	testCommit    = "1f0c1e0e0c5a9d1b7a2f3e4d5c6b7a8901234567"
)

// fakeStore is applyStore in a map. It enforces the ONE rule the real store's
// SQL enforces and the machine depends on: a terminal attempt never changes
// again, which is what makes a late report a no-op.
type fakeStore struct {
	mu       sync.Mutex
	attempts map[string]*Attempt
	sessions int
	status   string
	release  Release
	requests map[string]string // request id → attempt id
	nextReq  int
	counts   int // how many times the session count was read
}

func newFakeStore(a Attempt) *fakeStore {
	cp := a
	return &fakeStore{
		attempts: map[string]*Attempt{a.ID: &cp},
		status:   "online",
		release:  Release{ID: testReleaseID, SourceCommit: testCommit, SchemaVersion: 75},
		requests: map[string]string{},
	}
}

func (f *fakeStore) get(id string) *Attempt {
	return f.attempts[id]
}

func (f *fakeStore) snapshot(id string) Attempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.attempts[id]
}

func (f *fakeStore) setSessions(n int) {
	f.mu.Lock()
	f.sessions = n
	f.mu.Unlock()
}

func (f *fakeStore) HostStatus(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, nil
}

func (f *fakeStore) NonTerminalSessions(context.Context, string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts++
	return f.sessions, nil
}

func (f *fakeStore) SetWaitingSessions(_ context.Context, id string, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil || TerminalAttemptState(a.State) {
		return nil
	}
	a.State = AttemptWaitingSessions
	remaining := n
	a.SessionsRemaining = &remaining
	return nil
}

func (f *fakeStore) MintRequestID(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil || TerminalAttemptState(a.State) {
		return "", ErrAttemptNotFound
	}
	f.nextReq++
	req := "req-" + string(rune('a'+f.nextReq))
	f.requests[req] = id
	a.State = AttemptPending
	a.SessionsRemaining = nil
	now := time.Now()
	a.StartedAt = &now
	return req, nil
}

func (f *fakeStore) FailAttempt(_ context.Context, id, reason, output string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil || TerminalAttemptState(a.State) {
		return nil
	}
	a.State = AttemptFailed
	r := reason
	a.Reason = &r
	if output != "" {
		a.Output = output
	}
	return nil
}

func (f *fakeStore) SucceedAttempt(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil || TerminalAttemptState(a.State) {
		return false, nil
	}
	a.State = AttemptSucceeded
	a.Reason = nil
	return true, nil
}

func (f *fakeStore) Attempt(_ context.Context, id string) (Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil {
		return Attempt{}, ErrAttemptNotFound
	}
	return *a, nil
}

func (f *fakeStore) AttemptByRequestID(_ context.Context, req string) (Attempt, error) {
	f.mu.Lock()
	id, ok := f.requests[req]
	f.mu.Unlock()
	if !ok {
		return Attempt{}, ErrAttemptNotFound
	}
	return f.Attempt(context.Background(), id)
}

func (f *fakeStore) RecordReleaseState(_ context.Context, id, state string, previous []PreviousDigest, output string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.get(id)
	if a == nil || TerminalAttemptState(a.State) {
		return nil
	}
	a.State = state
	if len(previous) > 0 {
		a.PreviousDigests = previous
	}
	if output != "" {
		a.Output = output
	}
	return nil
}

func (f *fakeStore) SetPreviousDigests(_ context.Context, id string, previous []PreviousDigest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.get(id); a != nil && len(previous) > 0 {
		a.PreviousDigests = previous
	}
	return nil
}

func (f *fakeStore) OpenHostAttempt(_ context.Context, hostID string) (Attempt, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.attempts {
		if a.HostID != nil && *a.HostID == hostID && !TerminalAttemptState(a.State) {
			return *a, f.release.SourceCommit, nil
		}
	}
	return Attempt{}, "", ErrAttemptNotFound
}

func (f *fakeStore) OpenAttempts(context.Context) ([]Attempt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Attempt, 0)
	for _, a := range f.attempts {
		if !TerminalAttemptState(a.State) {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeStore) Release(context.Context, string) (Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.release, nil
}

// fakeAgent records what was sent and answers whatever the test set.
type fakeAgent struct {
	mu       sync.Mutex
	sent     []ApplyCommand
	ack      Ack
	err      error
	cordons  int
	uncordon int
}

func (a *fakeAgent) send(_ context.Context, _ string, cmd ApplyCommand) (Ack, error) {
	a.mu.Lock()
	a.sent = append(a.sent, cmd)
	ack, err := a.ack, a.err
	a.mu.Unlock()
	return ack, err
}

func (a *fakeAgent) sentCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent)
}

func (a *fakeAgent) deps() ApplyDeps {
	return ApplyDeps{
		Cordon: func(context.Context, string) error {
			a.mu.Lock()
			a.cordons++
			a.mu.Unlock()
			return nil
		},
		Uncordon: func(context.Context, string) error {
			a.mu.Lock()
			a.uncordon++
			a.mu.Unlock()
			return nil
		},
		Send: a.send,
	}
}

func queuedAttempt(force bool) Attempt {
	host := testHostID
	rel := testReleaseID
	return Attempt{
		ID:     "33333333-3333-4333-8333-333333333333",
		Kind:   KindApply,
		Target: TargetHost,
		HostID: &host, ReleaseID: &rel,
		RequestedDigests: []ComponentDigest{{
			Name: ComponentNodeAgent, Image: "ghcr.io/accreleus/quasar/quasar-node-agent",
			Digest: "sha256:" + repeat64('9'),
		}},
		PreviousDigests: []PreviousDigest{{Name: ComponentNodeAgent}},
		State:           AttemptQueued,
		Force:           force,
		CreatedAt:       time.Now(),
	}
}

func repeat64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}

func testRunner(store applyStore, deps ApplyDeps) *Runner {
	r := NewRunner(store, deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.AckTimeout = 50 * time.Millisecond
	r.PollInterval = 2 * time.Millisecond
	r.Deadline = 3 * time.Second
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The whole happy path: drain to zero, send, and succeed on the new agent's
// register — which is the success evidence, because the recreate killed the
// process that would have reported it.
func TestApplyDrainsThenSucceedsOnRegisterEvidence(t *testing.T) {
	a := queuedAttempt(false)
	store := newFakeStore(a)
	store.setSessions(2)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to report the sessions it is waiting on", func() bool {
		s := store.snapshot(a.ID)
		return s.State == AttemptWaitingSessions && s.SessionsRemaining != nil && *s.SessionsRemaining == 2
	})
	if agent.sentCount() != 0 {
		t.Fatal("release_apply was sent while sessions were still running")
	}

	store.setSessions(0)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
	if got := store.snapshot(a.ID).State; got != AttemptPending {
		t.Fatalf("state after send = %q, want pending", got)
	}

	// Progress relayed, then the new agent registers on the requested commit.
	r.HandleReleaseState(context.Background(), testHostID, ReleaseStateReport{
		RequestID: agent.sent[0].RequestID, State: AttemptRecreating,
		Previous: []PreviousDigest{{Name: ComponentNodeAgent, Digest: strPtr("sha256:" + repeat64('1'))}},
	})
	waitFor(t, "the relayed state", func() bool { return store.snapshot(a.ID).State == AttemptRecreating })

	commit := testCommit
	r.HandleRegister(context.Background(), testHostID, &commit)
	waitFor(t, "the attempt to succeed", func() bool { return store.snapshot(a.ID).State == AttemptSucceeded })

	final := store.snapshot(a.ID)
	if len(final.PreviousDigests) != 1 || final.PreviousDigests[0].Digest == nil {
		t.Errorf("previous digests were not recorded: %+v", final.PreviousDigests)
	}
	// The host was serving, so it goes back to serving.
	waitFor(t, "the cordon to be lifted", func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.cordons == 1 && agent.uncordon == 1
	})
}

// force is the operator agreeing to end N sessions: the wait is skipped, the
// count is still recorded, and NOTHING here stops a session — the recreate does.
func TestForceSkipsTheWaitAndRecordsTheCount(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	store.setSessions(3)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "release_apply to be sent despite live sessions", func() bool { return agent.sentCount() == 1 })
	if !agent.sent[0].Force {
		t.Error("force was not carried onto the wire")
	}
	store.mu.Lock()
	counts := store.counts
	store.mu.Unlock()
	if counts != 1 {
		t.Errorf("session count read %d times, want exactly 1 (force must not poll)", counts)
	}
}

// A rejected ack is a failure with the ack's own reason, and it never becomes
// "unsupported": the agent answered, so its build is not the problem.
func TestAckRejectionRecordsTheAgentsReason(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: false, Error: ReasonUpdaterAbsentFailure}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	if got := *store.snapshot(a.ID).Reason; got != ReasonUpdaterAbsentFailure {
		t.Errorf("reason = %q, want updater_absent", got)
	}
	if !r.Supported(testHostID) {
		t.Error("a rejecting agent must not be marked as predating the amendment")
	}
}

// Silence is the ONLY signal that an agent predates the amendment, because an
// unknown downstream type is wire-silent. It is recorded, not retried, and the
// host stays unsupported until it registers again.
func TestNoAckMarksTheHostUnsupportedUntilItRegisters(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{err: context.DeadlineExceeded}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	if got := *store.snapshot(a.ID).Reason; got != ReasonUnsupported {
		t.Errorf("reason = %q, want unsupported", got)
	}
	if r.Supported(testHostID) {
		t.Error("the host should be treated as not supporting apply")
	}
	// A register is the only evidence the build changed.
	r.HandleRegister(context.Background(), testHostID, nil)
	if !r.Supported(testHostID) {
		t.Error("a register must clear the unsupported flag")
	}
}

// An attempt that never reaches a terminal state is failed with `timeout` when
// the deadline expires — the rule that stops one target wedging forever.
func TestDeadlineFailsTheAttemptWithTimeout(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	r.Deadline = 40 * time.Millisecond
	defer r.Close()

	r.Start(a)
	waitFor(t, "the deadline to fire", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	if got := *store.snapshot(a.ID).Reason; got != ReasonTimeout {
		t.Errorf("reason = %q, want timeout", got)
	}
}

// A late release_state for an attempt already resolved by the register is a
// no-op, not a conflict.
func TestLateReleaseStateForAResolvedAttemptIsANoOp(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
	commit := testCommit
	r.HandleRegister(context.Background(), testHostID, &commit)
	waitFor(t, "the attempt to succeed", func() bool { return store.snapshot(a.ID).State == AttemptSucceeded })

	failed := ReasonRecreateFailed
	r.HandleReleaseState(context.Background(), testHostID, ReleaseStateReport{
		RequestID: agent.sent[0].RequestID, State: AttemptFailed, Reason: &failed,
	})
	if got := store.snapshot(a.ID); got.State != AttemptSucceeded || got.Reason != nil {
		t.Errorf("a resolved attempt was rewritten: state=%q reason=%v", got.State, got.Reason)
	}
}

// An unknown request id is DROPPED, not stored — the same posture image_state
// takes with an unknown image id.
func TestReleaseStateForAnUnknownRequestIsDropped(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	r := testRunner(store, (&fakeAgent{ack: Ack{OK: true}}).deps())
	defer r.Close()

	r.HandleReleaseState(context.Background(), testHostID, ReleaseStateReport{
		RequestID: "nobody", State: AttemptPulling,
	})
	if got := store.snapshot(a.ID).State; got != AttemptQueued {
		t.Errorf("state = %q, want the attempt untouched", got)
	}
}

// A host an admin had already cordoned STAYS cordoned: the apply restores the
// state it found, it does not impose one.
func TestAnAlreadyCordonedHostIsLeftCordoned(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	store.status = "draining"
	agent := &fakeAgent{ack: Ack{OK: false, Error: ReasonBusy}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to resolve", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	waitFor(t, "the cordon to be restored", func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()
		return agent.uncordon == 0 && agent.cordons == 1
	})
}

// A register on a DIFFERENT commit mid-apply leaves the attempt running: an
// agent may reconnect on the old build, and the deadline decides.
func TestRegisterOnADifferentCommitDoesNotResolveTheAttempt(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
	other := "abcdef1234567890abcdef1234567890abcdef12"
	r.HandleRegister(context.Background(), testHostID, &other)
	if got := store.snapshot(a.ID).State; TerminalAttemptState(got) {
		t.Errorf("state = %q, want the attempt still open", got)
	}
}

// A restart must not orphan an attempt: Adopt re-attaches to every open row.
func TestAdoptResumesAnAttemptLeftInFlight(t *testing.T) {
	a := queuedAttempt(true)
	a.State = AttemptWaitingSessions
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Adopt(context.Background())
	waitFor(t, "the adopted attempt to be sent", func() bool { return agent.sentCount() == 1 })
}

// A second Start for an attempt already being driven is dropped, so the
// endpoint and Adopt cannot double-drive one row.
func TestStartIsIdempotentPerAttempt(t *testing.T) {
	a := queuedAttempt(false)
	store := newFakeStore(a)
	store.setSessions(1) // parks it in waiting_sessions
	agent := &fakeAgent{ack: Ack{OK: true}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	r.Start(a)
	waitFor(t, "the drain to start", func() bool { return store.snapshot(a.ID).State == AttemptWaitingSessions })
	store.setSessions(0)
	waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() >= 1 })
	time.Sleep(20 * time.Millisecond)
	if n := agent.sentCount(); n != 1 {
		t.Errorf("release_apply sent %d times, want 1", n)
	}
}

func TestUnknownAckReasonIsStoredVerbatim(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{ack: Ack{OK: false, Error: "a_reason_from_the_future"}}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	if got := *store.snapshot(a.ID).Reason; got != "a_reason_from_the_future" {
		t.Errorf("reason = %q, want it stored verbatim", got)
	}
}

// A delivery failure is not silence: the agent could not be reached, which is
// updater_unreachable, and it must NOT mark the host as predating the amendment.
func TestDeliveryFailureIsUpdaterUnreachable(t *testing.T) {
	a := queuedAttempt(true)
	store := newFakeStore(a)
	agent := &fakeAgent{err: errors.New("agent not connected")}
	r := testRunner(store, agent.deps())
	defer r.Close()

	r.Start(a)
	waitFor(t, "the attempt to fail", func() bool { return store.snapshot(a.ID).State == AttemptFailed })
	if got := *store.snapshot(a.ID).Reason; got != ReasonUpdaterUnreachable {
		t.Errorf("reason = %q, want updater_unreachable", got)
	}
	if !r.Supported(testHostID) {
		t.Error("a delivery failure must not read as an old agent")
	}
}

func strPtr(s string) *string { return &s }
