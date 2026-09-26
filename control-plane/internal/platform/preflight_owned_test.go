package platform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/actorsocket"
)

// Amendment 14 §"Preflight": an owned target carries owner_conflict and no
// Compose checks, and a failing owner_conflict blocks it (#366).

func checkIDs(p Preflight) []string {
	ids := make([]string, 0, len(p.Checks))
	for _, c := range p.Checks {
		ids = append(ids, c.ID)
	}
	return ids
}

func check(p Preflight, id string) PreflightCheck {
	for _, c := range p.Checks {
		if c.ID == id {
			return c
		}
	}
	return PreflightCheck{}
}

func TestAnOwnedHostTargetCarriesOwnerConflictAndNoComposeChecks(t *testing.T) {
	owned := InstallOwned
	connected := true
	readiness, _ := json.Marshal([]readinessCheckWire{
		{ID: CheckUpdaterSocket, Status: "pass", Summary: "the recovery actor answered"},
		{ID: CheckHealthAddrBindable, Status: "pass", Summary: "ok"},
		{ID: CheckOwnerConflict, Status: "fail",
			Summary:     "quasar-node-agent-1 looks like a Quasar node agent, but this installation did not create it",
			Remediation: "docker rm -f quasar-node-agent-1"},
	})
	h := HostIdentity{InstallMode: &owned, AgentConnected: &connected, Readiness: readiness}
	p := PlanPreflight(TargetHost, HostPreflightFacts(h, &ImageFact{}))

	want := []string{CheckAgentConnected, CheckUpdaterSocket, CheckHealthAddrBindable, CheckOwnerConflict, CheckImageResolvable}
	if got := checkIDs(p); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("checks = %v, want %v", got, want)
	}
	if !p.Blocked() {
		t.Fatalf("state = %s, want blocked", p.State)
	}
	c := check(p, CheckOwnerConflict)
	if !strings.Contains(c.Detail, "quasar-node-agent-1") || !strings.Contains(c.Detail, "docker rm -f") {
		t.Errorf("detail = %q, want the container and the fix", c.Detail)
	}

	// An agent that predates the check: unknown, which never blocks.
	readiness, _ = json.Marshal([]readinessCheckWire{{ID: CheckUpdaterSocket, Status: "pass"}})
	h.Readiness = readiness
	p = PlanPreflight(TargetHost, HostPreflightFacts(h, &ImageFact{}))
	if check(p, CheckOwnerConflict).Status != CheckUnknown || p.Blocked() {
		t.Fatalf("an unreported owner_conflict = %+v (state %s), want unknown and not blocked", check(p, CheckOwnerConflict), p.State)
	}

	// A registry host keeps its Compose checks and carries no owner_conflict.
	registry := InstallRegistry
	h.InstallMode = &registry
	p = PlanPreflight(TargetHost, HostPreflightFacts(h, &ImageFact{}))
	if check(p, CheckOwnerConflict).ID != "" || check(p, CheckUpdaterStackDir).ID == "" {
		t.Fatalf("registry checks = %v", checkIDs(p))
	}
}

func TestAnOwnedControlPlaneTargetIsBlockedByTheActorsConflicts(t *testing.T) {
	fact := &OwnedActorFact{Socket: "/run/quasar-recovery/control.sock", Answered: true, Version: "0.4.0"}
	p := PlanPreflight(TargetControlPlane, PreflightFacts{OwnedActor: fact, Image: &ImageFact{}})
	if check(p, CheckOwnerConflict).Status != CheckPass || p.Blocked() {
		t.Fatalf("no conflicts = %+v (%s)", check(p, CheckOwnerConflict), p.State)
	}

	fact.Conflicts = []actorsocket.Conflict{{
		Container: "deploy-quasar-control-plane-1", ID: "9c41d7e0b2a3",
		Image: "ghcr.io/accreleus/quasar/quasar-control-plane:0.3.0", Role: "control-plane",
		Why: "a Compose service named like the Quasar control plane",
	}}
	p = PlanPreflight(TargetControlPlane, PreflightFacts{OwnedActor: fact, Image: &ImageFact{}})
	c := check(p, CheckOwnerConflict)
	if c.Status != CheckFail || !p.Blocked() || !strings.Contains(c.Detail, "docker rm -f deploy-quasar-control-plane-1") {
		t.Fatalf("a conflict = %+v (%s), want a fail naming the fix", c, p.State)
	}

	fact.Answered = false
	p = PlanPreflight(TargetControlPlane, PreflightFacts{OwnedActor: fact, Image: &ImageFact{}})
	if check(p, CheckOwnerConflict).Status != CheckUnknown {
		t.Fatalf("an actor that did not answer = %+v, want unknown", check(p, CheckOwnerConflict))
	}
}

func TestTheOwnMachineReadCarriesTheActorsConflicts(t *testing.T) {
	st := actorsocket.Status{
		Role:      actorsocket.RoleCombined,
		Conflicts: []actorsocket.Conflict{{Container: "x", Role: "node-agent"}},
	}
	if got := OwnMachineFromStatus(st).Conflicts; len(got) != 1 || got[0].Container != "x" {
		t.Fatalf("conflicts = %+v", got)
	}
}
