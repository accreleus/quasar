package images

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Template context sha resolution (protocol/schema.md's P4 amendment). A
// kind=template entry's build context lives at whatever
// instance_settings.image_catalog_ref currently names (a mutable branch/tag,
// like a prebuilt's tag before #440), so sync resolves it to a commit sha once
// and stamps it onto every template row written — the deterministic analogue
// of registry_digest. image_build's context_url is built from this sha, never
// a floating ref.

// commitSHARe validates what GitHub hands back before storing it: a full
// 40-char lowercase-hex sha, or it must not become the adopted context_sha.
var commitSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// repoRe validates the "owner/name" shape before interpolating into a request
// path. Both sources are operator-configured (QUASAR_IMAGE_CATALOG_REPO,
// never remote catalog data), but validate before build regardless.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// ContextResolver turns a (repo, ref) pair into the commit sha ref names right
// now. Injectable, same seam as DigestResolver for registries.
type ContextResolver interface {
	// Resolve returns the 40-hex commit sha, or an error. Never fatal to a
	// sync — the caller leaves context_sha empty and logs a warning.
	Resolve(ctx context.Context, repo, ref string) (string, error)
}

// noopContextResolver is the default for NewStoreWithFetcher (test entry
// point), so no test accidentally reaches the live internet.
type noopContextResolver struct{}

func (noopContextResolver) Resolve(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("template context resolution disabled")
}

// NoopContextResolver returns a resolver that never resolves anything, for
// tests and air-gapped deployments — every template stays un-installable, the
// documented safe state.
func NoopContextResolver() ContextResolver { return noopContextResolver{} }

// githubAPIHost is the only host GitHubContextResolver ever contacts: a
// compiled-in constant, unlike the digest resolver's remote-sourced registry host.
const githubAPIHost = "api.github.com"

// GitHubContextResolver asks GitHub's REST API for the commit sha a ref
// currently names (GET /repos/{owner}/{repo}/commits/{ref}, Accept:
// application/vnd.github.sha — a raw sha response, no JSON).
//
// HTTPS-only, repo/ref interpolated only into the path, and reuses digest.go's
// guarded client (no redirects, DNS-rebind-safe) even with a fixed host —
// same SSRF discipline as the digest resolver.
type GitHubContextResolver struct {
	client *http.Client
	// baseURL, when non-empty, replaces https://api.github.com — test seam only.
	baseURL string
}

// NewGitHubContextResolver builds the production resolver. A nil client gets
// the guarded client (shared with the digest resolver).
func NewGitHubContextResolver(client *http.Client) *GitHubContextResolver {
	if client == nil {
		client = newGuardedClient(defaultLookupIP)
	}
	return &GitHubContextResolver{client: client}
}

// newTestContextResolver builds a resolver whose HTTP calls all go to baseURL.
func newTestContextResolver(client *http.Client, baseURL string) *GitHubContextResolver {
	if client != nil && client.CheckRedirect == nil {
		client.CheckRedirect = noRedirect
	}
	return &GitHubContextResolver{client: client, baseURL: strings.TrimSuffix(baseURL, "/")}
}

func (r *GitHubContextResolver) base() string {
	if r.baseURL != "" {
		return r.baseURL
	}
	return "https://" + githubAPIHost
}

// escapeRefPath percent-escapes each slash-separated component while
// preserving the slashes, so "heads/my-branch" stays multi-segment but a
// component with '#', '%', '?' or a space doesn't misresolve the URL.
func escapeRefPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Resolve implements ContextResolver.
func (r *GitHubContextResolver) Resolve(ctx context.Context, repo, ref string) (string, error) {
	if !repoRe.MatchString(repo) {
		return "", fmt.Errorf("template context repo %q is not a valid owner/name", repo)
	}
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("template context ref is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, digestResolveTimeout)
	defer cancel()

	// repo is already validated by repoRe, so only the ref needs escaping.
	u := fmt.Sprintf("%s/repos/%s/commits/%s", r.base(), repo, escapeRefPath(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build commit sha request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.sha")
	req.Header.Set("User-Agent", "quasar-control-plane")

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve commit sha %s: %w", u, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve commit sha %s: status %d", u, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096)) // bounded: response is a bare 40-hex sha
	if err != nil {
		return "", fmt.Errorf("read commit sha response from %s: %w", u, err)
	}
	sha := strings.TrimSpace(string(body))
	if !commitSHARe.MatchString(sha) {
		return "", fmt.Errorf("resolve commit sha %s: malformed response %q", u, sha)
	}
	return sha, nil
}
