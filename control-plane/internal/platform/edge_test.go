package platform

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// fakeInspector answers per image ref. The OCI wire itself is covered by
// internal/images (config_test.go, an httptest registry); what is exercised
// here are the rules that turn two images into one edge build.
type fakeInspector struct {
	byRef map[string]images.ImageConfig
	err   error
	seen  []string
}

func (f *fakeInspector) InspectConfig(_ context.Context, ref string) (images.ImageConfig, error) {
	f.seen = append(f.seen, ref)
	if f.err != nil {
		return images.ImageConfig{}, f.err
	}
	cfg, ok := f.byRef[ref]
	if !ok {
		return images.ImageConfig{}, errors.New("no such image")
	}
	return cfg, nil
}

const (
	digestCP    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestAgent = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func labels(commit, built, schema string) map[string]string {
	m := map[string]string{LabelSourceCommit: commit, LabelBuiltAt: built}
	if schema != "" {
		m[LabelSchemaVersion] = schema
	}
	return m
}

// twoImages is a well-formed branch build of both components.
func twoImages(tag, commit, schema string) *fakeInspector {
	return &fakeInspector{byRef: map[string]images.ImageConfig{
		"ghcr.io/accreleus/quasar/quasar-control-plane:" + tag: {
			ManifestDigest: digestCP,
			Labels:         labels(commit, "2026-09-04T12:00:00Z", schema),
		},
		"ghcr.io/accreleus/quasar/quasar-node-agent:" + tag: {
			ManifestDigest: digestAgent,
			Labels:         labels(commit, "2026-09-04T12:00:00Z", ""),
		},
	}}
}

func TestEdgeResolveReadsBothImages(t *testing.T) {
	inspect := twoImages("develop", commitA, "76")
	src := NewRegistryEdgeSource(inspect, "", "")

	build, err := src.Resolve(context.Background(), "develop")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.SourceCommit != commitA || build.SchemaVersion != 76 || build.Tag != "develop" {
		t.Fatalf("build = %+v", build)
	}
	if build.BuiltAt.Format("2006-01-02T15:04:05Z") != "2026-09-04T12:00:00Z" {
		t.Fatalf("built_at = %v", build.BuiltAt)
	}
	if len(build.Components) != 2 ||
		build.Components[0].Name != "control-plane" || build.Components[0].Digest != digestCP ||
		build.Components[1].Name != "node-agent" || build.Components[1].Digest != digestAgent {
		t.Fatalf("components = %+v", build.Components)
	}
	// The component image is a repository name with no tag: a tag is never an
	// identity (ADR 0001), and the digest beside it is.
	if build.Components[0].Image != "ghcr.io/accreleus/quasar/quasar-control-plane" {
		t.Fatalf("image = %q", build.Components[0].Image)
	}
}

func TestEdgeResolveComponentsDisagree(t *testing.T) {
	inspect := twoImages("develop", commitA, "76")
	agent := inspect.byRef["ghcr.io/accreleus/quasar/quasar-node-agent:develop"]
	agent.Labels = labels(commitB, "2026-09-04T12:00:00Z", "")
	inspect.byRef["ghcr.io/accreleus/quasar/quasar-node-agent:develop"] = agent

	_, err := NewRegistryEdgeSource(inspect, "", "").Resolve(context.Background(), "develop")
	if !errors.Is(err, ErrEdgeComponentsDisagree) {
		t.Fatalf("err = %v, want ErrEdgeComponentsDisagree", err)
	}
}

func TestEdgeResolveMissingSchemaLabelIsUnknown(t *testing.T) {
	inspect := twoImages("develop", commitA, "")
	_, err := NewRegistryEdgeSource(inspect, "", "").Resolve(context.Background(), "develop")
	if !errors.Is(err, ErrEdgeSchemaUnknown) {
		t.Fatalf("err = %v, want ErrEdgeSchemaUnknown", err)
	}

	// A label that is present but not a positive integer is the same answer.
	inspect = twoImages("develop", commitA, "not-a-number")
	if _, err := NewRegistryEdgeSource(inspect, "", "").Resolve(context.Background(), "develop"); !errors.Is(err, ErrEdgeSchemaUnknown) {
		t.Fatalf("err = %v, want ErrEdgeSchemaUnknown", err)
	}
}

func TestEdgeResolveMissingCommitLabelFails(t *testing.T) {
	inspect := twoImages("develop", commitA, "76")
	cfg := inspect.byRef["ghcr.io/accreleus/quasar/quasar-control-plane:develop"]
	cfg.Labels = map[string]string{}
	inspect.byRef["ghcr.io/accreleus/quasar/quasar-control-plane:develop"] = cfg

	_, err := NewRegistryEdgeSource(inspect, "", "").Resolve(context.Background(), "develop")
	if err == nil || !strings.Contains(err.Error(), LabelSourceCommit) {
		t.Fatalf("err = %v, want a %s complaint", err, LabelSourceCommit)
	}
}

// A branch with a slash is published under a sanitized tag, so the resolver
// must ask for that tag and not the branch name.
func TestEdgeResolveTagMapping(t *testing.T) {
	inspect := twoImages("feature-x", commitA, "76")
	build, err := NewRegistryEdgeSource(inspect, "", "").Resolve(context.Background(), "feature/x")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Tag != "feature-x" {
		t.Fatalf("tag = %q, want feature-x", build.Tag)
	}
	if len(inspect.seen) == 0 || !strings.HasSuffix(inspect.seen[0], ":feature-x") {
		t.Fatalf("requested %v, want a :feature-x ref", inspect.seen)
	}
}

func TestBranchTag(t *testing.T) {
	for _, tc := range []struct{ branch, want string }{
		{"develop", "develop"},
		{"feature/x", "feature-x"},
		{"feat/111-edge-channel", "feat-111-edge-channel"},
		{"release/v1.2", "release-v1.2"},
		{"a b", "a-b"},
		{"-lead", "lead"},
		{"", ""},
	} {
		if got := BranchTag(tc.branch); got != tc.want {
			t.Errorf("BranchTag(%q) = %q, want %q", tc.branch, got, tc.want)
		}
	}
}

func TestEdgeSourceRegistryOverride(t *testing.T) {
	src := NewRegistryEdgeSource(nil, "registry.example.com", "acme/quasar")
	if got := src.ImageRef("quasar-control-plane", "develop"); got != "registry.example.com/acme/quasar/quasar-control-plane:develop" {
		t.Fatalf("ImageRef = %q", got)
	}
}
