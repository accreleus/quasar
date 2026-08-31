package images

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Fetcher retrieves the raw manifest bytes for a given catalog ref
// (instance_settings.image_catalog_ref — a branch, tag, or commit in
// accreleus/quasar-images). It is an interface so tests can inject the
// local fixture (control-plane/internal/images/testdata/manifest-v1.json)
// instead of making a live network call.
type Fetcher interface {
	Fetch(ctx context.Context, ref string) ([]byte, error)
}

// FetchFunc adapts a plain function to Fetcher.
type FetchFunc func(ctx context.Context, ref string) ([]byte, error)

func (f FetchFunc) Fetch(ctx context.Context, ref string) ([]byte, error) { return f(ctx, ref) }

// DefaultManifestRepo is "owner/name" of the manifest repo. Overridable via
// QUASAR_IMAGE_CATALOG_REPO — see NewHTTPFetcher.
const DefaultManifestRepo = "accreleus/quasar-images"

// DefaultManifestPath is where the manifest lives inside the repo (verified
// against the real repo: `quasar-manifest.json`, not `manifest.json`).
// Overridable via QUASAR_IMAGE_CATALOG_PATH.
const DefaultManifestPath = "quasar-manifest.json"

// DefaultFetchTimeout bounds a single manifest fetch so a slow/unreachable
// GitHub can never hang an admin's sync request or a scheduled sync.
const DefaultFetchTimeout = 15 * time.Second

// ConfiguredCatalogRepo reads QUASAR_IMAGE_CATALOG_REPO, defaulting to
// DefaultManifestRepo. Used by every template-context consumer (template.go's
// sha resolver, ensure.go's codeload URL builder) that shouldn't need a live
// *HTTPFetcher to learn the repo.
func ConfiguredCatalogRepo() string {
	if repo := os.Getenv("QUASAR_IMAGE_CATALOG_REPO"); repo != "" {
		return repo
	}
	return DefaultManifestRepo
}

// ConfiguredCatalogPath is the "which file" half of ConfiguredCatalogRepo,
// reading QUASAR_IMAGE_CATALOG_PATH with the same default NewHTTPFetcher uses.
func ConfiguredCatalogPath() string {
	if path := os.Getenv("QUASAR_IMAGE_CATALOG_PATH"); path != "" {
		return path
	}
	return DefaultManifestPath
}

// ManifestURL builds the raw-content URL a manifest fetch targets. It is the
// SQL-twin-style pairing for Fetch below: both must produce the same URL, and
// the #548 provenance record stores what this returns as the fetch URL an
// operator sees.
func ManifestURL(repo, path, ref string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, ref, path)
}

// RepoNamer is implemented by a Fetcher that knows its own configured repo —
// the seam Store.resolveTemplateContexts uses to resolve a template's context
// sha against the SAME repo the manifest itself was fetched from, rather than
// a possibly-stale compiled-in default. *HTTPFetcher implements it; a test
// fixture fetcher (FetchFunc, or a package test's fixtureFetcher) does not,
// and template context resolution then falls back to ConfiguredCatalogRepo().
type RepoNamer interface {
	RepoName() string
}

// RepoName implements RepoNamer.
func (f *HTTPFetcher) RepoName() string {
	if f.Repo != "" {
		return f.Repo
	}
	return DefaultManifestRepo
}

// URLNamer is implemented by a Fetcher that can name the URL it would fetch a
// given ref from — the seam Store.Sync uses to record the #548 provenance URL
// without a live *HTTPFetcher. A test fixture fetcher does not implement it,
// and the Store falls back to the configured repo/path.
type URLNamer interface {
	ManifestURL(ref string) string
}

// ManifestURL implements URLNamer.
func (f *HTTPFetcher) ManifestURL(ref string) string {
	path := f.Path
	if path == "" {
		path = DefaultManifestPath
	}
	return ManifestURL(f.RepoName(), path, ref)
}

// HTTPFetcher fetches the manifest from the quasar-images repo's raw content
// URL at a given ref. Never fails a launch: Store.Sync treats any error as
// "serve the cached catalog, report sync_error", never a 5xx.
type HTTPFetcher struct {
	Client *http.Client // defaults to a client with DefaultFetchTimeout if nil
	Repo   string       // "owner/name"; defaults to DefaultManifestRepo, overridable via QUASAR_IMAGE_CATALOG_REPO
	Path   string       // manifest path within the repo; defaults to DefaultManifestPath, overridable via QUASAR_IMAGE_CATALOG_PATH
}

// NewHTTPFetcher builds the production fetcher against
// accreleus/quasar-images's raw.githubusercontent.com content, or an
// operator override (QUASAR_IMAGE_CATALOG_REPO/_PATH, docs/configuration.md).
// instance_settings.image_catalog_ref (the branch/tag/commit) stays a
// separate DB-side admin-editable knob — these two env vars are the
// deploy-time "which repo/file" it resolves against.
func NewHTTPFetcher() *HTTPFetcher {
	repo := os.Getenv("QUASAR_IMAGE_CATALOG_REPO")
	if repo == "" {
		repo = DefaultManifestRepo
	}
	path := os.Getenv("QUASAR_IMAGE_CATALOG_PATH")
	if path == "" {
		path = DefaultManifestPath
	}
	return &HTTPFetcher{
		Client: &http.Client{Timeout: DefaultFetchTimeout},
		Repo:   repo,
		Path:   path,
	}
}

// Fetch retrieves the manifest at ref (a branch, tag, or commit SHA) via
// raw.githubusercontent.com. The context bounds the request in addition to
// the client's own timeout, so a caller-supplied deadline is always honored.
func (f *HTTPFetcher) Fetch(ctx context.Context, ref string) ([]byte, error) {
	if ref == "" {
		ref = "stable"
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultFetchTimeout}
	}
	repo := f.Repo
	if repo == "" {
		repo = DefaultManifestRepo
	}
	path := f.Path
	if path == "" {
		path = DefaultManifestPath
	}
	url := ManifestURL(repo, path, ref)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build manifest fetch request for %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Names the URL and the two operator knobs, not a bare status code.
		return nil, fmt.Errorf(
			"fetch manifest %s: unexpected status %d (check that ref %q, repo %q, and path %q are correct — "+
				"repo/path are QUASAR_IMAGE_CATALOG_REPO/_PATH at deploy time, ref is the admin-editable "+
				"instance_settings.image_catalog_ref)",
			url, resp.StatusCode, ref, repo, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // bounded: a misconfigured ref must not stream unbounded
	if err != nil {
		return nil, fmt.Errorf("read manifest body from %s: %w", url, err)
	}
	return body, nil
}
