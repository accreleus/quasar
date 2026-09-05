// digest_test.go — the P3 digest resolver against a fake registry (httptest).
// No DB and no network: these run in a plain `go test ./...`.
package images

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testDigest is a well-formed sha256 digest: exactly 64 lowercase hex chars
// (8 × the 8-char group below), which is what the resolver validates against.
const testDigest = "sha256:ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34ab12cd34"

// fakeRegistry is a minimal registry v2 server. Its behaviour per test is set
// by the fields: whether a token is required, whether it answers the manifest
// HEAD, and how long it stalls first.
type fakeRegistry struct {
	t *testing.T

	// requireToken, when set, makes the manifest HEAD answer 401 with a bearer
	// challenge until a token is presented.
	requireToken bool
	// notFound makes every manifest HEAD answer 404.
	notFound bool
	// stall delays the manifest HEAD (used to trip the resolver's timeout).
	stall time.Duration
	// missingDigestHeader answers 200 with no Docker-Content-Digest.
	missingDigestHeader bool
	// realmHost overrides the host in the WWW-Authenticate realm (default
	// auth.example.com) so a test can point it off an allowlist.
	realmHost string

	srv *httptest.Server

	// observed request facts the assertions care about.
	tokenScope  string
	acceptSeen  string
	authSeen    string
	tokenIssued int
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenScope = r.URL.Query().Get("scope")
		f.tokenIssued++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"token":"fake-anonymous-token"}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if f.stall > 0 {
			time.Sleep(f.stall)
		}
		f.acceptSeen = r.Header.Get("Accept")
		f.authSeen = r.Header.Get("Authorization")
		if f.requireToken && f.authSeen == "" {
			// A discovered realm on a real https host: the resolver validates it
			// (https + allowlist) and then the baseURL test seam redirects it at
			// this httptest server. realmHost lets a test point the realm off the
			// allowlist to prove that path is refused.
			host := f.realmHost
			if host == "" {
				host = "auth.example.com"
			}
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="https://%s/token",service="fake.registry",scope="repository:acme/app:pull"`, host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.notFound {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if !f.missingDigestHeader {
			w.Header().Set(dockerContentDigest, testDigest)
		}
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) resolver() *RegistryResolver {
	return newTestResolver(&http.Client{Timeout: digestResolveTimeout}, f.srv.URL)
}

// TestResolveGHCRTokenThenHead — the documented GHCR path: fetch the anonymous
// pull token, HEAD the manifest with the four manifest Accept types, take
// Docker-Content-Digest, and return the DIGEST form built from the ORIGINAL
// ref's registry+name (never from the transport host).
func TestResolveGHCRTokenThenHead(t *testing.T) {
	reg := newFakeRegistry(t)
	got, err := reg.resolver().Resolve(context.Background(), "ghcr.io/accreleus/quasar-steam:1.4.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "ghcr.io/accreleus/quasar-steam@" + testDigest
	if got != want {
		t.Fatalf("digest ref: got %q want %q", got, want)
	}
	if reg.tokenIssued != 1 {
		t.Fatalf("token fetches: got %d want 1 (ghcr must not need a 401 round trip first)", reg.tokenIssued)
	}
	if reg.tokenScope != "repository:accreleus/quasar-steam:pull" {
		t.Fatalf("token scope: got %q", reg.tokenScope)
	}
	if reg.authSeen != "Bearer fake-anonymous-token" {
		t.Fatalf("manifest HEAD Authorization: got %q", reg.authSeen)
	}
	for _, mt := range []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
	} {
		if !strings.Contains(reg.acceptSeen, mt) {
			t.Fatalf("manifest Accept header %q missing %q", reg.acceptSeen, mt)
		}
	}
}

// TestResolveAuthChallengeFlow — a non-GHCR registry: no token up front, the
// 401's WWW-Authenticate challenge names realm/service/scope, and the resolver
// follows it and retries.
func TestResolveAuthChallengeFlow(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.requireToken = true

	got, err := reg.resolver().Resolve(context.Background(), "registry.example.com/acme/app:2.0")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := "registry.example.com/acme/app@" + testDigest
	if got != want {
		t.Fatalf("digest ref: got %q want %q", got, want)
	}
	if reg.tokenIssued != 1 {
		t.Fatalf("token fetches: got %d want 1 (discovered from the challenge)", reg.tokenIssued)
	}
	if reg.tokenScope != "repository:acme/app:pull" {
		t.Fatalf("challenge scope: got %q", reg.tokenScope)
	}
}

// TestResolveNotFoundIsAnError — a 404 tag must surface as an error so the
// caller stores an empty digest (and refuses install) rather than inventing one.
func TestResolveNotFoundIsAnError(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.notFound = true

	if _, err := reg.resolver().Resolve(context.Background(), "ghcr.io/acme/gone:1.0"); err == nil {
		t.Fatal("resolve of a 404 tag: got nil error, want a failure")
	}
}

// TestResolveTimeout — a stalled registry must fail fast rather than hold the
// sync open. The resolver is given a client timeout well under the stall.
func TestResolveTimeout(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.stall = 2 * time.Second

	r := newTestResolver(&http.Client{Timeout: 150 * time.Millisecond}, reg.srv.URL)
	start := time.Now()
	if _, err := r.Resolve(context.Background(), "ghcr.io/acme/slow:1.0"); err == nil {
		t.Fatal("resolve against a stalled registry: got nil error, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("resolve took %s; it must fail fast, not hold the sync open", elapsed)
	}
}

// TestResolveMissingDigestHeader — a 200 with no Docker-Content-Digest is a
// failure, not an empty-but-successful digest.
func TestResolveMissingDigestHeader(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.missingDigestHeader = true

	if _, err := reg.resolver().Resolve(context.Background(), "ghcr.io/acme/app:1.0"); err == nil {
		t.Fatal("resolve without a digest header: got nil error, want a failure")
	}
}

// TestResolveAlreadyDigestRefIsPassedThrough — a manifest that already pins a
// digest must not cost a network call on every sync forever.
func TestResolveAlreadyDigestRefIsPassedThrough(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.notFound = true // any network call would fail, proving none was made

	ref := "ghcr.io/acme/app@" + testDigest
	got, err := reg.resolver().Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve of an already-digest ref: %v", err)
	}
	if got != ref {
		t.Fatalf("digest ref: got %q want %q", got, ref)
	}
}

// TestParseRef covers the ref shapes the catalog can carry.
func TestParseRef(t *testing.T) {
	cases := []struct {
		ref                          string
		registry, name, tag, apiHost string
	}{
		{"ghcr.io/accreleus/quasar-steam:1.4.0", "ghcr.io", "accreleus/quasar-steam", "1.4.0", "ghcr.io"},
		{"ghcr.io/accreleus/quasar-steam", "ghcr.io", "accreleus/quasar-steam", "latest", "ghcr.io"},
		{"ubuntu:24.04", "docker.io", "library/ubuntu", "24.04", "registry-1.docker.io"},
		{"acme/app:1", "docker.io", "acme/app", "1", "registry-1.docker.io"},
		{"localhost:5000/acme/app:dev", "localhost:5000", "acme/app", "dev", "localhost:5000"},
	}
	for _, c := range cases {
		p, err := parseRef(c.ref)
		if err != nil {
			t.Fatalf("parseRef(%q): %v", c.ref, err)
		}
		if p.Registry != c.registry || p.Name != c.name || p.Tag != c.tag || p.apiHost() != c.apiHost {
			t.Fatalf("parseRef(%q): got registry=%q name=%q tag=%q apiHost=%q, want %q/%q/%q/%q",
				c.ref, p.Registry, p.Name, p.Tag, p.apiHost(), c.registry, c.name, c.tag, c.apiHost)
		}
	}
	if _, err := parseRef("  "); err == nil {
		t.Fatal("parseRef of an empty ref: got nil error")
	}
}

// --- SSRF containment (protocol/control-api.md §Digest pinning) ---------------

// TestResolveRegistryHostOffAllowlist — a ref whose registry host is not on the
// allowlist resolves to an error (empty digest), never an outbound request. A
// private-IP host is the concrete SSRF case: it is off the default allowlist, so
// it is refused before any dial.
func TestResolveRegistryHostOffAllowlist(t *testing.T) {
	reg := newFakeRegistry(t)
	r := reg.resolver()
	r.allowHosts = map[string]struct{}{"ghcr.io": {}} // the production default

	for _, ref := range []string{
		"192.0.2.10/acme/app:1",     // private IP host
		"192.168.0.5:5000/x/y:2",    // private IP host with port
		"registry.internal/x/y:1.0", // off-allowlist name
	} {
		if _, err := r.Resolve(context.Background(), ref); err == nil {
			t.Fatalf("Resolve(%q): got nil error, want an off-allowlist refusal", ref)
		}
	}
}

// TestResolveRejectsURLSchemeRef — a registry_ref carrying a URL scheme is
// refused outright (it is a ref, not a URL, and a scheme is an SSRF smell).
func TestResolveRejectsURLSchemeRef(t *testing.T) {
	reg := newFakeRegistry(t)
	if _, err := reg.resolver().Resolve(context.Background(), "http://169.254.169.254/x/y:1"); err == nil {
		t.Fatal("Resolve of a scheme-carrying ref: got nil error, want a refusal")
	}
}

// TestResolveRealmOffAllowlist — the token realm host (from the registry's own
// WWW-Authenticate) must be on the allowlist too, even when the registry host
// itself is allowed.
func TestResolveRealmOffAllowlist(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.requireToken = true
	reg.realmHost = "evil.internal" // the realm points off the allowlist
	r := reg.resolver()
	r.allowHosts = map[string]struct{}{"registry.example.com": {}} // ref host allowed, realm host not

	if _, err := r.Resolve(context.Background(), "registry.example.com/acme/app:2.0"); err == nil {
		t.Fatal("Resolve with an off-allowlist token realm: got nil error, want a refusal")
	}
}

// TestFetchTokenRejectsNonHTTPSAndUserinfo — the realm-hardening rules, tested
// directly: http scheme, userinfo, and an off-allowlist host each fail.
func TestFetchTokenRejectsNonHTTPSAndUserinfo(t *testing.T) {
	r := &RegistryResolver{
		client:     &http.Client{},
		allowHosts: map[string]struct{}{"ghcr.io": {}},
	}
	ctx := context.Background()
	cases := []struct {
		name, realm string
	}{
		{"http scheme", "http://ghcr.io/token"},
		{"userinfo", "https://user:pass@ghcr.io/token"},
		{"off-allowlist host", "https://evil.internal/token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := r.fetchToken(ctx, c.realm, "svc", "scope"); err == nil {
				t.Fatalf("fetchToken(%q): got nil error, want a refusal", c.realm)
			}
		})
	}
}

// TestResolveDoesNotFollowRedirects — a registry answering the manifest HEAD
// with a 3xx must not be followed: the redirect target is never contacted, and
// the resolver surfaces the non-OK status as an error.
func TestResolveDoesNotFollowRedirects(t *testing.T) {
	var targetHit bool
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"token":"t"}`)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) {
		targetHit = true
		w.Header().Set(dockerContentDigest, testDigest)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := newTestResolver(&http.Client{Timeout: digestResolveTimeout}, srv.URL)
	if _, err := r.Resolve(context.Background(), "ghcr.io/acme/app:1.0"); err == nil {
		t.Fatal("Resolve against a redirecting registry: got nil error, want the redirect refused")
	}
	if targetHit {
		t.Fatal("the redirect target was contacted — CheckRedirect must stop the client following it")
	}
}

// The DNS-rebind dial guard and its IP classification moved to
// internal/outbound with the code they cover (#105); their tests live in
// internal/outbound/guard_test.go.

// TestAllowedHostsFromEnv — the env parse: default ghcr.io, comma split, trim,
// lowercase.
func TestAllowedHostsFromEnv(t *testing.T) {
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", "")
	if h := allowedHostsFromEnv(); len(h) != 1 || func() bool { _, ok := h["ghcr.io"]; return !ok }() {
		t.Fatalf("default: got %v want {ghcr.io}", h)
	}
	t.Setenv("QUASAR_IMAGE_REGISTRY_HOSTS", " ghcr.io , Registry.Example.COM ,, ")
	h := allowedHostsFromEnv()
	if _, ok := h["ghcr.io"]; !ok {
		t.Fatalf("missing ghcr.io: %v", h)
	}
	if _, ok := h["registry.example.com"]; !ok {
		t.Fatalf("missing lowercased registry.example.com: %v", h)
	}
	if len(h) != 2 {
		t.Fatalf("empty entries must be dropped: %v", h)
	}
}

// TestParseChallenge — a scope value legitimately contains commas, so the
// splitter must not treat them as parameter separators.
func TestParseChallenge(t *testing.T) {
	realm, params := parseChallenge(`Bearer realm="https://auth.example.com/token",service="reg",scope="repository:a/b:pull,push"`)
	if realm != "https://auth.example.com/token" {
		t.Fatalf("realm: got %q", realm)
	}
	if params["service"] != "reg" {
		t.Fatalf("service: got %q", params["service"])
	}
	if params["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("scope: got %q (a comma inside the quoted value must not split it)", params["scope"])
	}
}
