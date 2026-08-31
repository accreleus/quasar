package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Digest resolution (#440, protocol/control-api.md §"Digest pinning"). A
// registry_ref tag is mutable — two hosts adopting the same tag a week apart
// can pull different bits, silently splitting the fleet — so sync resolves
// each tag to its content digest and stores both, tag for display, digest for
// dispatch.
//
// A small dependency-free registry client rather than a vendored OCI library:
// the job is one token fetch plus one HEAD, and the failure mode is always
// "leave the digest empty", never an exception path.

const (
	// Bounds one ref's whole resolution (token + HEAD): a slow registry must not
	// stretch an N-image sync into N x forever. An unresolved digest is
	// supported; a hung sync is not.
	digestResolveTimeout = 5 * time.Second
	dockerContentDigest  = "Docker-Content-Digest"
)

// manifestAccept: all four media types, since a multi-arch image answers with
// an index and a single-arch one with a plain manifest — asking for only one
// 404s/406s a valid registry for half the catalog.
var manifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}, ", ")

// digestRe validates a registry's answer before it's stored: must never become
// an adopted registry_ref that only fails later, less obviously, at the
// agent's own dispatch-time validator.
var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// DigestResolver turns a tag ref into its immutable digest ref. Injectable so
// Store.Sync can run against a fake registry instead of the live internet.
type DigestResolver interface {
	// Resolve returns registry/name@sha256:<64hex>, or an error. Never fatal to
	// a sync — the caller stores an empty digest and logs a warning.
	Resolve(ctx context.Context, ref string) (string, error)
}

// noopResolver is what a test Store gets by default, so no test can
// accidentally make a live registry call.
type noopResolver struct{}

func (noopResolver) Resolve(context.Context, string) (string, error) {
	return "", fmt.Errorf("digest resolution disabled")
}

// NoopResolver returns a resolver that never resolves anything — for tests and
// air-gapped deployments, to keep every digest empty.
func NoopResolver() DigestResolver { return noopResolver{} }

// RegistryResolver is the production DigestResolver: an anonymous-pull registry
// v2 client.
//
// SSRF containment (protocol/control-api.md §Digest pinning): the registry host
// comes from remote catalog data and the token realm from the registry's own
// response, so both are untrusted. HTTPS-only, both hosts checked against an
// allowlist, no HTTP redirects followed, and the DialContext refuses any
// non-public IP.
type RegistryResolver struct {
	client *http.Client
	// Registry/realm hosts this resolver may contact (QUASAR_IMAGE_REGISTRY_HOSTS,
	// default "ghcr.io"). nil only in tests — allows every host.
	allowHosts map[string]struct{}
	// Test seam only: replaces scheme://host of every request so a test can parse
	// a real ref while HTTP lands on an httptest server. The digest ref RETURNED
	// is always built from the parsed ref, never this override.
	baseURL string
}

// NewRegistryResolver builds the production resolver. A nil client gets the
// guarded client (timeout, no redirects, DNS-rebind dialer guard); callers
// supplying their own transport take responsibility for those protections.
func NewRegistryResolver(client *http.Client) *RegistryResolver {
	if client == nil {
		client = newGuardedClient(defaultLookupIP)
	}
	return &RegistryResolver{client: client, allowHosts: allowedHostsFromEnv()}
}

// newTestResolver builds a resolver whose HTTP calls all go to baseURL, with a
// nil allowlist since the httptest server stands in for any registry.
func newTestResolver(client *http.Client, baseURL string) *RegistryResolver {
	if client != nil && client.CheckRedirect == nil {
		client.CheckRedirect = noRedirect
	}
	return &RegistryResolver{client: client, baseURL: strings.TrimSuffix(baseURL, "/")}
}

// noRedirect: a registry response is untrusted, and a 3xx Location could point
// the next request past the allowlist/dialer at an internal address.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// ipLookup resolves a host to its IPs. Injectable so the DNS-rebind guard can
// be tested with a resolver returning a private address for an allowed host.
type ipLookup func(ctx context.Context, host string) ([]net.IP, error)

func defaultLookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// newGuardedClient: per-request timeout, no redirects, DialContext refuses any
// non-public IP.
func newGuardedClient(lookup ipLookup) *http.Client {
	return &http.Client{
		Timeout:       digestResolveTimeout,
		CheckRedirect: noRedirect,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           guardedDialContext(lookup),
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// guardedDialContext refuses loopback/private/link-local/multicast/unspecified
// addresses on every connection (DNS-rebind guard): resolves the host itself
// and dials the resolved IP directly, so a name resolving public-then-private
// across two lookups can't slip past. TLS ServerName still derives from the
// request URL, unaffected.
func guardedDialContext(lookup ipLookup) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split dial address %q: %w", addr, err)
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		for _, ip := range ips {
			if disallowedIP(ip) {
				return nil, fmt.Errorf("refusing connection to non-public address %s (host %q)", ip, host)
			}
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no addresses for %q", host)
		}
		return nil, lastErr
	}
}

// disallowedIP reports whether ip is one the resolver must never connect to.
func disallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// allowedHostsFromEnv parses QUASAR_IMAGE_REGISTRY_HOSTS (comma-separated,
// default "ghcr.io"); always non-nil so hostAllowed enforces it in production.
func allowedHostsFromEnv() map[string]struct{} {
	raw := os.Getenv("QUASAR_IMAGE_REGISTRY_HOSTS")
	if strings.TrimSpace(raw) == "" {
		raw = "ghcr.io"
	}
	out := make(map[string]struct{})
	for _, h := range strings.Split(raw, ",") {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			out[h] = struct{}{}
		}
	}
	if len(out) == 0 {
		out["ghcr.io"] = struct{}{}
	}
	return out
}

// hostAllowed: nil allowlist (test resolvers only) allows everything.
func (r *RegistryResolver) hostAllowed(host string) bool {
	if r.allowHosts == nil {
		return true
	}
	_, ok := r.allowHosts[strings.ToLower(host)]
	return ok
}

// parsedRef is a registry reference split into the pieces the v2 API needs.
type parsedRef struct {
	Registry string // e.g. ghcr.io
	Name     string // e.g. accreleus/quasar-steam
	Tag      string // e.g. 1.4.0
	Digest   string // non-empty when the ref was ALREADY a digest ref
}

// parseRef splits a docker/OCI reference. Standard heuristic: the first
// slash-separated component is the registry only when it looks like a host
// (dot/colon, or "localhost"); otherwise it's a Docker Hub short name.
func parseRef(ref string) (parsedRef, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return parsedRef{}, fmt.Errorf("empty registry ref")
	}

	var p parsedRef

	if i := strings.Index(ref, "@"); i >= 0 { // already a digest ref: no resolution needed
		p.Digest = ref[i+1:]
		ref = ref[:i]
	}

	remainder := ref
	if i := strings.Index(ref, "/"); i >= 0 {
		first := ref[:i]
		if strings.ContainsAny(first, ".:") || first == "localhost" {
			p.Registry = first
			remainder = ref[i+1:]
		}
	}
	if p.Registry == "" {
		// Docker Hub short form: a single-component name is implicit library/.
		p.Registry = "docker.io"
		if !strings.Contains(remainder, "/") {
			remainder = "library/" + remainder
		}
	}

	// Tag is after the last colon, but only when that colon is after the last
	// slash — otherwise it's a registry port.
	p.Name = remainder
	if i := strings.LastIndex(remainder, ":"); i >= 0 && !strings.Contains(remainder[i+1:], "/") {
		p.Name = remainder[:i]
		p.Tag = remainder[i+1:]
	}
	if p.Name == "" {
		return parsedRef{}, fmt.Errorf("registry ref %q has no repository name", ref)
	}
	if p.Tag == "" && p.Digest == "" {
		p.Tag = "latest"
	}
	return p, nil
}

// apiHost handles docker.io's split between ref name and API endpoint.
func (p parsedRef) apiHost() string {
	if p.Registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return p.Registry
}

// base returns scheme://host for this ref, honouring the test override.
func (r *RegistryResolver) base(p parsedRef) string {
	if r.baseURL != "" {
		return r.baseURL
	}
	return "https://" + p.apiHost()
}

// Resolve implements DigestResolver.
func (r *RegistryResolver) Resolve(ctx context.Context, ref string) (string, error) {
	// A registry_ref is host/name:tag, never a URL — a scheme is a mistake or an
	// attempt to smuggle a non-https target past the allowlist.
	if strings.Contains(ref, "://") {
		return "", fmt.Errorf("registry ref %q must not carry a URL scheme (https is implied)", ref)
	}
	p, err := parseRef(ref)
	if err != nil {
		return "", err
	}
	// SSRF containment: must check the allowlist before any outbound request is
	// built, since the host comes from remote catalog data.
	if !r.hostAllowed(p.Registry) {
		return "", fmt.Errorf("registry host %q is not in the allowlist (QUASAR_IMAGE_REGISTRY_HOSTS)", p.Registry)
	}
	if p.Digest != "" {
		if !digestRe.MatchString(p.Digest) { // already immutable; nothing to look up
			return "", fmt.Errorf("registry ref %q carries a malformed digest", ref)
		}
		return p.Registry + "/" + p.Name + "@" + p.Digest, nil
	}

	ctx, cancel := context.WithTimeout(ctx, digestResolveTimeout)
	defer cancel()

	// GHCR's token endpoint is well-known, so skip the guaranteed 401 round
	// trip; every other registry is discovered via WWW-Authenticate below.
	token := ""
	if p.apiHost() == "ghcr.io" {
		token, _ = r.fetchToken(ctx, "https://ghcr.io/token", "ghcr.io", p.scope())
	}

	digest, challenge, err := r.headManifest(ctx, p, token)
	if err != nil {
		return "", err
	}
	if digest == "" && challenge != "" {
		// Standard bearer-challenge flow, anonymous only: a private registry
		// needing credentials legitimately stays unresolved.
		realm, params := parseChallenge(challenge)
		if realm == "" {
			return "", fmt.Errorf("registry %s: auth challenge without a realm", p.apiHost())
		}
		scope := params["scope"]
		if scope == "" {
			scope = p.scope()
		}
		token, err = r.fetchToken(ctx, realm, params["service"], scope)
		if err != nil {
			return "", err
		}
		digest, _, err = r.headManifest(ctx, p, token)
		if err != nil {
			return "", err
		}
	}
	if digest == "" {
		return "", fmt.Errorf("registry %s: no %s for %s:%s", p.apiHost(), dockerContentDigest, p.Name, p.Tag)
	}
	if !digestRe.MatchString(digest) {
		return "", fmt.Errorf("registry %s returned a malformed digest %q", p.apiHost(), digest)
	}
	return p.Registry + "/" + p.Name + "@" + digest, nil
}

// scope is the pull scope this ref's token needs.
func (p parsedRef) scope() string { return "repository:" + p.Name + ":pull" }

// headManifest returns (digest, "", nil) on success, ("", challenge, nil) on a
// 401 bearer challenge, and an error otherwise — 404 included, since a missing
// tag is operator-actionable and worth naming rather than silently retrying.
func (r *RegistryResolver) headManifest(ctx context.Context, p parsedRef, token string) (string, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", r.base(p), p.Name, p.Tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("build manifest request: %w", err)
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("head manifest %s: %w", url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Header.Get(dockerContentDigest), "", nil
	case resp.StatusCode == http.StatusUnauthorized:
		if ch := resp.Header.Get("WWW-Authenticate"); ch != "" && token == "" {
			return "", ch, nil
		}
		return "", "", fmt.Errorf("head manifest %s: unauthorized", url)
	default:
		return "", "", fmt.Errorf("head manifest %s: status %d", url, resp.StatusCode)
	}
}

// tokenResponse covers both spellings registries use for an anonymous pull
// token (`token` per the Docker spec, `access_token` per OAuth2).
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

// fetchToken asks realm for an anonymous pull token. realm is used verbatim
// when absolute (a WWW-Authenticate challenge); the test override applies only
// to the constructed well-known GHCR endpoint.
func (r *RegistryResolver) fetchToken(ctx context.Context, realm, service, scope string) (string, error) {
	// realm is the most dangerous input here: a full URL taken verbatim from the
	// registry's own header. Constrain it like the registry host — https only,
	// no userinfo, allowlisted — so a hostile registry can't redirect the token
	// fetch at an internal or credentialed endpoint.
	u, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse token realm %q: %w", realm, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("token realm %q is not https", realm)
	}
	if u.User != nil {
		return "", fmt.Errorf("token realm %q carries userinfo", realm)
	}
	if !r.hostAllowed(u.Hostname()) {
		return "", fmt.Errorf("token realm host %q is not in the allowlist (QUASAR_IMAGE_REGISTRY_HOSTS)", u.Hostname())
	}
	if r.baseURL != "" && strings.HasPrefix(realm, "https://") {
		// Test seam: redirect the constructed ghcr.io endpoint at the fake
		// registry (a discovered realm already points there).
		if i := strings.Index(realm[len("https://"):], "/"); i >= 0 {
			realm = r.baseURL + realm[len("https://")+i:]
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	q := req.URL.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	req.URL.RawQuery = q.Encode()

	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch registry token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch registry token: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // bounded: a hostile registry must not stream us out of memory
	if err != nil {
		return "", fmt.Errorf("read registry token: %w", err)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("decode registry token: %w", err)
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	if tr.AccessToken != "" {
		return tr.AccessToken, nil
	}
	return "", fmt.Errorf("registry token response carried no token")
}

// parseChallenge splits a `Bearer realm="…",service="…",scope="…"` header into
// its realm and the remaining parameters.
func parseChallenge(h string) (realm string, params map[string]string) {
	params = map[string]string{}
	h = strings.TrimSpace(h)
	if !strings.HasPrefix(strings.ToLower(h), "bearer") {
		return "", params
	}
	for _, part := range splitChallengeParams(h[len("bearer"):]) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		if k == "realm" {
			realm = v
			continue
		}
		params[k] = v
	}
	return realm, params
}

// splitChallengeParams splits on commas that are not inside a quoted value —
// a scope value legitimately contains commas ("repository:a:pull,push").
func splitChallengeParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, c := range s {
		switch {
		case c == '"':
			inQuote = !inQuote
			cur.WriteRune(c)
		case c == ',' && !inQuote:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
