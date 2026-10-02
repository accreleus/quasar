package platform

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Amendment 14 "Components of an apply on an owned machine", ADR 0008: the recovery
// actor moves first inside a host attempt, and success, restore and revert follow it.

const actorRepo = "ghcr.io/accreleus/quasar/quasar-recovery"

func actorDigest(c byte) ComponentDigest {
	return ComponentDigest{Name: ComponentRecovery, Image: actorRepo, Digest: "sha256:" + repeat64(c)}
}

func agentDigest(c byte) ComponentDigest {
	return ComponentDigest{Name: ComponentNodeAgent, Image: agentRepo, Digest: "sha256:" + repeat64(c)}
}

func ownedHost(agentCommit, actorCommit *string) HostIdentity {
	mode := InstallOwned
	return HostIdentity{HostID: testHostID, InstallMode: &mode, SourceCommit: agentCommit, RecoveryActorSourceCommit: actorCommit}
}

func TestHostComponentsAreOrderedActorFirst(t *testing.T) {
	release := []ComponentDigest{agentDigest('a'), actorDigest('b')}
	now, old := testCommit, strings.Repeat("0", 40)
	registry := InstallRegistry
	tests := []struct {
		name  string
		host  HostIdentity
		cpBox bool
		want  []string
	}{
		{"both behind: the actor, then the agent", ownedHost(&old, &old), false, []string{ComponentRecovery, ComponentNodeAgent}},
		{"an actor that reports no commit is not on the release", ownedHost(&old, nil), false, []string{ComponentRecovery, ComponentNodeAgent}},
		{"only the agent behind", ownedHost(&old, &now), false, []string{ComponentNodeAgent}},
		{"only the actor behind", ownedHost(&now, &old), false, []string{ComponentRecovery}},
		{"the control plane's own machine moves its actor in the control-plane step", ownedHost(&old, &old), true, []string{ComponentNodeAgent}},
		{"a registry host is never sent the actor", HostIdentity{InstallMode: &registry, SourceCommit: &old}, false, []string{ComponentNodeAgent}},
	}
	for _, tt := range tests {
		got := OrderHostComponents(release, now, tt.host, tt.cpBox)
		var names []string
		for _, c := range got {
			names = append(names, c.Name)
		}
		if !reflect.DeepEqual(names, tt.want) {
			t.Errorf("%s: %v, want %v", tt.name, names, tt.want)
		}
	}
	// A release that names no actor (a format-1 manifest) sends only the agent.
	got := OrderHostComponents([]ComponentDigest{agentDigest('a')}, now, ownedHost(&old, &old), false)
	if len(got) != 1 || got[0].Name != ComponentNodeAgent {
		t.Errorf("format-1 release: %+v", got)
	}
}

// A request naming only the actor never replaced the agent; one naming both
// succeeds on register only once the actor reports the commit too.
func TestRegisterEvidenceForAnAttemptThatMovesTheActor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		components []ComponentDigest
		actor      *string
		want       string
	}{
		{"actor only: the relayed outcome decides", []ComponentDigest{actorDigest('a')}, strPtr(testCommit), AttemptPending},
		{"both, actor not on the commit yet", []ComponentDigest{actorDigest('a'), agentDigest('b')}, strPtr(strings.Repeat("0", 40)), AttemptPending},
		{"both, the actor reports no commit", []ComponentDigest{actorDigest('a'), agentDigest('b')}, nil, AttemptPending},
		{"both on the commit", []ComponentDigest{actorDigest('a'), agentDigest('b')}, strPtr(testCommit), AttemptSucceeded},
	} {
		a := queuedAttempt(true)
		a.RequestedDigests = tc.components
		store := newFakeStore(a)
		store.actor = tc.actor
		agent := &fakeAgent{ack: Ack{OK: true}}
		r := testRunner(store, agent.deps())
		r.Start(a)
		waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
		if got := agent.sent[0].Components; !reflect.DeepEqual(got, tc.components) {
			t.Fatalf("%s: sent %+v, want the attempt's order", tc.name, got)
		}
		commit := testCommit
		r.HandleRegister(context.Background(), testHostID, &commit)
		r.Close()
		if got := store.snapshot(a.ID).State; got != tc.want {
			t.Errorf("%s: state %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ADR 0004 amendment, "one service per failure": the auto_revert row names only the
// component that was restored.
func TestAnAutoRevertNamesOnlyTheRestoredComponent(t *testing.T) {
	prevActor, prevAgent := "sha256:"+repeat64('c'), "sha256:"+repeat64('d')
	for _, tc := range []struct {
		name  string
		actor *string
		want  string
	}{
		{"the actor moved, the agent was put back", strPtr(testCommit), ComponentNodeAgent},
		{"the actor itself was put back", strPtr(strings.Repeat("0", 40)), ComponentRecovery},
		{"the actor's commit is unknown: logged, not guessed", nil, ""},
	} {
		a := queuedAttempt(true)
		a.RequestedDigests = []ComponentDigest{actorDigest('a'), agentDigest('b')}
		store := newFakeStore(a)
		store.actor = tc.actor
		agent := &fakeAgent{ack: Ack{OK: true}}
		r := testRunner(store, agent.deps())
		r.Start(a)
		waitFor(t, "release_apply to be sent", func() bool { return agent.sentCount() == 1 })
		reason := ReasonUnhealthy
		r.HandleReleaseState(context.Background(), testHostID, ReleaseStateReport{
			RequestID: agent.sent[0].RequestID, State: AttemptFailed, Reason: &reason, Restored: true,
			Previous: []PreviousDigest{{Name: ComponentRecovery, Digest: &prevActor}, {Name: ComponentNodeAgent, Digest: &prevAgent}},
		})
		r.Close()
		rows := store.autoReverts()
		if tc.want == "" {
			if len(rows) != 0 {
				t.Fatalf("%s: a guessed auto_revert row %+v", tc.name, rows)
			}
			continue
		}
		if len(rows) != 1 || len(rows[0].RequestedDigests) != 1 || rows[0].RequestedDigests[0].Name != tc.want {
			t.Fatalf("%s: auto_revert rows %+v, want one naming %s", tc.name, rows, tc.want)
		}
		if len(rows[0].PreviousDigests) != 1 || rows[0].PreviousDigests[0].Name != tc.want {
			t.Fatalf("%s: previous %+v", tc.name, rows[0].PreviousDigests)
		}
	}
}

// A revert runs the other way: the newer actor puts the agent back, then hands
// itself back.
func TestARevertPutsTheAgentBackBeforeTheActor(t *testing.T) {
	old, oldActor := digestOld, "sha256:"+repeat64('7')
	last := succeededApply(&old)
	last.RequestedDigests = []ComponentDigest{actorDigest('a'), {Name: ComponentNodeAgent, Image: agentRepo, Digest: digestNew}}
	last.PreviousDigests = []PreviousDigest{{Name: ComponentRecovery, Digest: &oldActor}, {Name: ComponentNodeAgent, Digest: &old}}
	d := PlanRevert(RevertInputs{LastSucceeded: last, ControlPlane: cp(commitB, 40)})
	if !d.OK {
		t.Fatalf("refused: %+v", d)
	}
	want := []ComponentDigest{
		{Name: ComponentNodeAgent, Image: agentRepo, Digest: old},
		{Name: ComponentRecovery, Image: actorRepo, Digest: oldActor},
	}
	if !reflect.DeepEqual(d.Requested, want) {
		t.Fatalf("requested %+v, want %+v", d.Requested, want)
	}
	// A last attempt that moved only the actor reverts only the actor.
	actorOnly := succeededApply(&old)
	actorOnly.RequestedDigests = []ComponentDigest{actorDigest('a')}
	actorOnly.PreviousDigests = []PreviousDigest{{Name: ComponentRecovery, Digest: &oldActor}}
	if d := PlanRevert(RevertInputs{LastSucceeded: actorOnly, ControlPlane: cp(commitB, 40)}); !d.OK ||
		!reflect.DeepEqual(d.Requested, []ComponentDigest{{Name: ComponentRecovery, Image: actorRepo, Digest: oldActor}}) {
		t.Fatalf("actor-only revert: %+v", d)
	}
	// With the agent's previous digest unknown, nothing is reverted, whatever the
	// actor's is.
	last.PreviousDigests = []PreviousDigest{{Name: ComponentRecovery, Digest: &oldActor}, {Name: ComponentNodeAgent}}
	if d := PlanRevert(RevertInputs{LastSucceeded: last, ControlPlane: cp(commitB, 40)}); d.OK || d.Code != CodeNothingToRevert {
		t.Fatalf("an unknown agent digest: %+v", d)
	}
}
