// Developer apply against the control plane's own machine (control-api.md
// §"Developer apply", §"The control plane's own machine"), on a real Postgres.
package platform

import (
	"context"
	"net/http"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// fakeOwnMachine is a recovery actor that answered with m, or did not (ok false).
type fakeOwnMachine struct {
	m  OwnMachine
	ok bool
}

func (f *fakeOwnMachine) Read(context.Context) (OwnMachine, bool) { return f.m, f.ok }
func (f *fakeOwnMachine) Invalidate()                             {}

func ownedMachine(role actorsocket.Role, node string) *fakeOwnMachine {
	st := actorsocket.Status{
		Actor:    actorsocket.ActorIdentity{Version: "0.4.0", Commit: commitB},
		Role:     role,
		NodeName: &node,
		Database: actorsocket.DatabaseOwned,
	}
	return &fakeOwnMachine{m: OwnMachineFromStatus(st), ok: true}
}

// newOwnMachineDevHarness is newDevHarness with the control plane's own machine wired.
func newOwnMachineDevHarness(t *testing.T, own OwnMachineSource) *devHarness {
	t.Helper()
	return newShapedDevHarness(t, own, MachineShape{})
}

// newShapedDevHarness also wires the machine shape the control plane's own
// configuration names (QUASAR_MACHINE_ROLE, QUASAR_MACHINE_NODE_NAME).
func newShapedDevHarness(t *testing.T, own OwnMachineSource, shape MachineShape) *devHarness {
	t.Helper()
	images := &fakeDevImages{commit: commitB}
	h := newApplyHarness(t, func(_ *applyHarness, handler *ApplyHandler) {
		handler.WithDeveloperApply(images, []string{"registry.example.invalid/dev"}).
			WithOwnMachine(own).WithMachineShape(shape)
	})
	mustExec(t, h.pool, `UPDATE hosts SET install_mode = 'owned' WHERE id = $1::uuid`, h.hostID)
	return &devHarness{applyHarness: h, images: images}
}

func cpComponent() ComponentDigest {
	return ComponentDigest{Name: ComponentControlPlane, Image: "registry.example.invalid/dev/quasar-control-plane", Digest: devAgentDigest}
}

func TestDeveloperApplyToTheControlPlaneOnAnOwnedMachine(t *testing.T) {
	body := map[string]any{"target": "control_plane", "components": []ComponentDigest{cpComponent()}}

	// A recovery actor that did not answer reads null: the safe refusal.
	silent := newOwnMachineDevHarness(t, &fakeOwnMachine{})
	if code, out := silent.post(t, devURL, silent.adminToken, body); code != http.StatusConflict || errCode(t, out) != CodeTargetNotOwned {
		t.Errorf("silent actor = %d %s, want 409 target_not_owned", code, out)
	}

	h := newOwnMachineDevHarness(t, ownedMachine(actorsocket.RoleControlOnly, "attic-server"))
	code, out := h.post(t, devURL, h.adminToken, body)
	if code != http.StatusNotImplemented || errCode(t, out) != CodeApplyUnsupported {
		t.Fatalf("owned control plane = %d %s, want 501 apply_unsupported", code, out)
	}
	if h.images.count() != 0 || h.agent.sentCount() != 0 {
		t.Fatal("a refused control-plane request read the registry or sent a command")
	}
}

// A combined host's own agent takes node-agent only: its actor moves in the
// control-plane step. Decided from the control plane's own configuration, so it
// holds with the recovery actor silent (fail closed).
func TestDeveloperApplyToTheCombinedHostNamesOnlyTheAgent(t *testing.T) {
	h := newShapedDevHarness(t, &fakeOwnMachine{}, MachineShape{Role: MachineRoleCombined, NodeName: "gpu-01"})
	code, out := h.post(t, devURL, h.adminToken, h.body(agentComponent(), actorComponent()))
	if code != http.StatusBadRequest || errCode(t, out) != "validation_failed" {
		t.Fatalf("actor to the combined host = %d %s, want 400 validation_failed", code, out)
	}
	if h.images.count() != 0 || h.agent.sentCount() != 0 {
		t.Fatal("a refused request read the registry or sent a command")
	}
	// Checked before any other refusal: even an offline combined host answers 400.
	mustExec(t, h.pool, `UPDATE hosts SET status = 'offline' WHERE id = $1::uuid`, h.hostID)
	if code, out := h.post(t, devURL, h.adminToken, h.body(actorComponent())); code != http.StatusBadRequest {
		t.Fatalf("actor alone to an offline combined host = %d %s, want 400", code, out)
	}
	mustExec(t, h.pool, `UPDATE hosts SET status = 'online' WHERE id = $1::uuid`, h.hostID)
	if code, out := h.post(t, devURL, h.adminToken, h.body(agentComponent())); code != http.StatusAccepted {
		t.Fatalf("agent only to the combined host = %d %s, want 202", code, out)
	}
}

func TestDeveloperApplyToAnotherHostMayNameTheActor(t *testing.T) {
	for name, shape := range map[string]MachineShape{
		"a different node":        {Role: MachineRoleCombined, NodeName: "some-other-machine"},
		"control-only, same name": {Role: MachineRoleControlOnly, NodeName: "gpu-01"},
		"not an owned machine":    {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newShapedDevHarness(t, ownedMachine(actorsocket.RoleCombined, "gpu-01"), shape)
			if code, out := h.post(t, devURL, h.adminToken, h.body(agentComponent(), actorComponent())); code != http.StatusAccepted {
				t.Fatalf("= %d %s, want 202", code, out)
			}
		})
	}
}
