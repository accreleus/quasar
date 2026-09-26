package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/buildinfo"
)

// below_floor (control-api.md amendment 14 §"below_floor"): judged by the pure planner
// against the control plane's declared floor, served per host, and changing no
// eligibility; a revert from or to a release below it is refused.

var testFloor = buildinfo.Floor{NodeAgent: "0.5.0", RecoveryActor: "0.5.0"}

func agentVersion(v string) func(*HostIdentity) {
	return func(h *HostIdentity) { h.AgentVersion = str(v) }
}

func actorVersion(v string) func(*HostIdentity) {
	return func(h *HostIdentity) { h.RecoveryActorVersion = str(v) }
}

func TestHostBelowFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []func(*HostIdentity)
		want bool
	}{
		{"agent and actor on the floor", []func(*HostIdentity){agentVersion("0.5.0"), actorVersion("0.5.0")}, false},
		{"agent below", []func(*HostIdentity){agentVersion("0.4.1"), actorVersion("0.5.2")}, true},
		{"only the actor below", []func(*HostIdentity){agentVersion("0.5.2"), actorVersion("0.4.1")}, true},
		{"a prerelease of the floor orders below it", []func(*HostIdentity){agentVersion("0.5.0-rc.2")}, true},
		{"above", []func(*HostIdentity){agentVersion("0.6.0"), actorVersion("0.5.1")}, false},
		{"an unstamped build is never below", []func(*HostIdentity){agentVersion("dev"), actorVersion("0.5.0")}, false},
		{"a leading v is not the grammar, so it is not judged", []func(*HostIdentity){agentVersion("v0.1.0")}, false},
		{"no actor version reported", []func(*HostIdentity){agentVersion("0.5.0"), func(h *HostIdentity) { h.RecoveryActorVersion = nil }}, false},
		{"no agent version reported", []func(*HostIdentity){func(h *HostIdentity) { h.AgentVersion = nil }}, false},
	} {
		h := host("h1", "gpu-01", commitA, append([]func(*HostIdentity){ownedInstall}, tc.opts...)...)
		if got := HostBelowFloor(h, testFloor); got != tc.want {
			t.Errorf("%s: below = %v, want %v", tc.name, got, tc.want)
		}
	}
	// A control plane that declares no floor judges nothing.
	if HostBelowFloor(host("h1", "gpu-01", commitA, agentVersion("0.0.1")), buildinfo.Floor{}) {
		t.Error("the zero floor put a host below it")
	}
}

// The read signal is served on every host and changes no target: a below-floor host is
// still offered the update to available[0], which is what it must take.
func TestPlanServesBelowFloorAndOffersOnlyTheUpdate(t *testing.T) {
	newest := rel("newest", "0.6.0", commitB, 74, at(3))
	below := host("h1", "gpu-01", commitA, ownedInstall, agentVersion("0.4.1"), actorVersion("0.4.1"))
	fine := host("h2", "gpu-02", commitA, ownedInstall, agentVersion("0.5.2"), actorVersion("0.5.2"))
	v := PlanRelease(PlanInputs{
		Channel:        ChannelStable,
		ControlPlane:   cp(commitB, 74),
		Floor:          testFloor,
		Hosts:          []HostIdentity{below, fine},
		Releases:       []Release{newest},
		UpdaterPresent: true,
	})
	if !v.Installed.Hosts[0].BelowFloor || v.Installed.Hosts[1].BelowFloor {
		t.Fatalf("below_floor = %v / %v, want true / false", v.Installed.Hosts[0].BelowFloor, v.Installed.Hosts[1].BelowFloor)
	}
	for _, target := range v.Targets[1:] {
		if !target.Eligible || target.Reason != nil {
			t.Errorf("host %s: target %+v, want eligible for the update", *target.NodeName, target)
		}
	}

	raw, err := json.Marshal(v.Installed.Hosts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"below_floor":`) != 2 {
		t.Errorf("below_floor is not serialized on every host: %s", raw)
	}
	if strings.Contains(string(raw), "recovery_actor_version") {
		t.Errorf("the frozen identity shape gained a field: %s", raw)
	}
}

func TestRevertIsRefusedBelowTheFloor(t *testing.T) {
	old := digestOld
	cpID := cp(commitB, 40)
	cpRel := relPtr(rel("rel-cp", "0.6.0", commitB, 40, at(4)))

	got := PlanRevert(RevertInputs{
		LastSucceeded:       succeededApply(&old),
		PreviousRelease:     relPtr(rel("rel-old", "0.5.1", commitA, 40, at(1))),
		ControlPlaneRelease: cpRel,
		ControlPlane:        cpID,
		HostBelowFloor:      true,
		Floor:               testFloor,
	})
	if got.OK || got.Code != CodeHostNotEligible || got.Reason != ReasonBelowFloor {
		t.Errorf("a host already below the floor: %+v, want host_not_eligible/below_floor", got)
	}

	got = PlanRevert(RevertInputs{
		LastSucceeded:       succeededApply(&old),
		PreviousRelease:     relPtr(rel("rel-old", "0.4.9", commitA, 40, at(1))),
		ControlPlaneRelease: cpRel,
		ControlPlane:        cpID,
		Floor:               testFloor,
	})
	if got.OK || got.Reason != ReasonBelowFloor {
		t.Errorf("a revert onto a release below the floor: %+v, want below_floor", got)
	}

	got = PlanRevert(RevertInputs{
		LastSucceeded:       succeededApply(&old),
		PreviousRelease:     relPtr(rel("rel-old", "0.5.0", commitA, 40, at(1))),
		ControlPlaneRelease: cpRel,
		ControlPlane:        cpID,
		Floor:               testFloor,
	})
	if !got.OK {
		t.Errorf("a revert onto the floor release: %+v, want ok", got)
	}

	// An edge build carries no version, so it is never below the floor.
	edge := rel("rel-edge", "", commitA, 40, at(1), onEdge)
	got = PlanRevert(RevertInputs{
		LastSucceeded:   succeededApply(&old),
		PreviousRelease: &edge,
		ControlPlane:    cpID,
		Floor:           testFloor,
	})
	if !got.OK {
		t.Errorf("a revert onto an edge build: %+v, want ok", got)
	}
}
