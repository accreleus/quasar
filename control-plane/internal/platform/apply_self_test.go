package platform

import (
	"context"
	"testing"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// The control-plane target with no recovery actor, and the helpers the owned
// tests (actor_client_test.go) share.

// AttemptRequestID completes selfStore on the runner tests' fake.
func (f *fakeStore) AttemptRequestID(_ context.Context, attemptID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for req, id := range f.requests {
		if id == attemptID {
			return req, nil
		}
	}
	return "", nil
}

func controlPlaneAttempt(state string) Attempt {
	return Attempt{
		ID: "cp-1", Kind: KindApply, Target: TargetControlPlane, State: state,
		ReleaseID: strPtr(testReleaseID),
		RequestedDigests: []ComponentDigest{{
			Name: ComponentControlPlane, Image: "ghcr.io/x/quasar-control-plane", Digest: "sha256:new",
		}},
		PreviousDigests: []PreviousDigest{}, CreatedAt: time.Now(),
	}
}

func testSelfApplier(t *testing.T, store selfStore, up UpdaterAPI) *SelfApplier {
	t.Helper()
	s := NewSelfApplier(store, up, testLogger())
	s.PollInterval = 5 * time.Millisecond
	s.Deadline = 2 * time.Second
	s.Identity = func() buildinfo.Identity { return buildinfo.Identity{Version: "dev"} }
	return s
}

// A control plane not installed with the seed has nothing to replace it: the
// apply is refused with a name, not attempted.
func TestSelfApplyWithNoRecoveryActorIsRefusedUpdaterAbsent(t *testing.T) {
	store := newFakeStore(controlPlaneAttempt(AttemptQueued))
	self := testSelfApplier(t, store, nil)
	if self.UpdaterPresent() {
		t.Fatal("a control plane with no recovery actor reported one present")
	}

	self.Apply(context.Background(), controlPlaneAttempt(AttemptQueued))

	a := store.snapshot("cp-1")
	if a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUpdaterAbsentFailure {
		t.Fatalf("state=%q reason=%v, want failed/updater_absent", a.State, a.Reason)
	}
}

// A sent attempt adopted by a control plane with no recovery actor has no
// verdict to wait for: it fails closed rather than trusting the booted binary.
func TestAdoptWithNoRecoveryActorFailsASentAttempt(t *testing.T) {
	open := controlPlaneAttempt(AttemptRecreating)
	store := newFakeStore(open)
	if _, err := store.MintRequestID(context.Background(), "cp-1"); err != nil {
		t.Fatal(err)
	}
	self := testSelfApplier(t, store, nil)
	self.Identity = func() buildinfo.Identity {
		return buildinfo.Identity{Version: "0.9.0", SourceCommit: strPtr(testCommit)}
	}

	if !self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt reported the attempt unresolved")
	}
	if a := store.snapshot("cp-1"); a.State != AttemptFailed || a.Reason == nil || *a.Reason != ReasonUpdaterAbsentFailure {
		t.Fatalf("state=%q reason=%v, want failed/updater_absent", a.State, a.Reason)
	}
}

// An attempt the restart caught before it was sent carries no request id; the
// run re-drives it rather than adopting a poll with nothing to poll.
func TestAdoptLeavesAnUnsentAttemptToTheRun(t *testing.T) {
	open := controlPlaneAttempt(AttemptQueued)
	store := newFakeStore(open)
	self := testSelfApplier(t, store, nil)

	if self.Adopt(context.Background(), open, testCommit) {
		t.Fatal("Adopt claimed an unsent attempt")
	}
	if got := store.snapshot("cp-1").State; got != AttemptQueued {
		t.Fatalf("state = %q, want it left queued", got)
	}
}
