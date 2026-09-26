package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/images"
	"github.com/accreleus/quasar/control-plane/internal/platform/prerh06"
)

// What an RH06-era release and edge build publish, read by this control plane and by
// one that predates RH06 (the prerh06 fixture, v0.3.0's readers verbatim). #365: an
// old control plane sees nothing for either; this one reads both.

// publishedRelease serves one GitHub release whose assets are exactly those named,
// each answering with body. It returns a doer allowlisting both fake hosts.
func publishedRelease(t *testing.T, assets map[string]string) *fakeDoer {
	t.Helper()
	doer := &fakeDoer{
		allow:     map[string]bool{"api.example.test": true, "assets.example.test": true},
		rewriteTo: map[string]string{},
		seen:      map[string]string{},
	}
	type asset struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	}
	list := make([]asset, 0, len(assets))
	for name := range assets {
		list = append(list, asset{Name: name, URL: "https://assets.example.test/v0.4.0/" + name})
	}
	listing, err := json.Marshal([]map[string]any{{
		"tag_name": "v0.4.0", "draft": false, "prerelease": false,
		"body": "### Changed\n- reinstall\n", "published_at": "2026-10-01T12:00:00Z",
		"assets": list,
	}})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_, _ = w.Write(listing)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := assets[filepath.Base(r.URL.Path)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(files.Close)
	doer.rewriteTo["api.example.test"] = api.URL
	doer.rewriteTo["assets.example.test"] = files.URL
	return doer
}

// An RH06-era release: the format-2 asset and its signature, and no format-1 asset.
func rh06Assets(t *testing.T) map[string]string {
	return map[string]string{
		ManifestAssetNameV2:          v2Fixture(t),
		ManifestAssetNameV2 + ".sig": `{"format_version":1,"signatures":[]}`,
	}
}

func TestThisControlPlaneReadsTheFormat2Asset(t *testing.T) {
	doer := publishedRelease(t, rh06Assets(t))
	src := NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "", []string{"assets.example.test"})
	listings, err := src.List(context.Background())
	if err != nil || len(listings) != 1 {
		t.Fatalf("list: %v, %+v", err, listings)
	}
	if listings[0].ManifestFormat != ManifestFormat2 {
		t.Fatalf("listing = %+v, want the v2 asset", listings[0])
	}
	raw, err := src.FetchManifest(context.Background(), listings[0].ManifestURL)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := ParseManifestAsset(raw, listings[0].ManifestFormat); err != nil {
		t.Fatalf("the published v2 asset does not parse: %v", err)
	}
}

// While both assets are published (the expand step), the v2 one is read.
func TestTheFormat2AssetWinsOverTheFormat1Asset(t *testing.T) {
	assets := rh06Assets(t)
	assets[ManifestAssetName] = goodManifest
	src := NewGitHubSource(publishedRelease(t, assets), "https://api.example.test", "accreleus/quasar", "", []string{"assets.example.test"})
	listings, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listings[0].ManifestFormat != ManifestFormat2 || !strings.HasSuffix(listings[0].ManifestURL, ManifestAssetNameV2) {
		t.Fatalf("listing = %+v, want the v2 asset", listings[0])
	}

	only1 := map[string]string{ManifestAssetName: goodManifest}
	src = NewGitHubSource(publishedRelease(t, only1), "https://api.example.test", "accreleus/quasar", "", []string{"assets.example.test"})
	listings, err = src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if listings[0].ManifestFormat != ManifestFormat1 {
		t.Fatalf("a release with only the format-1 asset: %+v", listings[0])
	}
}

// The pre-RH06 detector lists a release only when it fetches and parses
// platform-release-manifest.json; an RH06-era release publishes none, and its v2
// document would not parse there either.
func TestAPreRH06ControlPlaneSeesNothingForAFormat2Release(t *testing.T) {
	doer := publishedRelease(t, rh06Assets(t))
	old := prerh06.NewGitHubSource(doer, "https://api.example.test", "accreleus/quasar", "", []string{"assets.example.test"})
	listings, err := old.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, l := range listings {
		if l.ManifestURL != "" {
			t.Fatalf("the old reader found a manifest asset: %+v", l)
		}
		if _, err := old.FetchManifest(context.Background(), l.ManifestURL); err == nil {
			t.Fatal("the old reader fetched a manifest for a release that publishes none it knows")
		}
	}
	if _, err := prerh06.ParseManifest([]byte(v2Fixture(t))); err == nil {
		t.Fatal("the old reader accepted a format-2 document")
	}
}

// An RH06-era edge build is published under `o2-<branch>` and `sha-<short>` only. The
// old edge reader resolves `<branch>` and finds nothing; this one resolves the build.
func TestAPreRH06ControlPlaneSeesNothingForTheNewEdgeTags(t *testing.T) {
	registry := &fakeInspector{byRef: map[string]images.ImageConfig{}}
	for _, image := range []string{"quasar-control-plane", "quasar-node-agent", "quasar-recovery"} {
		cfg := images.ImageConfig{ManifestDigest: digestCP, Labels: labels(commitA, "2026-10-01T12:00:00Z", "96")}
		for _, tag := range []string{BranchTag("develop"), CommitTag(commitA)} {
			registry.byRef["ghcr.io/accreleus/quasar/"+image+":"+tag] = cfg
		}
	}

	if _, err := prerh06.NewRegistryEdgeSource(registry, "", "").Resolve(context.Background(), "develop"); err == nil {
		t.Fatal("the old edge reader resolved an RH06-era branch build")
	}
	build, err := NewRegistryEdgeSource(registry, "", "").Resolve(context.Background(), "develop")
	if err != nil || build.SourceCommit != commitA || build.Tag != "o2-develop" {
		t.Fatalf("this control plane's edge read: %+v, %v", build, err)
	}
}

// BranchTag is the twin of the images workflow's branch-tag rule: every platform image
// is tagged in the o2- family and none under the bare branch name that old control
// planes resolve.
func TestBranchTagMatchesTheImagesWorkflow(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".github", "workflows", "images.yml"))
	if err != nil {
		t.Fatalf("read the images workflow: %v", err)
	}
	text := string(raw)
	for _, image := range []string{"quasar-control-plane", "quasar-node-agent", "quasar-recovery"} {
		at := strings.Index(text, "images: ${{ env.REGISTRY_NS }}/"+image+"\n")
		if at < 0 {
			t.Errorf("%s: no metadata step in the images workflow", image)
			continue
		}
		block := text[at:]
		if end := strings.Index(block, "flavor:"); end > 0 {
			block = block[:end]
		}
		if !strings.Contains(block, "type=ref,event=branch,prefix="+EdgeTagPrefix+"\n") {
			t.Errorf("%s: branch builds are not tagged %s<branch>:\n%s", image, EdgeTagPrefix, block)
		}
		if strings.Contains(block, "type=ref,event=branch\n") {
			t.Errorf("%s: branch builds still move the bare branch tag old control planes resolve", image)
		}
	}
}
