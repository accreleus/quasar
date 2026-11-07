package compat

// The control plane's only reads of github.com for GE-Proton: a release by tag, pinned to the
// SHA-512 in that release's own checksum file (POST /v1/admin/compat-tools), and the latest
// releases list (GET /v1/admin/compat-tools/available). semantics: control-api.md §Compatibility
// tools, "Trust rules" T1–T4 and "Available tools".
//
// Threat model of a tag pin: the checksum file sits in the same GitHub release as the archive, so
// whoever controls the release when the tag is added controls both, and the pin cannot prove the
// release was untampered then. What it does prove is that every host installs exactly the bytes
// pinned at add time and that a later change to the release is caught (the agent's T4 check). A
// recommended entry narrows that window to Quasar's own release process, which is why a tag row
// keeps source "tag" and never becomes "recommended" by being added.

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/accreleus/quasar/control-plane/internal/outbound"
)

const (
	project       = "GloriousEggroll/proton-ge-custom"
	releasePath   = "/" + project + "/releases/download/"
	apiReleases   = "https://api.github.com/repos/" + project + "/releases"
	maxAssetBytes = int64(2) << 30 // T3
	maxSumBytes   = 4 << 10        // T3
	maxListBytes  = 1 << 20        // amendment 57; also bounds a release's metadata
	maxRedirects  = 5              // T2
	githubTimeout = 10 * time.Second
	// resolveTimeout bounds add-by-tag's whole sequence: metadata plus up to six checksum hops.
	resolveTimeout = 15 * time.Second
	// notFoundFor keeps repeated adds of a missing tag off GitHub's 60-an-hour anonymous budget.
	notFoundFor = 5 * time.Minute
	latestKeep  = 5
)

var (
	tagRe = regexp.MustCompile(`^GE-Proton[0-9]{1,3}-[0-9]{1,4}$`)
	// T4: one GNU sha512sum line, an optional trailing newline.
	sumLineRe = regexp.MustCompile(`^([0-9a-f]{128})(?:  | \*)([^\n]+)\n?$`)
)

// githubHosts is the egress allowlist. api.github.com is never a redirect target and the asset
// hosts are never a first request; redirectAllowed narrows each hop further.
var githubHosts = map[string]struct{}{
	"api.github.com": {}, "github.com": {},
	"release-assets.githubusercontent.com": {}, "objects.githubusercontent.com": {},
}

// UnverifiableError is 422 compat_tool_unverifiable; Reason is the message slug.
type UnverifiableError struct{ Reason, detail string }

func (e *UnverifiableError) Error() string { return e.Reason + ": " + e.detail }

func unverifiable(reason, format string, a ...any) error {
	return &UnverifiableError{Reason: reason, detail: fmt.Sprintf(format, a...)}
}

// errUndecodable: a Content-Encoding other than identity or gzip, or a gzip stream that does not open.
var errUndecodable = errors.New("response body in an encoding that was not asked for")

// ErrUpstream is 503 upstream_unavailable: unreachable, 5xx or rate-limited. Nothing is stored.
var ErrUpstream = errors.New("github.com unavailable")

// Pin is what add-by-tag stores: the asset and the SHA-512 and size every agent verifies against.
type Pin struct {
	Tag, AssetName, SHA512 string
	SizeBytes              int64
}

// GitHub reads release metadata and checksum files through the guarded client.
type GitHub struct {
	doer outbound.Doer
	log  *slog.Logger
	now  func() time.Time

	mu       sync.Mutex
	cached   *latestOutcome
	inflight chan struct{}
	notFound map[string]time.Time // tag -> until
	// limitedUntil is api.github.com's rate-limit window, shared by Resolve and Latest: both
	// spend the same anonymous budget.
	limitedUntil time.Time
}

func (g *GitHub) noteRateLimit(h http.Header) {
	until := rateLimitWindow(h, g.now())
	g.mu.Lock()
	if until.After(g.limitedUntil) {
		g.limitedUntil = until
	}
	g.mu.Unlock()
}

// NewGitHub builds the client: https only, the four GitHub hosts, no redirect followed by the
// transport, the connect-time address check, a 1 MiB body cap and a 10 s request deadline.
func NewGitHub(log *slog.Logger) (*GitHub, error) {
	c, err := outbound.New(outbound.Config{AllowHosts: githubHosts, Timeout: githubTimeout, MaxBodyBytes: maxListBytes})
	if err != nil {
		return nil, err
	}
	return &GitHub{doer: c, log: log, now: time.Now}, nil
}

type ghRelease struct {
	TagName     string  `json:"tag_name"`
	Draft       bool    `json:"draft"`
	Prerelease  bool    `json:"prerelease"`
	PublishedAt *string `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// pick is T1's asset choice: <tag>-x86_64.tar.gz, else <tag>.tar.gz; ok reports the picked
// asset's <stem>.sha512sum is listed too.
func (r ghRelease) pick() (name string, size int64, ok, found bool) {
	has := func(n string) (int64, bool) {
		for _, a := range r.Assets {
			if a.Name == n {
				return a.Size, true
			}
		}
		return 0, false
	}
	for _, n := range []string{r.TagName + "-x86_64.tar.gz", r.TagName + ".tar.gz"} {
		if s, f := has(n); f {
			_, sum := has(strings.TrimSuffix(n, ".tar.gz") + ".sha512sum")
			return n, s, sum, true
		}
	}
	return "", 0, false, false
}

func (g *GitHub) get(ctx context.Context, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	if strings.HasPrefix(rawURL, "https://api.github.com/") {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	return g.doer.Do(req)
}

func rateLimited(resp *http.Response) bool {
	return resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0")
}

// readDecoded reads at most limit decoded bytes; a gzip answer sent despite identity is
// decompressed under the same cap, so a small body cannot expand without bound.
func readDecoded(resp *http.Response, limit int64) ([]byte, error) {
	var r io.Reader = resp.Body
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, errUndecodable
		}
		defer zr.Close()
		r = zr
	default:
		return nil, errUndecodable
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = outbound.ErrBodyTooLarge
	}
	return b, err
}

// Resolve is add-by-tag's T1–T4: the release's metadata, the picked asset, then its checksum file,
// fetched once and checked, and nothing else trusted.
func (g *GitHub) Resolve(ctx context.Context, tag string) (Pin, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	g.mu.Lock()
	now := g.now()
	missing, limited := now.Before(g.notFound[tag]), now.Before(g.limitedUntil)
	g.mu.Unlock()
	switch {
	case missing:
		return Pin{}, unverifiable("release_not_found", "no release %s (answered 404 within %v)", tag, notFoundFor)
	case limited:
		return Pin{}, ErrUpstream
	}
	resp, err := g.get(ctx, apiReleases+"/tags/"+tag)
	if err != nil {
		g.log.Warn("compat tools: reading a release failed", "tag", tag, "err", err)
		return Pin{}, ErrUpstream
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusNotFound:
		g.mu.Lock()
		if g.notFound == nil {
			g.notFound = map[string]time.Time{}
		}
		for t, until := range g.notFound {
			if !now.Before(until) {
				delete(g.notFound, t)
			}
		}
		g.notFound[tag] = now.Add(notFoundFor)
		g.mu.Unlock()
		return Pin{}, unverifiable("release_not_found", "no release %s", tag)
	case rateLimited(resp):
		g.noteRateLimit(resp.Header)
		return Pin{}, ErrUpstream
	case outbound.IsRedirect(resp.StatusCode):
		return Pin{}, unverifiable(reasonRedirectRefused, "release metadata answered %d; no redirect is followed there", resp.StatusCode)
	default:
		g.log.Warn("compat tools: reading a release failed", "tag", tag, "status", resp.StatusCode)
		return Pin{}, ErrUpstream
	}
	body, err := readDecoded(resp, maxListBytes)
	var rel ghRelease
	if err == nil {
		err = json.Unmarshal(body, &rel)
	}
	if err != nil {
		g.log.Warn("compat tools: a release's metadata did not decode", "tag", tag, "err", err)
		return Pin{}, ErrUpstream
	}
	if rel.TagName != tag {
		return Pin{}, unverifiable("release_not_found", "github answered for %q", rel.TagName)
	}
	if rel.Draft || rel.Prerelease { // the available list never offers one either
		return Pin{}, unverifiable("release_not_found", "%s is a draft or prerelease", tag)
	}
	name, size, hasSum, found := rel.pick()
	switch {
	case !found || size < 1:
		return Pin{}, unverifiable("asset_not_found", "no usable %s-x86_64.tar.gz or %s.tar.gz", tag, tag)
	case size > maxAssetBytes:
		return Pin{}, unverifiable("asset_too_large", "%s is %d bytes", name, size)
	case !hasSum:
		return Pin{}, unverifiable("checksum_missing", "the release lists no checksum file for %s", name)
	}
	sha, err := g.checksum(ctx, tag, name)
	if err != nil {
		return Pin{}, err
	}
	return Pin{Tag: tag, AssetName: name, SHA512: sha, SizeBytes: size}, nil
}

// reasonRedirectRefused is the agent's own reason word for T2; control-api.md lists no
// compat_tool_unverifiable message for it (raised with the 0.6 lead).
const reasonRedirectRefused = "redirect_refused"

// checksum fetches <asset stem>.sha512sum from the release's download path, following at most 5
// redirects, each one checked raw before any URL parsing (T2).
func (g *GitHub) checksum(ctx context.Context, tag, asset string) (string, error) {
	next := "https://github.com" + releasePath + tag + "/" + strings.TrimSuffix(asset, ".tar.gz") + ".sha512sum"
	for hop := 0; ; hop++ {
		resp, err := g.get(ctx, next)
		if err != nil {
			g.log.Warn("compat tools: fetching a checksum file failed", "tag", tag, "err", err)
			return "", ErrUpstream
		}
		if outbound.IsRedirect(resp.StatusCode) {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if hop == maxRedirects {
				return "", unverifiable(reasonRedirectRefused, "more than %d redirects", maxRedirects)
			}
			if err := redirectAllowed(loc, tag); err != nil {
				return "", unverifiable(reasonRedirectRefused, "%v", err)
			}
			next = loc
			continue
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusOK:
		case resp.StatusCode == http.StatusNotFound:
			return "", unverifiable("checksum_missing", "the checksum file for %s answered 404", asset)
		default:
			g.log.Warn("compat tools: fetching a checksum file failed", "tag", tag, "status", resp.StatusCode)
			return "", ErrUpstream
		}
		body, err := readDecoded(resp, maxSumBytes)
		if errors.Is(err, outbound.ErrBodyTooLarge) || errors.Is(err, errUndecodable) {
			return "", unverifiable("checksum_malformed", "the checksum file is over %d bytes or not plain text", maxSumBytes)
		}
		if err != nil {
			g.log.Warn("compat tools: reading a checksum file failed", "tag", tag, "err", err)
			return "", ErrUpstream
		}
		m := sumLineRe.FindSubmatch(body)
		if m == nil {
			return "", unverifiable("checksum_malformed", "not one sha512sum line")
		}
		if string(m[2]) != asset {
			return "", unverifiable("checksum_malformed", "the checksum file names %q, not %q", m[2], asset)
		}
		return string(m[1]), nil
	}
}

// redirectAllowed checks a raw Location before any URL library sees it, since a normalizing
// parser would clean `..` or `%2e` into a match. Twin of node-agent compat_tools::fetch::redirect_allowed.
func redirectAllowed(raw, tag string) error {
	for i := 0; i < len(raw); i++ {
		if c := raw[i]; c < 0x20 || c == 0x7f || c == ' ' || c == '\\' || c == '#' {
			return errors.New("Location holds a control character, space, backslash or fragment")
		}
	}
	if _, err := url.Parse(raw); err != nil {
		return fmt.Errorf("Location does not parse: %w", err)
	}
	rest, ok := strings.CutPrefix(raw, "https://")
	if !ok {
		return errors.New("Location is not an absolute https URL")
	}
	end := strings.IndexAny(rest, "/?")
	if end < 0 {
		end = len(rest)
	}
	authority, tail := rest[:end], rest[end:]
	if strings.Contains(authority, "@") {
		return errors.New("Location carries userinfo")
	}
	host := authority
	if h, port, found := strings.Cut(authority, ":"); found {
		if port != "443" {
			return errors.New("Location names a port other than 443")
		}
		host = h
	}
	if host == "release-assets.githubusercontent.com" || host == "objects.githubusercontent.com" {
		return nil
	}
	if host != "github.com" {
		return fmt.Errorf("Location host %q is not allowed", host)
	}
	path, _, _ := strings.Cut(tail, "?")
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") {
		return errors.New("Location path holds an encoded dot or slash")
	}
	if !strings.HasPrefix(path, "/") {
		return errors.New("Location path is outside the run's release")
	}
	for _, s := range strings.Split(path[1:], "/") {
		if s == "" || s == "." || s == ".." {
			return errors.New("Location path has an empty, `.` or `..` segment")
		}
	}
	if !strings.HasPrefix(path, releasePath+tag+"/") {
		return errors.New("Location path is outside the run's release")
	}
	return nil
}

// latestRelease is one github_latest entry, or a recommended tag's published_at.
type latestRelease struct {
	Tag       string
	Published *string // RFC 3339, else nil
}

type latestOutcome struct {
	releases     []latestRelease   // up to 5, recommended tags dropped, ordered
	recPublished map[string]string // recommended tag -> published_at
	reason       string            // "" on success
	fetchedAt    time.Time
	validUntil   time.Time
}

// Latest is the GitHub half of the available list: the cached outcome while it is within its
// window, else one shared fetch under its own context. A caller that aborts only stops waiting.
func (g *GitHub) Latest(ctx context.Context) (*latestOutcome, error) {
	for {
		g.mu.Lock()
		if c := g.cached; c != nil && g.now().Before(c.validUntil) {
			g.mu.Unlock()
			return c, nil
		}
		wait := g.inflight
		if wait == nil {
			wait = make(chan struct{})
			g.inflight = wait
			go g.fetchLatest(wait)
		}
		g.mu.Unlock()
		select {
		case <-wait:
			g.mu.Lock()
			c := g.cached
			g.mu.Unlock()
			if c != nil {
				return c, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (g *GitHub) fetchLatest(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), githubTimeout)
	defer cancel()
	out := g.fetchLatestOnce(ctx)
	g.mu.Lock()
	g.cached, g.inflight = out, nil
	g.mu.Unlock()
	close(done)
}

func (g *GitHub) fetchLatestOnce(ctx context.Context) *latestOutcome {
	now := g.now()
	fail := func(reason string, until time.Time, detail ...any) *latestOutcome {
		g.log.Warn("compat tools: the GitHub release list is unavailable", append([]any{"reason", reason}, detail...)...)
		return &latestOutcome{reason: reason, validUntil: until}
	}
	failWindow := now.Add(5 * time.Minute)
	g.mu.Lock()
	limited := g.limitedUntil
	g.mu.Unlock()
	if now.Before(limited) {
		return fail("rate_limited", limited, "until", limited)
	}
	resp, err := g.get(ctx, apiReleases+"?per_page=10")
	if err != nil {
		return fail("unreachable", failWindow, "err", err)
	}
	defer resp.Body.Close()
	if rateLimited(resp) {
		g.noteRateLimit(resp.Header)
		return fail("rate_limited", rateLimitWindow(resp.Header, now), "status", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fail("upstream_error", failWindow, "status", resp.StatusCode)
	}
	body, err := readDecoded(resp, maxListBytes)
	if errors.Is(err, outbound.ErrBodyTooLarge) || errors.Is(err, errUndecodable) {
		return fail("response_invalid", failWindow, "err", err)
	}
	if err != nil {
		return fail("unreachable", failWindow, "err", err)
	}
	var raw []ghRelease
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return fail("response_invalid", failWindow, "err", err)
	}
	latest, recPublished := filterLatest(raw, recommendedTags())
	return &latestOutcome{fetchedAt: now, validUntil: now.Add(time.Hour), releases: latest, recPublished: recPublished}
}

// filterLatest is amendment 57's four steps: filter, dedupe, drop recommended (keeping their
// published_at), order and cut.
func filterLatest(raw []ghRelease, rec map[string]bool) (latest []latestRelease, recPublished map[string]string) {
	seen := map[string]bool{}
	recPublished = map[string]string{}
	for _, r := range raw {
		if r.Draft || r.Prerelease || !tagRe.MatchString(r.TagName) || seen[r.TagName] {
			continue
		}
		if _, _, hasSum, found := r.pick(); !found || !hasSum {
			continue
		}
		seen[r.TagName] = true
		var pub *string
		if r.PublishedAt != nil {
			if _, err := time.Parse(time.RFC3339, *r.PublishedAt); err == nil {
				pub = r.PublishedAt
			}
		}
		if rec[r.TagName] {
			if pub != nil {
				recPublished[r.TagName] = *pub
			}
			continue
		}
		latest = append(latest, latestRelease{Tag: r.TagName, Published: pub})
	}
	sort.SliceStable(latest, func(i, j int) bool {
		a, b := latest[i], latest[j]
		if (a.Published == nil) != (b.Published == nil) {
			return a.Published != nil
		}
		if a.Published != nil {
			ta, _ := time.Parse(time.RFC3339, *a.Published)
			tb, _ := time.Parse(time.RFC3339, *b.Published)
			if !ta.Equal(tb) {
				return ta.After(tb)
			}
		}
		return a.Tag > b.Tag
	})
	if len(latest) > latestKeep {
		latest = latest[:latestKeep]
	}
	return latest, recPublished
}

// rateLimitWindow: 5 minutes, or until the time GitHub names when that is later, at most 1 hour.
// Retry-After is read in its delta-seconds form only, else X-RateLimit-Reset (Unix seconds).
func rateLimitWindow(h http.Header, now time.Time) time.Time {
	until := now.Add(5 * time.Minute)
	var named time.Time
	if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("Retry-After")), 10, 64); err == nil && s >= 0 {
		named = now.Add(time.Duration(s) * time.Second)
	} else if s, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil && s >= 0 {
		named = time.Unix(s, 0)
	}
	if named.After(until) {
		until = named
	}
	if max := now.Add(time.Hour); until.After(max) {
		until = max
	}
	return until
}

func recommendedTags() map[string]bool {
	out := make(map[string]bool, len(recommended))
	for _, r := range recommended {
		out[r.Tag] = true
	}
	return out
}
