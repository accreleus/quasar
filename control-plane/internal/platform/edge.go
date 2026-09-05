package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// The edge channel's source (#111). A branch publishes no release and no
// manifest asset, so an edge build's identity is read off the images a branch
// tag points at, from the labels #107 stamps on them.
// Knobs: docs/configuration.md (QUASAR_PLATFORM_REGISTRY).

const (
	DefaultPlatformRegistry = "ghcr.io"

	// Asserted by deploy/image-contract.json. LabelSchemaVersion is on the
	// CONTROL-PLANE image only — schema_version is a property of the binary
	// that runs migrations.
	LabelSourceCommit  = "org.quasar.source.commit"
	LabelBuiltAt       = "org.quasar.built.at"
	LabelSchemaVersion = "org.quasar.schema.version"
)

// edgeComponent is one component image. Names and ORDER match
// manifestComponents (manifest.go): control plane first (ADR 0002).
type edgeComponent struct {
	Name  string // "control-plane"
	Image string // the repository's last path element
}

var edgeComponents = [2]edgeComponent{
	{Name: "control-plane", Image: "quasar-control-plane"},
	{Name: "node-agent", Image: "quasar-node-agent"},
}

// EdgeBuild is one resolved edge build: what the branch tag pointed at, at the
// moment it was read.
type EdgeBuild struct {
	// Tag is the branch tag that was resolved (BranchTag of the branch).
	Tag string
	// SourceCommit is the commit BOTH images agree on; a disagreement is never
	// resolved to one of them (ErrEdgeComponentsDisagree).
	SourceCommit string
	// BuiltAt comes from the control-plane image; both are built in one run.
	BuiltAt time.Time
	// SchemaVersion is the control-plane image's LabelSchemaVersion, never
	// guessed: without it the build is unorderable (ErrEdgeSchemaUnknown).
	SchemaVersion int
	// Components are the digests the tag resolved to. NOT stored on the row:
	// `platform_releases.manifest` is NULL on edge (schema.md). They go in the
	// detection run's summary instead.
	Components []ManifestComponent
}

// EdgeResolver resolves the current build of one branch. The interface exists
// so the detector runs against a fake instead of a live registry.
type EdgeResolver interface {
	Resolve(ctx context.Context, branch string) (EdgeBuild, error)
}

// ErrEdgeSchemaUnknown: no readable LabelSchemaVersion, so the build cannot be
// ordered (the ADR 0002 key). The detector skips it and the run still
// succeeds — an image built before the label existed is not a failure.
var ErrEdgeSchemaUnknown = errors.New("the edge control-plane image carries no " + LabelSchemaVersion + " label")

// ErrEdgeComponentsDisagree: the two images on the tag were built from
// different commits (a half-finished publish), so the build has no identity and
// none is stored. NOT a PlatformReleaseFault — that vocabulary is closed in
// openapi.yaml; it surfaces as a failed run, i.e. the view's `last_error`.
var ErrEdgeComponentsDisagree = errors.New("the edge component images were built from different commits")

// RegistryEdgeSource resolves a branch build from the container registry.
type RegistryEdgeSource struct {
	inspect  images.ImageInspector
	registry string // e.g. ghcr.io
	repo     string // e.g. accreleus/quasar
}

// NewRegistryEdgeSource builds the source. registry and repo fall back to the
// documented defaults when blank.
func NewRegistryEdgeSource(inspect images.ImageInspector, registry, repo string) *RegistryEdgeSource {
	registry = strings.Trim(strings.TrimSpace(registry), "/")
	if registry == "" {
		registry = DefaultPlatformRegistry
	}
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo == "" {
		repo = DefaultReleaseRepo
	}
	return &RegistryEdgeSource{inspect: inspect, registry: registry, repo: repo}
}

// ConfiguredPlatformRegistry reads QUASAR_PLATFORM_REGISTRY. Unlike the release
// repo, empty is not a disable switch: it falls back to the default.
func ConfiguredPlatformRegistry() string {
	if v := strings.TrimSpace(os.Getenv("QUASAR_PLATFORM_REGISTRY")); v != "" {
		return strings.Trim(v, "/")
	}
	return DefaultPlatformRegistry
}

// ImageRef is the full reference of one component image on a tag.
func (e *RegistryEdgeSource) ImageRef(component, tag string) string {
	return e.ImageName(component) + ":" + tag
}

// ImageName is the repository name with no tag, the form
// `ReleaseManifest.image` uses (a tag is never an identity — ADR 0001).
func (e *RegistryEdgeSource) ImageName(component string) string {
	return e.registry + "/" + e.repo + "/" + component
}

// Resolve reads both component images on the branch tag.
func (e *RegistryEdgeSource) Resolve(ctx context.Context, branch string) (EdgeBuild, error) {
	tag := BranchTag(branch)
	if tag == "" {
		return EdgeBuild{}, fmt.Errorf("edge branch %q maps to no usable image tag", branch)
	}
	build := EdgeBuild{Tag: tag, Components: make([]ManifestComponent, 0, len(edgeComponents))}

	for i, comp := range edgeComponents {
		ref := e.ImageRef(comp.Image, tag)
		cfg, err := e.inspect.InspectConfig(ctx, ref)
		if err != nil {
			return EdgeBuild{}, fmt.Errorf("edge image %s: %w", ref, err)
		}
		commit := strings.ToLower(cfg.Label(LabelSourceCommit))
		if !fullCommitRe.MatchString(commit) {
			return EdgeBuild{}, fmt.Errorf("edge image %s: %s is %q, not 40 lowercase hex",
				ref, LabelSourceCommit, cfg.Label(LabelSourceCommit))
		}
		if i == 0 {
			build.SourceCommit = commit
			built, err := time.Parse(time.RFC3339, cfg.Label(LabelBuiltAt))
			if err != nil {
				return EdgeBuild{}, fmt.Errorf("edge image %s: %s %q is not RFC3339: %w",
					ref, LabelBuiltAt, cfg.Label(LabelBuiltAt), err)
			}
			build.BuiltAt = built.UTC()
			schema, err := parseSchemaLabel(cfg.Label(LabelSchemaVersion))
			if err != nil {
				return EdgeBuild{}, fmt.Errorf("edge image %s: %w", ref, err)
			}
			build.SchemaVersion = schema
		} else if commit != build.SourceCommit {
			return EdgeBuild{}, fmt.Errorf("%w: %s is %s while %s is %s (tag %q)",
				ErrEdgeComponentsDisagree, edgeComponents[0].Name, shortCommit(build.SourceCommit),
				comp.Name, shortCommit(commit), tag)
		}
		if cfg.ManifestDigest == "" {
			return EdgeBuild{}, fmt.Errorf("edge image %s resolved to no digest", ref)
		}
		build.Components = append(build.Components, ManifestComponent{
			Name:   comp.Name,
			Image:  e.ImageName(comp.Image),
			Digest: cfg.ManifestDigest,
		})
	}
	return build, nil
}

// parseSchemaLabel: absent and unreadable are the same answer, unknown.
func parseSchemaLabel(raw string) (int, error) {
	if raw == "" {
		return 0, ErrEdgeSchemaUnknown
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w (it reads %q)", ErrEdgeSchemaUnknown, raw)
	}
	return n, nil
}

// BranchTag maps a branch to the tag it is published under: twin of
// docker/metadata-action's `type=ref,event=branch` in
// .github/workflows/images.yml, which replaces every character outside
// [A-Za-z0-9._-] with "-" (so `feature/x` is `feature-x`).
func BranchTag(branch string) string {
	branch = strings.TrimSpace(branch)
	var b strings.Builder
	for _, c := range branch {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	// A tag may not begin with a separator; a branch legitimately can.
	return strings.TrimLeft(b.String(), ".-")
}

// shortCommit is the operator-prose form of a commit.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
