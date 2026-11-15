package compat

// github_test.go — add-by-tag's T1–T4 checks and the available list's fetch, cache and
// single-flight, against a fake GitHub. semantics: control-api.md §Compatibility tools.

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGitHub stands in for every GitHub host. An httptest server is loopback and plain http, which
// the real outbound client refuses by design, so requests go through this Doer: it keeps the
// client's host allowlist real and hands each request to serve with the host it was meant for.
type fakeGitHub struct {
	t     *testing.T
	srv   *httptest.Server
	serve func(host string, w http.ResponseWriter, r *http.Request)
	// extra hosts the fake answers though the allowlist would refuse them, so a test can show a
	// redirect there is refused by the caller's own check, before any request.
	extra map[string]bool
	mu    sync.Mutex
	hits  map[string]int
	reqs  []*http.Request
}

func newFakeGitHub(t *testing.T, serve func(host string, w http.ResponseWriter, r *http.Request)) (*fakeGitHub, *GitHub) {
	f := &fakeGitHub{t: t, serve: serve, hits: map[string]int{}, extra: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(r.Header.Get("X-Fake-Host"), w, r)
	}))
	t.Cleanup(f.srv.Close)
	g := &GitHub{doer: f, log: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	return f, g
}

func (f *fakeGitHub) Do(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if _, ok := githubHosts[host]; !ok && !f.extra[host] {
		return nil, fmt.Errorf("outbound: host %q is not in the allowlist", host)
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("outbound: %q is not https", req.URL)
	}
	f.mu.Lock()
	f.hits[host]++
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	t, _ := url.Parse(f.srv.URL)
	u := *req.URL
	u.Scheme, u.Host = t.Scheme, t.Host
	out, _ := http.NewRequestWithContext(req.Context(), req.Method, u.String(), nil)
	out.Header = req.Header.Clone()
	out.Header.Set("X-Fake-Host", host)
	resp, err := (&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		// The real client never adds Accept-Encoding itself, so neither may the fake's transport.
		Transport: &http.Transport{DisableCompression: true},
	}).Do(out)
	if err == nil {
		resp.Body = boundBody(resp.Body)
	}
	return resp, err
}

// boundBody mirrors the outbound client's 1 MiB wire cap.
func boundBody(rc io.ReadCloser) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, maxListBytes+1), rc}
}

func (f *fakeGitHub) hit(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[host]
}

const (
	addTag   = "GE-Proton11-13"
	addAsset = addTag + "-x86_64.tar.gz"
	addSum   = addTag + "-x86_64.sha512sum"
)

var addSHA = strings.Repeat("ab", 64)

func releaseJSON(tag string, assets map[string]int64) string {
	var as []string
	for n, s := range assets {
		as = append(as, fmt.Sprintf(`{"name":%q,"size":%d,"browser_download_url":"https://evil.example/x"}`, n, s))
	}
	return fmt.Sprintf(`{"tag_name":%q,"draft":false,"prerelease":false,"name":"<script>","assets":[%s]}`, tag, strings.Join(as, ","))
}

// release serves a GitHub that publishes tag with assets, a checksum file body sum, and the
// redirect github.com answers each download with (to the asset host, as GitHub does today).
func release(tag string, assets map[string]int64, sum string) func(string, http.ResponseWriter, *http.Request) {
	return func(host string, w http.ResponseWriter, r *http.Request) {
		switch {
		case host == "api.github.com" && r.URL.Path == "/repos/"+project+"/releases/tags/"+tag:
			io.WriteString(w, releaseJSON(tag, assets))
		case host == "api.github.com":
			http.NotFound(w, r)
		case host == "github.com" && strings.HasPrefix(r.URL.Path, releasePath+tag+"/"):
			w.Header().Set("Location", "https://release-assets.githubusercontent.com/github-production-release-asset/1?sp=r&sig=x&f="+
				url.QueryEscape(strings.TrimPrefix(r.URL.Path, releasePath+tag+"/")))
			w.WriteHeader(http.StatusFound)
		case host == "release-assets.githubusercontent.com":
			if !strings.HasSuffix(r.URL.Query().Get("f"), ".sha512sum") || sum == "" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, sum)
		default:
			http.NotFound(w, r)
		}
	}
}

func goodAssets() map[string]int64 { return map[string]int64{addAsset: 566859228, addSum: 160} }

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var u *UnverifiableError
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want compat_tool_unverifiable", err)
	}
	return u.Reason
}

func TestAValidTagIsPinnedToItsReleaseChecksumFile(t *testing.T) {
	f, g := newFakeGitHub(t, release(addTag, goodAssets(), addSHA+"  "+addAsset+"\n"))
	pin, err := g.Resolve(context.Background(), addTag)
	if err != nil {
		t.Fatal(err)
	}
	if pin != (Pin{Tag: addTag, AssetName: addAsset, SHA512: addSHA, SizeBytes: 566859228}) {
		t.Fatalf("pin = %+v", pin)
	}
	if f.hit("release-assets.githubusercontent.com") != 1 || f.hit("api.github.com") != 1 || f.hit("github.com") != 1 {
		t.Errorf("hits = %v, want one metadata read, one download and one redirect hop", f.hits)
	}
	for _, r := range f.reqs {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("%s: Authorization=%q Accept-Encoding=%q", r.URL, r.Header.Get("Authorization"), r.Header.Get("Accept-Encoding"))
		}
		if strings.Contains(r.URL.String(), "evil.example") {
			t.Errorf("fetched the release's browser_download_url %s; T1 builds every URL from the tag", r.URL)
		}
	}

	// A release before GE-Proton 11 ships <tag>.tar.gz, and its checksum line may use binary mode.
	plain := "GE-Proton9-27"
	_, g = newFakeGitHub(t, release(plain, map[string]int64{plain + ".tar.gz": 10, plain + ".sha512sum": 1}, addSHA+" *"+plain+".tar.gz"))
	if pin, err := g.Resolve(context.Background(), plain); err != nil || pin.AssetName != plain+".tar.gz" {
		t.Fatalf("plain asset: %+v %v", pin, err)
	}
}

func TestAMissingChecksumFileIsRefused(t *testing.T) {
	// Not listed in the release: nothing is downloaded.
	f, g := newFakeGitHub(t, release(addTag, map[string]int64{addAsset: 5, addTag + ".sha512sum": 1}, addSHA+"  "+addAsset))
	if got := reasonOf(t, func() error { _, err := g.Resolve(context.Background(), addTag); return err }()); got != "checksum_missing" {
		t.Errorf("unlisted checksum: %s", got)
	}
	if f.hit("github.com") != 0 {
		t.Error("fetched a checksum file the release does not list")
	}
	// Listed, but the download answers 404.
	_, g = newFakeGitHub(t, release(addTag, goodAssets(), ""))
	if got := reasonOf(t, func() error { _, err := g.Resolve(context.Background(), addTag); return err }()); got != "checksum_missing" {
		t.Errorf("404 checksum: %s", got)
	}
}

func TestAChecksumFileThatDoesNotMatchTheAssetIsRefused(t *testing.T) {
	for name, sum := range map[string]string{
		"names another file":  addSHA + "  " + addTag + ".tar.gz\n",
		"names another tag":   addSHA + "  GE-Proton11-12-x86_64.tar.gz\n",
		"two lines":           addSHA + "  " + addAsset + "\n" + addSHA + "  " + addAsset + "\n",
		"uppercase hex":       strings.ToUpper(addSHA) + "  " + addAsset + "\n",
		"short digest":        addSHA[:126] + "  " + addAsset + "\n",
		"sha256 length":       addSHA[:64] + "  " + addAsset + "\n",
		"one space":           addSHA + " " + addAsset + "\n",
		"empty":               "",
		"over 4 KiB":          addSHA + "  " + addAsset + strings.Repeat(" ", 5000),
		"trailing blank line": addSHA + "  " + addAsset + "\n\n",
		"CRLF":                addSHA + "  " + addAsset + "\r\n",
	} {
		if sum == "" {
			sum = "\n"
		}
		_, g := newFakeGitHub(t, release(addTag, goodAssets(), sum))
		_, err := g.Resolve(context.Background(), addTag)
		if got := reasonOf(t, err); got != "checksum_malformed" {
			t.Errorf("%s: %s", name, got)
		}
	}
}

func TestATagNotFoundIsRefused(t *testing.T) {
	_, g := newFakeGitHub(t, release("GE-Proton11-12", goodAssets(), addSHA+"  "+addAsset))
	_, err := g.Resolve(context.Background(), addTag)
	if got := reasonOf(t, err); got != "release_not_found" {
		t.Errorf("got %s", got)
	}
	// The release exists but carries neither asset, or an asset over the 2 GiB cap.
	_, g = newFakeGitHub(t, release(addTag, map[string]int64{addTag + "-x86_64.tar.xz": 5}, ""))
	if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != "asset_not_found" {
		t.Errorf("no asset: %v", err)
	}
	_, g = newFakeGitHub(t, release(addTag, map[string]int64{addAsset: maxAssetBytes + 1, addSum: 1}, addSHA+"  "+addAsset))
	if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != "asset_too_large" {
		t.Errorf("too large: %v", err)
	}
}

func TestAPrereleaseOrDraftIsRefused(t *testing.T) {
	for _, field := range []string{"prerelease", "draft"} {
		inner := release(addTag, goodAssets(), addSHA+"  "+addAsset)
		f, g := newFakeGitHub(t, func(host string, w http.ResponseWriter, r *http.Request) {
			if host == "api.github.com" {
				io.WriteString(w, strings.Replace(releaseJSON(addTag, goodAssets()), `"`+field+`":false`, `"`+field+`":true`, 1))
				return
			}
			inner(host, w, r)
		})
		if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != "release_not_found" || f.hit("github.com") != 0 {
			t.Errorf("%s: %v after %d checksum requests", field, err, f.hit("github.com"))
		}
	}
}

func TestAMissingTagIsRememberedForFiveMinutes(t *testing.T) {
	f, g := newFakeGitHub(t, release("GE-Proton1-1", goodAssets(), ""))
	c := &clock{t: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)}
	g.now = c.now
	for i := 0; i < 3; i++ {
		if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != "release_not_found" {
			t.Fatalf("try %d: %v", i, err)
		}
		c.add(time.Minute)
	}
	if n := f.hit("api.github.com"); n != 1 {
		t.Errorf("%d requests for a missing tag within 5 minutes, want 1", n)
	}
	c.add(2 * time.Minute)
	g.Resolve(context.Background(), addTag)
	if n := f.hit("api.github.com"); n != 2 {
		t.Errorf("%d requests after 5 minutes, want 2", n)
	}
}

func TestResolveAndTheListShareOneRateLimitWindow(t *testing.T) {
	var limited atomic.Bool
	limited.Store(true)
	inner := release(addTag, goodAssets(), addSHA+"  "+addAsset)
	f, g := newFakeGitHub(t, func(host string, w http.ResponseWriter, r *http.Request) {
		if host == "api.github.com" && limited.Load() {
			w.Header().Set("Retry-After", "1800")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if host == "api.github.com" && r.URL.Path == "/repos/"+project+"/releases" {
			io.WriteString(w, listJSON(ok("GE-Proton11-20", "")))
			return
		}
		inner(host, w, r)
	})
	c := &clock{t: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)}
	g.now = c.now
	if _, err := g.Resolve(context.Background(), addTag); !errors.Is(err, ErrUpstream) {
		t.Fatalf("rate-limited add: %v", err)
	}
	limited.Store(false)
	c.add(10 * time.Minute)
	if _, err := g.Resolve(context.Background(), addTag); !errors.Is(err, ErrUpstream) {
		t.Errorf("add inside the window: %v", err)
	}
	out, _ := g.Latest(context.Background())
	if out.reason != "rate_limited" || f.hit("api.github.com") != 1 {
		t.Errorf("list inside an add's window: %q after %d requests, want rate_limited and no request", out.reason, f.hit("api.github.com"))
	}
	c.add(21 * time.Minute)
	if _, err := g.Resolve(context.Background(), addTag); err != nil {
		t.Errorf("add after the window: %v", err)
	}

	// And the other way: a rate-limited list holds adds back.
	f2, g2, c2 := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1200")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	g2.Latest(context.Background())
	c2.add(time.Minute)
	if _, err := g2.Resolve(context.Background(), addTag); !errors.Is(err, ErrUpstream) || f2.hit("api.github.com") != 1 {
		t.Errorf("add inside the list's window: %v after %d requests", err, f2.hit("api.github.com"))
	}
}

// A tag row binds only its own asset stem (Converger.bindName), which is the name every shipped
// GE-Proton entry registers.
func TestShippedNamesAreTheirAssetStems(t *testing.T) {
	for _, r := range recommended {
		if r.SteamName != strings.TrimSuffix(r.Asset, ".tar.gz") || !strings.HasPrefix(r.Asset, r.Tag) {
			t.Errorf("%s: steam_name %s, asset %s", r.Tag, r.SteamName, r.Asset)
		}
	}
}

func TestANonGitHubRedirectIsRefused(t *testing.T) {
	for _, loc := range []string{
		"https://evil.example/" + addSum,
		"https://api.github.com/" + addSum, // allowlisted for metadata, never a redirect target
		"https://github.com/evil/proton-ge-custom/releases/download/" + addTag + "/" + addSum,
		"https://github.com" + releasePath + "GE-Proton11-12/" + addSum,
		"https://github.com" + releasePath + addTag + "/../GE-Proton11-12/" + addSum,
		"https://github.com" + releasePath + addTag + "/%2e%2e/x",
		"http://release-assets.githubusercontent.com/x",
		"https://release-assets.githubusercontent.com.evil.example/x",
		"https://release-assets.githubusercontent.com:8443/x",
		"https://user@release-assets.githubusercontent.com/x",
	} {
		f, g := newFakeGitHub(t, nil)
		f.extra["evil.example"], f.extra["release-assets.githubusercontent.com.evil.example"] = true, true
		inner := release(addTag, goodAssets(), addSHA+"  "+addAsset)
		f.serve = func(host string, w http.ResponseWriter, r *http.Request) {
			if host == "github.com" && strings.HasPrefix(r.URL.Path, releasePath+addTag+"/") {
				w.Header().Set("Location", loc)
				w.WriteHeader(http.StatusFound)
				return
			}
			inner(host, w, r)
		}
		_, err := g.Resolve(context.Background(), addTag)
		if got := reasonOf(t, err); got != reasonRedirectRefused {
			t.Errorf("%s: %s", loc, got)
		}
		if n := len(f.reqs); n != 2 {
			t.Errorf("%s: %d requests, want the metadata and the first hop only", loc, n)
		}
	}

	// Release metadata follows no redirect at all.
	f, g := newFakeGitHub(t, func(host string, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://api.github.com/repos/other/repo/releases/tags/"+addTag)
		w.WriteHeader(http.StatusMovedPermanently)
	})
	if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != reasonRedirectRefused || len(f.reqs) != 1 {
		t.Errorf("metadata redirect: %v after %d requests", err, len(f.reqs))
	}
}

func TestFiveRedirectsAreFollowedAndASixthIsRefused(t *testing.T) {
	chain := func(n int) func(string, http.ResponseWriter, *http.Request) {
		inner := release(addTag, goodAssets(), addSHA+"  "+addAsset)
		return func(host string, w http.ResponseWriter, r *http.Request) {
			if host == "github.com" {
				hop := strings.Count(r.URL.Path, "/x")
				if hop < n-1 {
					w.Header().Set("Location", "https://github.com"+releasePath+addTag+strings.Repeat("/x", hop+1)+"/"+addSum)
				} else {
					w.Header().Set("Location", "https://objects.githubusercontent.com/a?f="+addSum)
				}
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if host == "objects.githubusercontent.com" {
				io.WriteString(w, addSHA+"  "+addAsset)
				return
			}
			inner(host, w, r)
		}
	}
	_, g := newFakeGitHub(t, chain(5))
	if _, err := g.Resolve(context.Background(), addTag); err != nil {
		t.Errorf("5 redirects: %v", err)
	}
	_, g = newFakeGitHub(t, chain(6))
	if _, err := g.Resolve(context.Background(), addTag); reasonOf(t, err) != reasonRedirectRefused {
		t.Errorf("6 redirects: %v", err)
	}
}

func TestUpstreamFailuresAreRetryableAndStoreNothing(t *testing.T) {
	for name, serve := range map[string]func(string, http.ResponseWriter, *http.Request){
		"5xx": func(_ string, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"429": func(_ string, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) },
		"403 limit": func(_ string, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(403)
		},
		"not JSON": func(_ string, w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "<html>") },
		"checksum 5xx": func(h string, w http.ResponseWriter, r *http.Request) {
			if h == "release-assets.githubusercontent.com" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			release(addTag, goodAssets(), "x")(h, w, r)
		},
	} {
		_, g := newFakeGitHub(t, serve)
		if _, err := g.Resolve(context.Background(), addTag); !errors.Is(err, ErrUpstream) {
			t.Errorf("%s: %v, want upstream_unavailable", name, err)
		}
	}
	f, g := newFakeGitHub(t, nil)
	f.srv.Close() // unreachable
	if _, err := g.Resolve(context.Background(), addTag); !errors.Is(err, ErrUpstream) {
		t.Errorf("unreachable: %v", err)
	}
}

// --- the available list ---------------------------------------------------------------------

type rel struct {
	Tag, Published string
	Draft, Pre     bool
	Assets         []string
}

func listJSON(rs ...rel) string {
	var out []map[string]any
	for _, r := range rs {
		var as []map[string]any
		for _, a := range r.Assets {
			as = append(as, map[string]any{"name": a, "size": 1})
		}
		m := map[string]any{"tag_name": r.Tag, "draft": r.Draft, "prerelease": r.Pre, "assets": as, "published_at": nil}
		if r.Published != "" {
			m["published_at"] = r.Published
		}
		out = append(out, m)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func ok(tag, published string) rel {
	return rel{Tag: tag, Published: published, Assets: []string{tag + "-x86_64.tar.gz", tag + "-x86_64.sha512sum"}}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func latestGitHub(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) (*fakeGitHub, *GitHub, *clock) {
	f, g := newFakeGitHub(t, func(host string, w http.ResponseWriter, r *http.Request) {
		if host != "api.github.com" || r.URL.Path != "/repos/"+project+"/releases" || r.URL.RawQuery != "per_page=10" {
			t.Errorf("unexpected request %s %s?%s", host, r.URL.Path, r.URL.RawQuery)
		}
		serve(w, r)
	})
	c := &clock{t: time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)}
	g.now = c.now
	return f, g, c
}

func TestTheGitHubListIsFilteredDedupedAndCut(t *testing.T) {
	rec := recommended[0].Tag
	_, g, _ := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, listJSON(
			ok("GE-Proton11-20", "2026-11-01T00:00:00Z"),
			ok("GE-Proton11-20", "2026-11-09T00:00:00Z"), // a duplicate: the first in the answer wins
			rel{Tag: "GE-Proton11-19", Published: "2026-11-08T00:00:00Z", Draft: true, Assets: ok("GE-Proton11-19", "").Assets},
			rel{Tag: "GE-Proton11-18", Published: "2026-11-08T00:00:00Z", Pre: true, Assets: ok("GE-Proton11-18", "").Assets},
			ok("Proton-11.0", "2026-11-08T00:00:00Z"),
			// The picked asset is x86_64, and it has no checksum file, though the plain one has.
			rel{Tag: "GE-Proton11-17", Published: "2026-11-08T00:00:00Z", Assets: []string{"GE-Proton11-17-x86_64.tar.gz", "GE-Proton11-17.tar.gz", "GE-Proton11-17.sha512sum"}},
			ok(rec, "2026-10-30T18:02:11Z"),
			ok("GE-Proton11-16", "2026-10-31T00:00:00Z"),
			ok("GE-Proton11-15", "not a date"),
			ok("GE-Proton11-14", "2026-10-31T00:00:00Z"),
			ok("GE-Proton11-11", "2026-10-20T00:00:00+10:00"),
			ok("GE-Proton11-10", "2026-10-19T00:00:00Z"),
			rel{Tag: "GE-Proton9-27", Published: "2026-10-18T00:00:00Z", Assets: []string{"GE-Proton9-27.tar.gz", "GE-Proton9-27.sha512sum"}},
		))
	})
	out, err := g.Latest(context.Background())
	if err != nil || out.reason != "" {
		t.Fatalf("%v %q", err, out.reason)
	}
	var got []string
	for _, r := range out.releases {
		p := "null"
		if r.Published != nil {
			p = *r.Published
		}
		got = append(got, r.Tag+"@"+p)
	}
	want := []string{
		"GE-Proton11-20@2026-11-01T00:00:00Z",
		"GE-Proton11-16@2026-10-31T00:00:00Z", // equal dates: tag descending
		"GE-Proton11-14@2026-10-31T00:00:00Z",
		"GE-Proton11-11@2026-10-20T00:00:00+10:00",
		"GE-Proton11-10@2026-10-19T00:00:00Z",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("latest =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if out.recPublished[rec] != "2026-10-30T18:02:11Z" {
		t.Errorf("recommended published_at = %q", out.recPublished[rec])
	}
}

func TestASuccessfulListIsCachedForAnHour(t *testing.T) {
	f, g, c := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, listJSON(ok("GE-Proton11-20", ""))) })
	for i := 0; i < 3; i++ {
		if _, err := g.Latest(context.Background()); err != nil {
			t.Fatal(err)
		}
		c.add(19 * time.Minute)
	}
	if n := f.hit("api.github.com"); n != 1 {
		t.Errorf("%d upstream requests in 57 minutes, want 1", n)
	}
	c.add(4 * time.Minute)
	out, _ := g.Latest(context.Background())
	if n := f.hit("api.github.com"); n != 2 || !out.fetchedAt.Equal(c.now()) {
		t.Errorf("after an hour: %d requests, fetched_at %v", n, out.fetchedAt)
	}
}

func TestAFailureIsCachedForFiveMinutesAndNeverServesAStaleList(t *testing.T) {
	var status atomic.Int32
	status.Store(200)
	f, g, c := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		if s := int(status.Load()); s != 200 {
			w.WriteHeader(s)
			return
		}
		io.WriteString(w, listJSON(ok("GE-Proton11-20", "")))
	})
	g.Latest(context.Background())
	c.add(time.Hour)
	status.Store(502)
	out, _ := g.Latest(context.Background())
	if out.reason != "upstream_error" || out.releases != nil {
		t.Fatalf("after a failed refetch: reason %q, %d releases; a stale list must not be served", out.reason, len(out.releases))
	}
	c.add(4 * time.Minute)
	g.Latest(context.Background())
	if n := f.hit("api.github.com"); n != 2 {
		t.Errorf("%d requests within the failure window, want 2", n)
	}
	c.add(time.Minute)
	g.Latest(context.Background())
	if n := f.hit("api.github.com"); n != 3 {
		t.Errorf("%d requests after five minutes, want 3", n)
	}
}

func TestEachFailureHasOneReason(t *testing.T) {
	big := listJSON(ok("GE-Proton11-20", "")) + strings.Repeat(" ", maxListBytes)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	io.WriteString(zw, big) // about 1 KiB on the wire, over 1 MiB decoded
	zw.Close()
	var small bytes.Buffer
	zw = gzip.NewWriter(&small)
	io.WriteString(zw, listJSON(ok("GE-Proton11-20", "")))
	zw.Close()
	for name, c := range map[string]struct {
		serve  func(w http.ResponseWriter)
		reason string
	}{
		"429":           {func(w http.ResponseWriter) { w.WriteHeader(429) }, "rate_limited"},
		"403 exhausted": {func(w http.ResponseWriter) { w.Header().Set("X-RateLimit-Remaining", "0"); w.WriteHeader(403) }, "rate_limited"},
		"403 other":     {func(w http.ResponseWriter) { w.WriteHeader(403) }, "upstream_error"},
		"302": {func(w http.ResponseWriter) {
			w.Header().Set("Location", "https://api.github.com/x")
			w.WriteHeader(302)
		}, "upstream_error"},
		"over 1 MiB":     {func(w http.ResponseWriter) { io.WriteString(w, big) }, "response_invalid"},
		"gzip bomb":      {func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "gzip"); w.Write(gz.Bytes()) }, "response_invalid"},
		"brotli":         {func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "br"); io.WriteString(w, "[]") }, "response_invalid"},
		"not JSON":       {func(w http.ResponseWriter) { io.WriteString(w, "<html>") }, "response_invalid"},
		"not an array":   {func(w http.ResponseWriter) { io.WriteString(w, `{"message":"x"}`) }, "response_invalid"},
		"array of junk":  {func(w http.ResponseWriter) { io.WriteString(w, `[1,2]`) }, "response_invalid"},
		"gzip under cap": {func(w http.ResponseWriter) { w.Header().Set("Content-Encoding", "gzip"); w.Write(small.Bytes()) }, ""},
	} {
		_, g, _ := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) { c.serve(w) })
		out, err := g.Latest(context.Background())
		if err != nil || out.reason != c.reason {
			t.Errorf("%s: reason %q err %v, want %q", name, out.reason, err, c.reason)
		}
	}
	f, g, _ := latestGitHub(t, func(http.ResponseWriter, *http.Request) {})
	f.srv.Close()
	if out, _ := g.Latest(context.Background()); out.reason != "unreachable" {
		t.Errorf("closed server: %q", out.reason)
	}
}

func TestARateLimitIsServedUntilGitHubsResetWithinAnHour(t *testing.T) {
	now := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		h    map[string]string
		want time.Duration
	}{
		"no header":            {nil, 5 * time.Minute},
		"retry-after 30 min":   {map[string]string{"Retry-After": "1800"}, 30 * time.Minute},
		"retry-after 2 h":      {map[string]string{"Retry-After": "7200"}, time.Hour},
		"retry-after 1 min":    {map[string]string{"Retry-After": "60"}, 5 * time.Minute},
		"retry-after negative": {map[string]string{"Retry-After": "-5"}, 5 * time.Minute},
		"retry-after date":     {map[string]string{"Retry-After": "Sat, 07 Nov 2026 01:40:00 GMT"}, 5 * time.Minute},
		"reset 20 min":         {map[string]string{"X-RateLimit-Reset": fmt.Sprint(now.Add(20 * time.Minute).Unix())}, 20 * time.Minute},
		"reset past":           {map[string]string{"X-RateLimit-Reset": fmt.Sprint(now.Add(-time.Hour).Unix())}, 5 * time.Minute},
		"reset junk":           {map[string]string{"X-RateLimit-Reset": "soon"}, 5 * time.Minute},
		"retry-after wins":     {map[string]string{"Retry-After": "900", "X-RateLimit-Reset": fmt.Sprint(now.Add(40 * time.Minute).Unix())}, 15 * time.Minute},
	} {
		h := http.Header{}
		for k, v := range c.h {
			h.Set(k, v)
		}
		if got := rateLimitWindow(h, now).Sub(now); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

func TestConcurrentMissesShareOneFetchThatOutlivesAnAbortedCaller(t *testing.T) {
	release := make(chan struct{})
	f, g, _ := latestGitHub(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		io.WriteString(w, listJSON(ok("GE-Proton11-20", "")))
	})
	// The caller that starts the fetch gives up first.
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := g.Latest(ctx); first <- err }()
	for f.hit("api.github.com") == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted caller: %v", err)
	}
	var wg sync.WaitGroup
	results := make([]*latestOutcome, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], _ = g.Latest(context.Background()) }(i)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := f.hit("api.github.com"); n != 1 {
		t.Errorf("%d upstream requests for 11 concurrent callers, want 1", n)
	}
	for i, r := range results {
		if r == nil || r.reason != "" || len(r.releases) != 1 {
			t.Fatalf("caller %d: %+v; the abort must not be cached as a failure", i, r)
		}
	}
}

func TestRedirectAllowedMatchesTheAgent(t *testing.T) {
	// The same cases as node-agent compat_tools::fetch::tests::redirects_only_to_the_allowlist.
	tag := "GE-Proton11-12"
	for _, u := range []string{
		"https://release-assets.githubusercontent.com/github-production-release-asset/1?sp=r&sig=x",
		"https://objects.githubusercontent.com/a/b",
		"https://objects.githubusercontent.com:443/a",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/x",
	} {
		if err := redirectAllowed(u, tag); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{
		"http://objects.githubusercontent.com/a",
		"https://evil.example/a",
		"https://objects.githubusercontent.com.evil.example/a",
		"https://x.objects.githubusercontent.com/a",
		"https://objects.githubusercontent.com:8443/a",
		"https://user@objects.githubusercontent.com/a",
		"/relative/path",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-13/x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/../GE-Proton11-13/x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/%2e%2e/x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/%2F/x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12//x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/./x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/a\\b",
		"https://github.com/evil/proton-ge-custom/releases/download/GE-Proton11-12/x",
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/x#f",
		"https://api.github.com/repos/GloriousEggroll/proton-ge-custom/releases",
		"https://GITHUB.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/x",
	} {
		if err := redirectAllowed(u, tag); err == nil {
			t.Errorf("%s: allowed", u)
		}
	}
}
