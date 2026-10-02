package platform

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// A control plane booted by an operator's `quasar-recovery reconfigure` (#386).
// The reconfigure is the operator's attempt: it has no platform_apply_attempts
// row, and the control socket never answers with it (it answers only for
// attempts submitted on that socket). So the boot-time adopters, which start
// from rows, have nothing of it to adopt, and a row of the control plane's own
// never takes the busy machine for its verdict.

const reconfigureFixture = "status-combined-operator-reconfigure.json"

// The shared fixture: the machine is busy with the operator's attempt, and the
// control plane's own request has no result in it.
func TestTheControlSocketDuringAReconfigureHoldsNoResultOfTheControlPlanes(t *testing.T) {
	path, _ := serveStatus(t, fixtureBody(t, reconfigureFixture))
	_, err := NewActorClient(path).Result(context.Background(), "3c8e2a61-5b7d-4e19-a0f4-6d2b9c1e7f30")
	if !errors.Is(err, ErrNoResult) {
		t.Fatalf("Result = %v, want ErrNoResult", err)
	}
	m, ok := NewOwnMachineReader(path).Read(context.Background())
	if !ok || m.Identity.InstallMode == nil || *m.Identity.InstallMode != InstallOwned {
		t.Fatalf("own machine = %+v ok=%v, want an owned machine read as usual", m, ok)
	}
}

// A row this control plane sent is decided by the actor's result for it, never
// by the booted binary alone, and never by the operator's attempt in flight:
// with no result for it the row fails closed. Defence in depth: the actor admits
// one attempt at a time, so this row cannot really be verifying while the
// operator's attempt is in flight.
func TestABootMidReconfigureNeverTakesTheBusyMachineAsItsOwnVerdict(t *testing.T) {
	path, _ := serveStatus(t, fixtureBody(t, reconfigureFixture))
	open := ownedAttempt(AttemptVerifying)
	store := newFakeStore(open)
	if _, err := store.MintRequestID(context.Background(), "cp-1"); err != nil {
		t.Fatal(err)
	}
	self := testSelfApplier(t, store, NewActorClient(path))
	self.Deadline = 30 * time.Millisecond
	self.VerdictSilence = 30 * time.Millisecond
	self.Identity = func() buildinfo.Identity {
		return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
	}
	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonTimeout {
		t.Fatalf("state=%q reason=%v, want failed/timeout, never succeeded", a.State, a.Reason)
	}
}

// A control-plane apply that was never sent when the reconfigure restarted the
// control plane is re-driven, and meets the reconfigure still in flight: the
// actor's busy refusal is its failure, named.
func TestAnUnsentApplyMeetingAReconfigureIsRefusedBusy(t *testing.T) {
	open := ownedAttempt(AttemptQueued)
	store := newFakeStore(open)
	actor := &fakeActor{reject: &actorsocket.Rejection{
		Reason:  actorsocket.ReasonBusy,
		Message: "attempt 5b0e9d4c-7a21-4f3e-8c6d-2e1f0a9b8c7d is in flight; submit once it has finished",
	}}
	self := testSelfApplier(t, store, NewActorClient(serveActor(t, actor)))
	if self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt resolved an attempt that was never sent")
	}
	self.Apply(context.Background(), open)
	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonBusy || a.Output == "" {
		t.Fatalf("state=%q reason=%v output=%q, want failed/busy with the actor's message", a.State, a.Reason, a.Output)
	}
}

// With no open row, neither control-plane adopter drives anything: a boot
// caused by a reconfigure starts clean.
func TestABootWithNoOpenControlPlaneAttemptAdoptsNothing(t *testing.T) {
	fleetStore := newFakeFleetStore(false)
	fleetStore.run.State = RunSucceeded
	drivers := &fakeDrivers{store: fleetStore}
	testFleet(t, fleetStore, drivers, planningView(fleetStore, fakeReleaseSchema)).Adopt(context.Background())

	self := &countingSelf{}
	dev := NewSelfDeveloperRunner(&emptyDeveloperStore{}, self, FleetCordons{}, nil, testLogger())
	t.Cleanup(dev.Close)
	dev.Adopt(context.Background())
	time.Sleep(20 * time.Millisecond)

	if steps := drivers.steps(); len(steps) != 0 {
		t.Fatalf("the fleet adopter drove %v", steps)
	}
	if self.calls() != 0 {
		t.Fatalf("the developer-apply adopter drove %d attempts", self.calls())
	}
}

type countingSelf struct {
	mu sync.Mutex
	n  int
}

func (c *countingSelf) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingSelf) UpdaterPresent() bool { return true }

func (c *countingSelf) Apply(context.Context, Attempt) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingSelf) Adopt(ctx context.Context, a Attempt, _ string) bool {
	c.Apply(ctx, a)
	return true
}

// emptyDeveloperStore is a database with no control-plane attempt in it.
type emptyDeveloperStore struct{}

func (emptyDeveloperStore) Hosts(context.Context) ([]HostIdentity, error) { return nil, nil }
func (emptyDeveloperStore) Attempt(context.Context, string) (Attempt, error) {
	return Attempt{}, ErrAttemptNotFound
}
func (emptyDeveloperStore) OpenAttempts(context.Context) ([]Attempt, error)           { return nil, nil }
func (emptyDeveloperStore) FleetInFlightSessions(context.Context) (int, error)        { return 0, nil }
func (emptyDeveloperStore) FleetNonTerminalSessions(context.Context) (int, error)     { return 0, nil }
func (emptyDeveloperStore) SetWaitingSessions(context.Context, string, int) error     { return nil }
func (emptyDeveloperStore) FailAttempt(context.Context, string, string, string) error { return nil }
func (emptyDeveloperStore) TerminalStandaloneControlPlaneHolds(context.Context) ([]PlatformHold, error) {
	return nil, nil
}
