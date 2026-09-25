package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// labelled answers a config per reference, recording which inspector ran.
type labelled struct {
	name   string
	calls  *[]string
	labels map[string]map[string]string
}

func (l labelled) InspectConfig(_ context.Context, ref string) (images.ImageConfig, error) {
	*l.calls = append(*l.calls, l.name+" "+ref)
	lab, ok := l.labels[ref]
	if !ok {
		return images.ImageConfig{}, errors.New("manifest unknown")
	}
	return images.ImageConfig{Labels: lab}, nil
}

func TestTheIdentityReadRoutesOnlyTheNamedPlainRegistriesOverPlainHTTP(t *testing.T) {
	var calls []string
	lab := map[string]map[string]string{
		"registry.lan:5000/dev/quasar-node-agent@" + devAgentDigest:  {LabelSourceCommit: commitB},
		"ghcr.io/accreleus/quasar/quasar-recovery@" + devActorDigest: {LabelSourceCommit: strings.ToUpper(commitB)},
	}
	routed := RoutedInspector{
		Plain:      labelled{"plain", &calls, lab},
		PlainHosts: map[string]struct{}{"registry.lan:5000": {}},
		TLS:        labelled{"tls", &calls, lab},
	}
	commit, err := NewRegistryDeveloperImages(routed).Commit(context.Background(), []ComponentDigest{
		{Name: ComponentRecovery, Image: "ghcr.io/accreleus/quasar/quasar-recovery", Digest: devActorDigest},
		{Name: ComponentNodeAgent, Image: "registry.lan:5000/dev/quasar-node-agent", Digest: devAgentDigest},
	})
	if err != nil || commit != commitB {
		t.Fatalf("commit = %q, %v", commit, err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "tls ghcr.io/") || !strings.HasPrefix(calls[1], "plain registry.lan:5000/") {
		t.Fatalf("routing = %v", calls)
	}
}

func TestTheIdentityReadRefusesMissingDisagreeingOrPartialCommits(t *testing.T) {
	var calls []string
	agent := devAgentImage + "@" + devAgentDigest
	actor := devActorImage + "@" + devActorDigest
	for name, lab := range map[string]map[string]map[string]string{
		"unresolvable": {agent: {LabelSourceCommit: commitB}},
		"no label":     {agent: {LabelSourceCommit: commitB}, actor: {}},
		"short commit": {agent: {LabelSourceCommit: commitB}, actor: {LabelSourceCommit: commitB[:12]}},
		"disagree":     {agent: {LabelSourceCommit: commitB}, actor: {LabelSourceCommit: commitA}},
	} {
		d := NewRegistryDeveloperImages(labelled{"tls", &calls, lab})
		if _, err := d.Commit(context.Background(), []ComponentDigest{agentComponent(), actorComponent()}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDeveloperCommitAllowedIsTheControlPlanesOwnOrAKnownReleaseAtOrBelowIt(t *testing.T) {
	v := View{Channel: ChannelStable, Installed: Installed{ControlPlane: cp(commitB, 50)}}
	if !DeveloperCommitAllowed(commitB, v, nil) {
		t.Error("the control plane's own commit refused")
	}
	if DeveloperCommitAllowed(commitA, v, nil) {
		t.Error("an unknown commit allowed")
	}
	below := &Release{Channel: ChannelStable, SourceCommit: commitA, SchemaVersion: 49}
	if !DeveloperCommitAllowed(commitA, v, below) {
		t.Error("a known release below the control plane refused")
	}
	above := &Release{Channel: ChannelStable, SourceCommit: commitA, SchemaVersion: 51}
	if DeveloperCommitAllowed(commitA, v, above) {
		t.Error("a known release above the control plane allowed")
	}
}

func TestValidateDeveloperApplyOrdersTheActorFirst(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	out, err := ValidateDeveloperApply(DeveloperApplyRequest{Target: TargetHost, HostID: &id,
		Components: []ComponentDigest{agentComponent(), actorComponent()}})
	if err != nil || out[0].Name != ComponentRecovery || out[1].Name != ComponentNodeAgent {
		t.Fatalf("out = %+v, %v", out, err)
	}
	out, err = ValidateDeveloperApply(DeveloperApplyRequest{Target: TargetControlPlane,
		Components: []ComponentDigest{{Name: ComponentControlPlane, Image: devAgentImage, Digest: devAgentDigest}, actorComponent()}})
	if err != nil || out[0].Name != ComponentRecovery {
		t.Fatalf("control plane out = %+v, %v", out, err)
	}
	if got := NamespaceHosts([]string{"ghcr.io/accreleus/quasar", "registry.lan:5000/dev", "noregistry"}); len(got) != 2 {
		t.Errorf("namespace hosts = %v", got)
	}
}
