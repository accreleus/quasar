package platform

import "context"

// The preview channel's source (amendment 48): one document at the preview tag
// of quasar-release-manifest, in the registry and repository edge reads.
const (
	PreviewManifestImage = "quasar-release-manifest"
	PreviewTag           = "preview"
)

// PreviewSource reads the preview manifest. An error wrapping
// images.ErrRegistryNotFound is "not yet published"; images.ErrArtifactShape is an
// invalid document; anything else is an unreachable registry.
type PreviewSource interface {
	FetchPreview(ctx context.Context) ([]byte, error)
}

// ArtifactFetcher is images.RegistryResolver's FetchArtifact.
type ArtifactFetcher interface {
	FetchArtifact(ctx context.Context, ref string) ([]byte, error)
}

// RegistryPreviewSource reads the preview tag from the platform registry.
type RegistryPreviewSource struct {
	fetch ArtifactFetcher
	ref   string
}

// NewRegistryPreviewSource builds the source; registry and repo fall back to the
// defaults as NewRegistryEdgeSource's do.
func NewRegistryPreviewSource(fetch ArtifactFetcher, registry, repo string) *RegistryPreviewSource {
	return &RegistryPreviewSource{
		fetch: fetch,
		ref:   NewRegistryEdgeSource(nil, registry, repo).ImageRef(PreviewManifestImage, PreviewTag),
	}
}

func (s *RegistryPreviewSource) FetchPreview(ctx context.Context) ([]byte, error) {
	return s.fetch.FetchArtifact(ctx, s.ref)
}
