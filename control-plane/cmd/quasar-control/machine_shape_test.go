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
