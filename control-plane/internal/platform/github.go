package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// GitHub Releases as a ReleaseSource. Knobs: docs/configuration.md
// (QUASAR_PLATFORM_RELEASE_REPO / _API / _TOKEN / _ASSET_HOSTS).

const (
	DefaultReleaseRepo   = "accreleus/quasar"
	DefaultReleaseAPI    = "https://api.github.com"
	defaultAPIHost       = "api.github.com"
	defaultAssetHostList = "github.com,objects.githubusercontent.com"

	// One page is more history than an instance needs; walking them all is an
	// unbounded egress loop.
	releasePageSize = 30

	// A manifest is ~600 bytes; the bound stops a hostile asset host streaming
	// the process out of memory.
	manifestMaxBytes int64 = 1 << 20

	// A slow remote must not stretch an N-release loop into N x forever.
	releaseHTTPTimeout = 10 * time.Second
)

// Doer is the egress surface the source needs: *outbound.Client in production.
// A test needs a fake — an httptest server is loopback and plain http, both of
// which the real client refuses by design.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
	HostAllowed(host string) bool
}

// GitHubSource lists a repository's releases and downloads their manifests.
type GitHubSource struct {
	client  Doer
	apiBase string
	repo    string // "owner/name"
	token   string
}

// NewGitHubSource builds the source. apiBase and repo fall back to the
// documented defaults when blank.
func NewGitHubSource(client Doer, apiBase, repo, token string) *GitHubSource {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultReleaseAPI
	}
	if strings.TrimSpace(repo) == "" {
		repo = DefaultReleaseRepo
	}
	return &GitHubSource{
		client:  client,
		apiBase: strings.TrimRight(strings.TrimSpace(apiBase), "/"),
		repo:    strings.Trim(strings.TrimSpace(repo), "/"),
		token:   strings.TrimSpace(token),
	}
}

// ConfiguredReleaseRepo reads QUASAR_PLATFORM_RELEASE_REPO. An explicitly empty
// value disables detection rather than falling back to the default: an operator
// who blanks it is turning the feature off.
func ConfiguredReleaseRepo() string {
	if v, ok := os.LookupEnv("QUASAR_PLATFORM_RELEASE_REPO"); ok {
		return strings.TrimSpace(v)
	}
	return DefaultReleaseRepo
}

// ConfiguredReleaseAPI reads QUASAR_PLATFORM_RELEASE_API.
func ConfiguredReleaseAPI() string {
	if v := strings.TrimSpace(os.Getenv("QUASAR_PLATFORM_RELEASE_API")); v != "" {
		return v
	}
	return DefaultReleaseAPI
}

// ReleaseEgressHosts is the release client's allowlist: the API host, derived
// from QUASAR_PLATFORM_RELEASE_API so a fork needs no second knob, plus the
// asset hosts (QUASAR_PLATFORM_RELEASE_ASSET_HOSTS), a separate CDN on GitHub.
func ReleaseEgressHosts() map[string]struct{} {
	// ParseHostList's fallback is one host, not a list, so an unset knob is
	// expanded here rather than handed to it whole.
	raw := os.Getenv("QUASAR_PLATFORM_RELEASE_ASSET_HOSTS")
	if strings.TrimSpace(raw) == "" {
		raw = defaultAssetHostList
	}
	hosts := outbound.ParseHostList(raw, defaultAPIHost)
	apiHost := defaultAPIHost
	if u, err := url.Parse(ConfiguredReleaseAPI()); err == nil && u.Hostname() != "" {
		apiHost = strings.ToLower(u.Hostname())
	}
	hosts[apiHost] = struct{}{}
	return hosts
}

// NewReleaseClient builds the hardened egress client for release detection.
func NewReleaseClient() (*outbound.Client, error) {
	return outbound.New(outbound.Config{
		AllowHosts:   ReleaseEgressHosts(),
		Timeout:      releaseHTTPTimeout,
		MaxBodyBytes: manifestMaxBytes,
	})
}

// ghRelease is the subset of the Releases payload this consumer reads.
type ghRelease struct {
	TagName     string `json:"tag_name"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	CreatedAt   string `json:"created_at"`
	Assets      []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// The asset a stable release carries. Distinct from
// scripts/release/release-manifest.json, which is a different file.
const ManifestAssetName = "platform-release-manifest.json"

func (g *GitHubSource) List(ctx context.Context) ([]Listing, error) {
	u := fmt.Sprintf("%s/repos/%s/releases?per_page=%d", g.apiBase, g.repo, releasePageSize)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	g.authorize(req)

	body, err := g.read(req)
	if err != nil {
		return nil, fmt.Errorf("list releases of %s: %w", g.repo, err)
	}
	var raw []ghRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode releases of %s: %w", g.repo, err)
	}

	out := make([]Listing, 0, len(raw))
	for _, r := range raw {
		// A draft is not published: its images may not exist.
		if r.Draft {
			continue
		}
		l := Listing{
			Tag:         r.TagName,
			Version:     strings.TrimPrefix(r.TagName, "v"),
			Prerelease:  r.Prerelease,
			Body:        r.Body,
			PublishedAt: parseGitHubTime(r.PublishedAt, r.CreatedAt),
		}
		for _, a := range r.Assets {
			if a.Name == ManifestAssetName {
				l.ManifestURL = a.BrowserDownloadURL
				break
			}
		}
		out = append(out, l)
	}
	return out, nil
}

func (g *GitHubSource) FetchManifest(ctx context.Context, rawURL string) ([]byte, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("release carries no %s asset", ManifestAssetName)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("asset URL %q is unparseable: %w", rawURL, err)
	}
	// The asset host is remote-supplied, so it is refused by name here rather
	// than deep inside the transport.
	if !g.client.HostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("asset host %q is not on the release egress allowlist "+
			"(QUASAR_PLATFORM_RELEASE_ASSET_HOSTS)", u.Hostname())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build asset request: %w", err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	g.authorize(req)
	return g.read(req)
}

func (g *GitHubSource) CompareURL(fromCommit, toCommit string) string {
	if fromCommit == "" || toCommit == "" || g.repo == "" {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/compare/%s...%s", g.repo, fromCommit, toCommit)
}

// A public repository needs no token; a fork or a rate-limited instance does.
func (g *GitHubSource) authorize(req *http.Request) {
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
}

func (g *GitHubSource) read(req *http.Request) ([]byte, error) {
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	// The outbound client follows no redirect (a Location could point past the
	// allowlist), so a 3xx arrives here as an ordinary status.
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", req.URL.Redacted(), err)
	}
	return body, nil
}

// Falls back to created_at: a release published from an existing tag can carry
// a null published_at.
func parseGitHubTime(published, created string) time.Time {
	for _, s := range []string{published, created} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
