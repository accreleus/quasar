package main

import (
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/config"
	"github.com/accreleus/quasar/control-plane/internal/platform"
)

// A Compose control plane given a local enrollment token serves no machine
// shape (control-api.md: null), yet its developer-apply check still fails closed.
func TestTheLocalTokenShapesOnlyTheDeveloperApplyCheck(t *testing.T) {
	compose := &config.Config{LocalEnrollmentNodeName: "living-room-pc"}
	if got := machineShape(compose); got != (platform.MachineShape{}) {
		t.Fatalf("served shape = %+v, want none", got)
	}
	if node, ok := applyMachineShape(compose).CombinedNodeName(); !ok || node != "living-room-pc" {
		t.Fatalf("apply shape = %q %v, want the token's node as combined", node, ok)
	}

	owned := &config.Config{
		MachineRole:             platform.MachineRoleControlOnly,
		MachineNodeName:         "attic-server",
		LocalEnrollmentNodeName: "attic-server",
	}
	want := platform.MachineShape{Role: platform.MachineRoleControlOnly, NodeName: "attic-server"}
	if got := machineShape(owned); got != want {
		t.Fatalf("served shape = %+v, want %+v", got, want)
	}
	if got := applyMachineShape(owned); got != want {
		t.Fatalf("apply shape = %+v, want the configured one %+v", got, want)
	}
}

// TestTheOwnMachineGuardRefusesAnIdentityItCannotTrust: Forget and Remove host
// must refuse a host whenever the control plane cannot tell it apart from its
// own machine, and an owned install (a recovery control socket) with no machine
// shape cannot tell any host apart (#1395 part 2). A Compose control plane has
// no agent of its own, so its hosts stay removable.
func TestTheOwnMachineGuardRefusesAnIdentityItCannotTrust(t *testing.T) {
	unowned := &config.Config{}
	if ownMachineGuard(unowned)("some-other-host") {
		t.Error("an agentless control plane refused a host; nothing of its own is at risk")
	}

	ownedButUnshaped := &config.Config{RecoveryControlSocket: "/run/quasar/recovery.sock"}
	if !ownMachineGuard(ownedButUnshaped)("some-other-host") {
		t.Error("an owned install with no machine shape let a host through: that host may be its own agent")
	}

	// A shape the loader rejects (role without its node name) is not evidence of
	// anything, so it refuses every host too.
	unknown := &config.Config{MachineRole: platform.MachineRoleCombined}
	if !ownMachineGuard(unknown)("some-other-host") {
		t.Error("a role with no node name let a host through")
	}

	// And a shape that does name another machine still lets that host go: failing
	// closed must not become refusing every removal.
	known := &config.Config{MachineRole: platform.MachineRoleCombined, MachineNodeName: "attic-server"}
	if ownMachineGuard(known)("some-other-host") {
		t.Error("a host on another machine was refused by a correctly shaped control plane")
	}
	if !ownMachineGuard(known)("attic-server") {
		t.Error("the control plane's own host was not refused")
	}
}
