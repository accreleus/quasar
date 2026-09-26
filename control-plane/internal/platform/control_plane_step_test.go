package platform

import (
	"context"
	"strings"
	"testing"
)

// The fleet run's control-plane step on an owned machine (#363): the actor
// moves first, and a migrating release is refused before anything moves.

func TestOrderControlPlaneComponents(t *testing.T) {
	cp := ComponentDigest{Name: ComponentControlPlane, Image: "r/quasar-control-plane", Digest: "sha256:c"}
	actor := ComponentDigest{Name: ComponentRecovery, Image: "r/quasar-recovery", Digest: "sha256:a"}
	onRelease, behind := testCommit, "0000000000000000000000000000000000000000"
	names := func(cs []ComponentDigest) string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		name        string
		release     []ComponentDigest
		owned       bool
		actorCommit *string
		want        string
	}{
		{"owned, actor behind", []ComponentDigest{cp, actor}, true, &behind, "recovery-actor,control-plane"},
		{"owned, actor not reported", []ComponentDigest{actor, cp}, true, nil, "recovery-actor,control-plane"},
		{"owned, actor on the release", []ComponentDigest{cp, actor}, true, &onRelease, "control-plane"},
		{"owned, release names no actor", []ComponentDigest{cp}, true, &behind, "control-plane"},
		{"Compose never moves an actor", []ComponentDigest{cp, actor}, false, &behind, "control-plane"},
		{"no control plane", []ComponentDigest{actor}, true, &behind, ""},
	} {
		if got := names(OrderControlPlaneComponents(tc.release, testCommit, tc.owned, tc.actorCommit)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// fixedComponents resolves every release to the same components.
type fixedComponents []ComponentDigest

func (c fixedComponents) HostComponents(context.Context, Release) ([]ComponentDigest, error) {
	return []ComponentDigest{{Name: ComponentNodeAgent, Image: "r/quasar-node-agent", Digest: "sha256:n"}}, nil
}

func (c fixedComponents) ControlPlaneComponents(context.Context, Release) ([]ComponentDigest, error) {
	return c, nil
}

func ownedFleet(t *testing.T, store *fakeFleetStore, d *fakeDrivers, own OwnMachineSource) *FleetRunner {
	t.Helper()
	f := testFleet(t, store, d, fleetView("", hostTarget("h1", "gpu-01", ""), hostTarget("h2", "gpu-02", "")))
	f.resolve = fixedComponents{
		{Name: ComponentControlPlane, Image: "r/quasar-control-plane", Digest: "sha256:c"},
		{Name: ComponentRecovery, Image: "r/quasar-recovery", Digest: "sha256:a"},
	}
	return f.WithMachineShape(MachineShape{Role: MachineRoleCombined, NodeName: "gpu-01"}).WithOwnMachine(own)
}

func TestAnOwnedControlPlaneStepMovesTheActorFirstThenTheControlPlane(t *testing.T) {
	store := newFakeFleetStore(false)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := ownedFleet(t, store, d, &fakeOwnMachine{})
	f.SchemaVersion = fakeReleaseSchema // not migrating

	run := runToEnd(t, f, store)
	if run.State != RunSucceeded {
		t.Fatalf("run = %q (%v), want succeeded", run.State, run.Error)
	}
	attempts, _ := store.RunAttempts(context.Background(), testRunID)
	var cp *Attempt
	for i := range attempts {
		if attempts[i].Target == TargetControlPlane {
			cp = &attempts[i]
		}
	}
	if cp == nil || len(cp.RequestedDigests) != 2 ||
		cp.RequestedDigests[0].Name != ComponentRecovery || cp.RequestedDigests[1].Name != ComponentControlPlane {
		t.Fatalf("control-plane attempt = %+v, want [recovery-actor, control-plane]", cp)
	}
}

// #352 decision 14: a non-migrating control plane is an ordinary unattended step
// on an owned machine too.
func TestAnUnattendedRunUpdatesAnOwnedControlPlane(t *testing.T) {
	store := newFakeFleetStore(false)
	store.run.Unattended = true
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := ownedFleet(t, store, d, &fakeOwnMachine{})
	f.SchemaVersion = fakeReleaseSchema

	run := runToEnd(t, f, store)
	if run.State != RunSucceeded {
		t.Fatalf("run = %q (%v), want succeeded", run.State, run.Error)
	}
	if steps := d.steps(); len(steps) == 0 || steps[0] != TargetControlPlane {
		t.Fatalf("steps = %v, want the control plane first", steps)
	}
}

// A migrating step needs the pre-update dump (RH06-12, #364): refused before
// the fleet is cordoned, drained or sent anything, so refusing changes nothing.
func TestAMigratingControlPlaneStepOnAnOwnedMachineIsRefusedBeforeAnythingMoves(t *testing.T) {
	store := newFakeFleetStore(true)
	d := &fakeDrivers{store: store, outcome: map[string]string{}}
	f := ownedFleet(t, store, d, &fakeOwnMachine{})
	// testFleet's default: the fixture release migrates.

	run := runToEnd(t, f, store)
	if run.State != RunFailed || run.Error == nil || !strings.Contains(*run.Error, "#364") {
		t.Fatalf("run = %q (%v), want failed naming #364", run.State, run.Error)
	}
	if steps := d.steps(); len(steps) != 0 {
		t.Fatalf("steps = %v, want none", steps)
	}
	if attempts, _ := store.RunAttempts(context.Background(), testRunID); len(attempts) != 0 {
		t.Fatalf("attempts = %+v, want none", attempts)
	}
	if cordons, _ := store.CordonedHosts(context.Background(), testRunID); len(cordons) != 0 {
		t.Fatalf("cordons = %+v, want none: nothing was cordoned", cordons)
	}
}
