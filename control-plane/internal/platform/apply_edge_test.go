package platform

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// testLogger keeps a handler's own logging out of test output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func edgeRelease(commit string) Release {
	return Release{ID: testReleaseID, Channel: ChannelEdge, SourceCommit: commit, SchemaVersion: 75}
}

func TestEdgeApplyResolvesTheCommitTagToADigest(t *testing.T) {
	inspect := &fakeInspector{byRef: map[string]images.ImageConfig{
		"ghcr.io/accreleus/quasar/quasar-node-agent:sha-" + commitB[:7]: {
			ManifestDigest: digestAgent,
			Labels:         map[string]string{LabelSourceCommit: commitB},
		},
	}}
	got, err := NewEdgeApplyResolver(inspect, "", "").NodeAgentComponent(context.Background(), edgeRelease(commitB))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Name != ComponentNodeAgent || got.Digest != digestAgent {
		t.Fatalf("component = %+v, want the node agent on its resolved digest", got)
	}
	if got.Image != "ghcr.io/accreleus/quasar/quasar-node-agent" {
		t.Errorf("image = %q, must carry no tag and no digest", got.Image)
	}
}

// A tag that moved must not become an apply of something else.
func TestEdgeApplyRefusesAMismatchedCommitLabel(t *testing.T) {
	inspect := &fakeInspector{byRef: map[string]images.ImageConfig{
		"ghcr.io/accreleus/quasar/quasar-node-agent:sha-" + commitB[:7]: {
			ManifestDigest: digestAgent,
			Labels:         map[string]string{LabelSourceCommit: commitA},
		},
	}}
	if _, err := NewEdgeApplyResolver(inspect, "", "").NodeAgentComponent(context.Background(), edgeRelease(commitB)); err == nil {
		t.Fatal("a mismatched source-commit label was accepted")
	}
}

func TestEdgeApplyRefusesAnUnresolvableRef(t *testing.T) {
	inspect := &fakeInspector{byRef: map[string]images.ImageConfig{}}
	_, err := NewEdgeApplyResolver(inspect, "", "").NodeAgentComponent(context.Background(), edgeRelease(commitB))
	if err == nil {
		t.Fatal("an absent image resolved anyway")
	}
	if len(inspect.seen) != 1 || inspect.seen[0] != "ghcr.io/accreleus/quasar/quasar-node-agent:sha-"+commitB[:7] {
		t.Errorf("looked up %v, want the commit tag", inspect.seen)
	}
}

// A stable release resolves from its manifest and never touches the registry.
func TestStableApplyUsesTheManifestComponent(t *testing.T) {
	raw := json.RawMessage(applyManifest(commitB, 75))
	got := releaseComponents(Release{Manifest: raw})
	if len(got) != 1 || got[0].Name != ComponentNodeAgent {
		t.Fatalf("components = %+v, want the manifest's node agent", got)
	}
}

func TestCommitTagIsTheShortShaForm(t *testing.T) {
	if got := CommitTag(commitB); got != "sha-"+commitB[:7] {
		t.Errorf("CommitTag = %q", got)
	}
	if got := CommitTag("abc"); got != "" {
		t.Errorf("a too-short commit must map to no tag, got %q", got)
	}
}
