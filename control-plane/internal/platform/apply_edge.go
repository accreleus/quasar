package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// An edge release stores `manifest` NULL (schema.md), so it carries no pinned
// node-agent digest. Rather than change the frozen contract, the digest is
// resolved AT APPLY TIME off the commit's `sha-<short>` image tag, which the
// Images workflow publishes for every build in the same promote run as the
// branch tag. ADR 0001 still holds: what is sent is the resolved digest, and
// the tag is only how it was found.

// ApplyComponentResolver resolves a host's component when the release row has
// no manifest. Nil on a build with no registry egress: an edge apply is then
// refused rather than guessed at.
type ApplyComponentResolver interface {
	NodeAgentComponent(ctx context.Context, release Release) (ComponentDigest, error)
}

// EdgeApplyResolver reads the registry.
type EdgeApplyResolver struct {
	inspect  images.ImageInspector
	registry string
	repo     string
}

// NewEdgeApplyResolver builds the resolver; registry and repo fall back to the
// documented defaults when blank, as NewRegistryEdgeSource does.
func NewEdgeApplyResolver(inspect images.ImageInspector, registry, repo string) *EdgeApplyResolver {
	registry = strings.Trim(strings.TrimSpace(registry), "/")
	if registry == "" {
		registry = DefaultPlatformRegistry
	}
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		repo = DefaultReleaseRepo
	}
	return &EdgeApplyResolver{inspect: inspect, registry: registry, repo: repo}
}

// CommitTag is the tag one commit's images are published under: twin of
// docker/metadata-action's `type=sha,prefix=sha-` in
// .github/workflows/images.yml, which uses the 7-character short sha.
func CommitTag(commit string) string {
	commit = strings.ToLower(strings.TrimSpace(commit))
	if len(commit) < 7 {
		return ""
	}
	return "sha-" + commit[:7]
}

// NodeAgentComponent resolves the release's node-agent image to a digest, and
// refuses unless the image's own commit label agrees with the release: a tag
// that moved must not become an apply of something else (ADR 0001).
func (r *EdgeApplyResolver) NodeAgentComponent(ctx context.Context, release Release) (ComponentDigest, error) {
	tag := CommitTag(release.SourceCommit)
	if tag == "" {
		return ComponentDigest{}, fmt.Errorf("release %s has no usable source commit", release.ID)
	}
	image := r.registry + "/" + r.repo + "/" + edgeComponents[1].Image
	ref := image + ":" + tag

	cfg, err := r.inspect.InspectConfig(ctx, ref)
	if err != nil {
		return ComponentDigest{}, fmt.Errorf("%s: %w", ref, err)
	}
	got := strings.ToLower(cfg.Label(LabelSourceCommit))
	if !commitsMatch(got, release.SourceCommit) {
		return ComponentDigest{}, fmt.Errorf("%s: %s is %q, not this release's commit %s",
			ref, LabelSourceCommit, cfg.Label(LabelSourceCommit), shortCommit(release.SourceCommit))
	}
	if !digestRe.MatchString(cfg.ManifestDigest) {
		return ComponentDigest{}, fmt.Errorf("%s resolved to %q, not a sha256 digest", ref, cfg.ManifestDigest)
	}
	return ComponentDigest{Name: ComponentNodeAgent, Image: image, Digest: cfg.ManifestDigest}, nil
}
