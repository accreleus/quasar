package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

// GitHub Releases as a ReleaseSource. Knobs: docs/configuration.md
// (QUASAR_PLATFORM_RELEASE_REPO / _API / _TOKEN / _ASSET_HOSTS).

const (
	DefaultReleaseRepo = "accreleus/quasar"
	DefaultReleaseAPI  = "https://api.github.com"
	defaultAPIHost     = "api.github.com"
	// GitHub serves release assets from a CDN it moves without notice: the
	// 302 target was objects.githubusercontent.com and is now
	// release-assets.githubusercontent.com, which cost a live detection run.
	// All three stay listed — an old target that stops being used costs
	// nothing, and a hop this list does not cover is a dead detector.
	defaultAssetHostList = "github.com,objects.githubusercontent.com,release-assets.githubusercontent.com"

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
	// assetHosts narrows the ONE redirect an asset download may take. Passed in
	// rather than read from the environment here, so a test names its own hosts.
	assetHosts []string
}

// NewGitHubSource builds the source. apiBase, repo and assetHosts fall back to
// the documented defaults when blank.
func NewGitHubSource(client Doer, apiBase, repo, token string, assetHosts []string) *GitHubSource {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultReleaseAPI
	}
	if strings.TrimSpace(repo) == "" {
		repo = DefaultReleaseRepo
	}
	if len(assetHosts) == 0 {
		assetHosts = ReleaseAssetHosts()
	}
	return &GitHubSource{
		client:     client,
		apiBase:    strings.TrimRight(strings.TrimSpace(apiBase), "/"),
		repo:       strings.Trim(strings.TrimSpace(repo), "/"),
		token:      strings.TrimSpace(token),
		assetHosts: assetHosts,
	}
}

// ConfiguredReleaseRepo reads QUASAR_PLATFORM_RELEASE_REPO. Returns "" when
// detection is switched off.
//
// EMPTY MEANS THE DEFAULT, NOT OFF. Every compose layer forwards a knob as
// `${VAR:-}`, so a stock install with nothing in deploy/.env hands this process
// an empty string — and reading that as "off" would silently disable
// self-update on every one of them. `off` is the off switch, and it is the only
// one; the same reading applies to every other knob in this package.
func ConfiguredReleaseRepo() string {
	v := strings.TrimSpace(os.Getenv("QUASAR_PLATFORM_RELEASE_REPO"))
	switch strings.ToLower(v) {
	case "":
		return DefaultReleaseRepo
	case "off", "none", "disabled":
		return ""
	}
	return v
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
	hosts := make(map[string]struct{})
	for _, h := range ReleaseAssetHosts() {
		hosts[h] = struct{}{}
	}
	apiHost := defaultAPIHost
	if u, err := url.Parse(ConfiguredReleaseAPI()); err == nil && u.Hostname() != "" {
		apiHost = strings.ToLower(u.Hostname())
	}
	hosts[apiHost] = struct{}{}
	return hosts
}

// ReleaseAssetHosts is QUASAR_PLATFORM_RELEASE_ASSET_HOSTS: the hosts a release
// asset may be served from, and the only hosts its one redirect may point at.
func ReleaseAssetHosts() []string {
	// ParseHostList's fallback is one host, not a list, so an unset knob is
	// expanded here rather than handed to it whole.
	raw := os.Getenv("QUASAR_PLATFORM_RELEASE_ASSET_HOSTS")
	if strings.TrimSpace(raw) == "" {
		raw = defaultAssetHostList
	}
	set := outbound.ParseHostList(raw, defaultAPIHost)
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out) // stable, so a log line and an error read the same twice
	return out
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

	// GitHub answers every asset download with a 302 to a presigned URL on its
	// own CDN, and the outbound client follows nothing by default — a Location
	// is remote-supplied and could point anywhere. Found live against
	// v0.2.0-rc.1: the 302 was read as the answer, so every real release
	// recorded manifest_invalid.
	//
	// This is the body of (*outbound.Client).GetFollowingOneRedirectWithHeader,
	// reached through the Doer seam that helper exports for exactly this case:
	// the method takes a *Client, and a *Client refuses the loopback plain-http
	// server the tests below stand up, so calling the method would leave the
	// hop untestable here. Same helper, same rules, same error values — one
	// validated hop, https, allowlisted, Authorization dropped on the second
	// request; g.assetHosts narrows it to the release asset hosts.
	resp, err := outbound.GetOneRedirect(g.client, req, nil, g.assetHosts)
	if err != nil {
		if errors.Is(err, outbound.ErrRedirectHost) {
			// The helper names the refused host; naming the knob as well is
			// what turns the job summary into something an operator can act on.
			return nil, fmt.Errorf("%w — widen QUASAR_PLATFORM_RELEASE_ASSET_HOSTS to allow it", err)
		}
		return nil, err
	}
	return readBody(req, resp)
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
	// The listing takes no hop: the outbound client follows no redirect (a
	// Location could point past the allowlist), so a 3xx arrives here as an
	// ordinary status. Only the asset download follows one, in FetchManifest.
	return readBody(req, resp)
}

// readBody drains and closes resp, failing any non-200. The body is bounded by
// the outbound client, so ReadAll cannot be run out of memory by a remote.
func readBody(req *http.Request, resp *http.Response) ([]byte, error) {
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
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
