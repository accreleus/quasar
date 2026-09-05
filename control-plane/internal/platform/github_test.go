package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// An httptest server is loopback and plain http, both of which the real
// outbound.Client refuses by design (its dial guard is the DNS-rebind
// protection). So the source is exercised through this Doer, which keeps the
// one check the source itself owns — the asset host allowlist — real.
type fakeDoer struct {
	allow map[string]bool
	// rewriteTo maps a fake hostname onto a test server, so a URL can name a
	// host the allowlist knows while still resolving to httptest. Keyed by
	// host, because an asset download is answered by a SECOND origin.
	rewriteTo map[string]string
	// seen records the Authorization header per hostname, so the test can
	// assert the token does not ride the redirect hop.
	seen map[string]string
}

func (f *fakeDoer) HostAllowed(host string) bool { return f.allow[strings.ToLower(host)] }

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	// The real client refuses an off-allowlist host before any I/O; the fake
	// must too, or a test could pass on a request production would never send.
	if !f.HostAllowed(host) {
		return nil, fmt.Errorf("outbound: host %q is not in the allowlist", host)
	}
	if f.seen != nil {
		f.seen[host] = req.Header.Get("Authorization")
	}
	target, ok := f.rewriteTo[host]
	if !ok {
		return nil, fmt.Errorf("fakeDoer: no server for host %q", host)
	}
	t, _ := url.Parse(target)
	u := *req.URL
	u.Scheme, u.Host = t.Scheme, t.Host
	out, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header
	// The outbound client never follows a redirect: a 3xx is returned to the
	// caller as-is, which is exactly the case under test.
	return (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(out)
}

type ghAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func releasesJSON(t *testing.T, assetHost string) string {
	t.Helper()
	body := []map[string]any{
		{
			"tag_name": "v0.2.0", "draft": false, "prerelease": false,
			"body": "### Fixed\n- a thing\n", "published_at": "2026-09-04T12:00:00Z",
			"assets": []ghAsset{{Name: ManifestAssetName, URL: "https://" + assetHost + "/manifest.json"}},
		},
		{
			"tag_name": "v0.3.0-rc.1", "draft": true, "prerelease": true,
			"body": "draft", "published_at": "2026-09-05T12:00:00Z",
			"assets": []ghAsset{},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// harness wires a fake API host, a fake asset host, and (optionally) a fake CDN
// the asset host 302s to — GitHub's real shape.
type harness struct {
	src  *GitHubSource
	doer *fakeDoer
}

// auth is the Authorization header the given fake host saw.
func (h harness) auth(host string) string { return h.doer.seen[host] }

// newGitHubHarness serves the releases listing on api.example.test and the
// asset on assetHost. redirectTo, when set, makes the asset host answer 302 to
// that host — which is what github.com does for every real release asset.
func newGitHubHarness(t *testing.T, assetHost, redirectTo string, allow ...string) harness {
	t.Helper()

	doer := &fakeDoer{
		allow:     map[string]bool{"api.example.test": true},
		rewriteTo: map[string]string{},
		seen:      map[string]string{},
	}
	for _, h := range allow {
		doer.allow[h] = true
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_, _ = w.Write([]byte(releasesJSON(t, assetHost)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	doer.rewriteTo["api.example.test"] = api.URL

	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if redirectTo != "" {
			http.Redirect(w, r, "https://"+redirectTo+"/presigned/manifest.json", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(goodManifest))
	}))
	t.Cleanup(asset.Close)
	doer.rewriteTo[assetHost] = asset.URL

	if redirectTo != "" {
		cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(goodManifest))
		}))
		t.Cleanup(cdn.Close)
		doer.rewriteTo[redirectTo] = cdn.URL
	}

	assetHosts := []string{assetHost}
	if redirectTo != "" && doer.allow[redirectTo] {
		assetHosts = append(assetHosts, redirectTo)
	}
	return harness{
		src:  NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "tok", assetHosts),
		doer: doer,
	}
}

func TestGitHubSourceListsNonDraftReleasesWithTheirManifestAsset(t *testing.T) {
	h := newGitHubHarness(t, "assets.example.test", "", "assets.example.test")
	src := h.src

	listings, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listings) != 1 {
		t.Fatalf("listings = %+v, want the one non-draft release", listings)
	}
	l := listings[0]
	if l.Version != "0.2.0" {
		t.Errorf("version = %q, want the tag without its leading v", l.Version)
	}
	if l.ManifestURL == "" {
		t.Error("the manifest asset's browser_download_url was not carried")
	}
	if got := h.auth("api.example.test"); got != "Bearer tok" {
		t.Errorf("Authorization = %q, want the configured token as a bearer", got)
	}

	raw, err := src.FetchManifest(context.Background(), l.ManifestURL)
	if err != nil {
		t.Fatalf("fetch manifest: %v", err)
	}
	if _, err := ParseManifest(raw); err != nil {
		t.Fatalf("fetched manifest does not validate: %v", err)
	}
}

func TestGitHubSourceRefusesAnAssetHostOutsideTheAllowlist(t *testing.T) {
	// The asset host comes out of the listing body — remote-supplied — so a
	// release naming a host nobody allowlisted must be refused, by name,
	// before any request is built.
	src := newGitHubHarness(t, "evil.example.test", "").src

	listings, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_, err = src.FetchManifest(context.Background(), listings[0].ManifestURL)
	if err == nil {
		t.Fatal("an asset host off the allowlist was fetched")
	}
	if !strings.Contains(err.Error(), "evil.example.test") ||
		!strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error %q must name the refused host and the allowlist", err)
	}
}

func TestGitHubSourceReportsANonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	doer := &fakeDoer{
		allow:     map[string]bool{"api.example.test": true},
		rewriteTo: map[string]string{"api.example.test": srv.URL},
	}

	src := NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "", nil)
	if _, err := src.List(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the 503 reported", err)
	}
}

func TestReleaseEgressHostsCoversTheAPIAndTheAssetHosts(t *testing.T) {
	hosts := ReleaseEgressHosts()
	for _, want := range []string{defaultAPIHost, "github.com", "objects.githubusercontent.com"} {
		if _, ok := hosts[want]; !ok {
			t.Errorf("the default release allowlist is missing %q", want)
		}
	}
}

func TestCompareURLIsNilWithoutBothCommits(t *testing.T) {
	src := NewGitHubSource(&fakeDoer{}, "", "accreleus/quasar", "", nil)
	if got := src.CompareURL("", commitA); got != "" {
		t.Errorf("CompareURL with no from-commit = %q, want empty", got)
	}
	want := "https://github.com/accreleus/quasar/compare/" + commitA + "..." + commitB
	if got := src.CompareURL(commitA, commitB); got != want {
		t.Errorf("CompareURL = %q, want %q", got, want)
	}
}

// GitHub answers every real asset download with a 302 to a presigned URL on its
// own CDN. Live finding against v0.2.0-rc.1: the flat no-redirect rule read
// that 302 as the answer, so the release recorded manifest_invalid and stored
// nothing.
func TestGitHubSourceFollowsOneRedirectToAnAllowedAssetHost(t *testing.T) {
	h := newGitHubHarness(t, "github.example.test", "objects.example.test",
		"github.example.test", "objects.example.test")

	listings, err := h.src.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	raw, err := h.src.FetchManifest(context.Background(), listings[0].ManifestURL)
	if err != nil {
		t.Fatalf("a 302 to an allowed asset host must be followed: %v", err)
	}
	if _, err := ParseManifest(raw); err != nil {
		t.Fatalf("manifest fetched across the redirect does not validate: %v", err)
	}
	// The redirect target is already presigned; forwarding the token to a CDN
	// would leak it.
	if got := h.auth("objects.example.test"); got != "" {
		t.Errorf("the redirect hop carried Authorization %q, want none", got)
	}
}

func TestGitHubSourceRefusesARedirectOffTheAllowlistAndNamesTheHost(t *testing.T) {
	// The redirect target is remote-supplied too, so it passes the same
	// allowlist the first request did — and the error names it, because
	// widening QUASAR_PLATFORM_RELEASE_ASSET_HOSTS is the operator's remedy.
	h := newGitHubHarness(t, "github.example.test", "cdn.evil.test", "github.example.test")

	listings, err := h.src.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	_, err = h.src.FetchManifest(context.Background(), listings[0].ManifestURL)
	if err == nil {
		t.Fatal("a redirect off the allowlist was followed")
	}
	if !strings.Contains(err.Error(), "cdn.evil.test") ||
		!strings.Contains(err.Error(), "QUASAR_PLATFORM_RELEASE_ASSET_HOSTS") {
		t.Fatalf("error %q must name the redirect target and the knob that widens the allowlist", err)
	}
}

func TestGitHubSourceFollowsOnlyOneHop(t *testing.T) {
	// Two hops is a redirect chain, and a chain is how a hostile origin walks a
	// client somewhere the first check never saw.
	doer := &fakeDoer{
		allow:     map[string]bool{"api.example.test": true, "a.example.test": true},
		rewriteTo: map[string]string{},
		seen:      map[string]string{},
	}
	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://a.example.test/again", http.StatusFound)
	}))
	t.Cleanup(loop.Close)
	doer.rewriteTo["a.example.test"] = loop.URL

	src := NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "",
		[]string{"a.example.test"})
	_, err := src.FetchManifest(context.Background(), "https://a.example.test/manifest.json")
	if err == nil || !strings.Contains(err.Error(), "second redirect is not followed") {
		t.Fatalf("err = %v, want the second redirect refused", err)
	}
}
