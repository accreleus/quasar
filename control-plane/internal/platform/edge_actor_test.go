package platform

import (
	"context"
	"errors"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// An edge build names a recovery-actor component whenever one was published for its
// commit (control-api.md amendment 14, "Release manifest format 2"), so an owned host on
// edge moves its actor first and is up to date only once the actor is on the build.

const digestActor = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

func edgeImages(commit string, withActor bool) *fakeInspector {
	f := &fakeInspector{byRef: map[string]images.ImageConfig{}}
	for image, digest := range map[string]string{"quasar-control-plane": digestCP, "quasar-node-agent": digestAgent, "quasar-recovery": digestActor} {
		if image == "quasar-recovery" && !withActor {
			continue
		}
		cfg := images.ImageConfig{ManifestDigest: digest, Labels: labels(commit, "2026-10-01T12:00:00Z", "96")}
		f.byRef["ghcr.io/accreleus/quasar/"+image+":"+BranchTag("develop")] = cfg
		f.byRef["ghcr.io/accreleus/quasar/"+image+":"+CommitTag(commit)] = cfg
	}
	return f
}

func TestEdgeDetectionNamesTheRecoveryActorWhenPublished(t *testing.T) {
	build, err := NewRegistryEdgeSource(edgeImages(commitA, true), "", "").Resolve(context.Background(), "develop")
	if err != nil {
		t.Fatal(err)
	}
	if len(build.Components) != 3 || build.Components[2].Name != ComponentRecovery || build.Components[2].Digest != digestActor {
		t.Fatalf("components = %+v", build.Components)
	}

	build, err = NewRegistryEdgeSource(edgeImages(commitA, false), "", "").Resolve(context.Background(), "develop")
	if err != nil || len(build.Components) != 2 {
		t.Fatalf("a build with no recovery image: %+v, %v", build, err)
	}

	mixed := edgeImages(commitA, true)
	actor := mixed.byRef["ghcr.io/accreleus/quasar/quasar-recovery:"+BranchTag("develop")]
	actor.Labels = labels(commitB, "2026-10-01T12:00:00Z", "")
	mixed.byRef["ghcr.io/accreleus/quasar/quasar-recovery:"+BranchTag("develop")] = actor
	if _, err := NewRegistryEdgeSource(mixed, "", "").Resolve(context.Background(), "develop"); !errors.Is(err, ErrEdgeComponentsDisagree) {
		t.Fatalf("an actor from another commit: %v, want ErrEdgeComponentsDisagree", err)
	}
}

func TestEdgeApplyResolvesTheRecoveryActorWhenPublished(t *testing.T) {
	ctx := context.Background()
	edge := NewEdgeApplyResolver(edgeImages(commitB, true), "", "")
	got, err := EdgeHostComponents(ctx, edge, edgeRelease(commitB))
	if err != nil || len(got) != 2 || got[1].Digest != digestActor {
		t.Fatalf("host components %v (%v)", got, err)
	}
	got, err = EdgeControlPlaneComponents(ctx, edge, edgeRelease(commitB))
	if err != nil || len(componentNames(got)) != 2 || got[0].Name != ComponentControlPlane || got[1].Name != ComponentRecovery {
		t.Fatalf("control-plane components %v (%v)", got, err)
	}

	// Not published: the agent alone. The ordering then sends an owned host the agent.
	got, err = EdgeHostComponents(ctx, NewEdgeApplyResolver(edgeImages(commitB, false), "", ""), edgeRelease(commitB))
	if err != nil || len(got) != 1 || got[0].Name != ComponentNodeAgent {
		t.Fatalf("no recovery image: %v (%v)", got, err)
	}

	// A registry outage is not "not published".
	outage := &fakeInspector{err: errors.New("registry unreachable")}
	if _, err := EdgeHostComponents(ctx, NewEdgeApplyResolver(outage, "", ""), edgeRelease(commitB)); err == nil {
		t.Fatal("an outage resolved to the agent alone")
	}

	// An owned host whose actor is behind moves the actor first on edge too.
	old := commitA
	both, _ := EdgeHostComponents(ctx, edge, edgeRelease(commitB))
	ordered := OrderHostComponents(both, commitB, ownedHost(&old, &old), false)
	if len(ordered) != 2 || ordered[0].Name != ComponentRecovery {
		t.Fatalf("ordered = %v, want the actor first", componentNames(ordered))
	}
}

// up_to_date on edge accounts for the actor.
func TestAnOwnedHostOnEdgeIsUpToDateOnlyWithItsActor(t *testing.T) {
	newest := rel("edge-b", "", commitB, 74, at(3), onEdge, noManifest)
	actorBehind := host("h1", "gpu-01", commitB, ownedInstall, func(h *HostIdentity) { h.RecoveryActorSourceCommit = str(commitA) })
	actorOn := host("h2", "gpu-02", commitB, ownedInstall, func(h *HostIdentity) { h.RecoveryActorSourceCommit = str(commitB) })
	plan := func(withActor bool) View {
		resolver := NewImageResolver(edgeImages(commitB, withActor), NewEdgeApplyResolver(edgeImages(commitB, withActor), "", ""), 0)
		return PlanRelease(PlanInputs{
			Channel:        ChannelEdge,
			ControlPlane:   cp(commitB, 74),
			Hosts:          []HostIdentity{actorBehind, actorOn},
			Releases:       []Release{newest},
			UpdaterPresent: true,
			ImageFor:       func(r Release) *ImageFact { return resolver.Check(context.Background(), r) },
		})
	}
	v := plan(true)
	if t1 := v.Targets[1]; !t1.Eligible {
		t.Errorf("an owned host whose actor is behind the edge build: %+v, want eligible", t1)
	}
	if t2 := v.Targets[2]; t2.Reason == nil || *t2.Reason != ReasonUpToDate {
		t.Errorf("an owned host fully on the edge build: %+v, want up_to_date", t2)
	}

	// A build that published no recovery image has only the agent to send, and the agent
	// is already on it: offering the host would re-apply that agent again and again.
	v = plan(false)
	for _, tg := range v.Targets[1:] {
		if tg.Reason == nil || *tg.Reason != ReasonUpToDate {
			t.Errorf("%s on a build with no recovery image: %+v, want up_to_date", *tg.NodeName, tg)
		}
	}

	// Nobody resolved the build (no registry egress): nothing but the agent is known.
	v = PlanRelease(PlanInputs{
		Channel: ChannelEdge, ControlPlane: cp(commitB, 74), Hosts: []HostIdentity{actorBehind},
		Releases: []Release{newest}, UpdaterPresent: true,
	})
	if t1 := v.Targets[1]; t1.Reason == nil || *t1.Reason != ReasonUpToDate {
		t.Errorf("an unresolved edge build: %+v, want up_to_date", t1)
	}
}
