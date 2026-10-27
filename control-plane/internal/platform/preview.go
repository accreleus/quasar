package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/accreleus/quasar/control-plane/internal/images"
)

// The preview channel's source (amendment 48): one document at the preview tag
// of quasar-release-manifest, in the registry and repository edge reads.
const (
	PreviewManifestImage = "quasar-release-manifest"
	PreviewTag           = "preview"
)

// PreviewSource reads the preview manifest and the images it names. From
// FetchPreview, an error wrapping images.ErrRegistryNotFound is "not published";
// images.ErrArtifactShape is an invalid document; anything else is an unreachable
// registry.
type PreviewSource interface {
	FetchPreview(ctx context.Context) ([]byte, error)
	images.ImageInspector
}

// PreviewRegistry is what images.RegistryResolver provides the preview source.
type PreviewRegistry interface {
	FetchArtifact(ctx context.Context, ref string) ([]byte, error)
	images.ImageInspector
}

// RegistryPreviewSource reads the preview tag from the platform registry.
type RegistryPreviewSource struct {
	PreviewRegistry
	ref string
}

// NewRegistryPreviewSource builds the source; registry and repo fall back to the
// defaults as NewRegistryEdgeSource's do.
func NewRegistryPreviewSource(reg PreviewRegistry, registry, repo string) *RegistryPreviewSource {
	return &RegistryPreviewSource{
		PreviewRegistry: reg,
		// <registry>/<repo>/quasar-release-manifest:preview; the tag is literal, not a branch tag.
		ref: NewRegistryEdgeSource(nil, registry, repo).ImageRef(PreviewManifestImage, PreviewTag),
	}
}

func (s *RegistryPreviewSource) FetchPreview(ctx context.Context) ([]byte, error) {
	return s.FetchArtifact(ctx, s.ref)
}

// errComponentCommit: a component image was not built from the manifest's
// source_commit, or is not where the manifest says. The manifest is invalid.
var errComponentCommit = errors.New("a component image disagrees with the manifest")

// verifyComponentCommits checks "every component was built from source_commit"
// off each image's label, as edge does. Any other inspection error fails the run.
func verifyComponentCommits(ctx context.Context, inspect images.ImageInspector, m Manifest) error {
	for _, c := range m.Components {
		ref := c.Image + "@" + c.Digest
		cfg, err := inspect.InspectConfig(ctx, ref)
		switch {
		case errors.Is(err, images.ErrRegistryNotFound), errors.Is(err, images.ErrDigestMismatch):
			return fmt.Errorf("%w: %s: %v", errComponentCommit, ref, err)
		case err != nil:
			return fmt.Errorf("inspect %s: %w", ref, err)
		}
		if got := labelCommit(cfg); got != m.SourceCommit {
			return fmt.Errorf("%w: %s %s is %q, the manifest's source_commit is %s",
				errComponentCommit, ref, LabelSourceCommit, got, m.SourceCommit)
		}
	}
	return nil
}
