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
	// hostFor maps a fake hostname onto the test server, so a URL can name a
	// host the allowlist knows while still resolving to httptest.
	rewriteTo string
}

func (f *fakeDoer) HostAllowed(host string) bool { return f.allow[strings.ToLower(host)] }

func (f *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	if !f.HostAllowed(req.URL.Hostname()) {
		return nil, fmt.Errorf("outbound: host %q is not in the allowlist", req.URL.Hostname())
	}
	u := *req.URL
	target, _ := url.Parse(f.rewriteTo)
	u.Scheme, u.Host = target.Scheme, target.Host
	out, err := http.NewRequestWithContext(req.Context(), req.Method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	out.Header = req.Header
	return http.DefaultClient.Do(out)
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

func newGitHubHarness(t *testing.T, assetHost string, allow ...string) (*GitHubSource, *string) {
	t.Helper()
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases"):
			seenAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(releasesJSON(t, assetHost)))
		case strings.HasSuffix(r.URL.Path, "/manifest.json"):
			_, _ = w.Write([]byte(goodManifest))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	allowed := map[string]bool{"api.example.test": true}
	for _, h := range allow {
		allowed[h] = true
	}
	doer := &fakeDoer{allow: allowed, rewriteTo: srv.URL}
	return NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "tok"), &seenAuth
}

func TestGitHubSourceListsNonDraftReleasesWithTheirManifestAsset(t *testing.T) {
	src, auth := newGitHubHarness(t, "assets.example.test", "assets.example.test")

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
	if *auth != "Bearer tok" {
		t.Errorf("Authorization = %q, want the configured token as a bearer", *auth)
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
	src, _ := newGitHubHarness(t, "evil.example.test")

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
	doer := &fakeDoer{allow: map[string]bool{"api.example.test": true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	doer.rewriteTo = srv.URL

	src := NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "")
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
	src := NewGitHubSource(&fakeDoer{}, "", "accreleus/quasar", "")
	if got := src.CompareURL("", commitA); got != "" {
		t.Errorf("CompareURL with no from-commit = %q, want empty", got)
	}
	want := "https://github.com/accreleus/quasar/compare/" + commitA + "..." + commitB
	if got := src.CompareURL(commitA, commitB); got != want {
		t.Errorf("CompareURL = %q, want %q", got, want)
	}
}
