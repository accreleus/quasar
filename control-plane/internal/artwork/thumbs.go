// thumbs.go — search-preview inlining (#80, part 2).
//
// The provider hands Search candidates a ThumbURL on the SteamGridDB CDN, but
// the CSP is `img-src 'self' data: blob:` (httpx/security.go) and stays that
// way on purpose: the hotlinking rule (blobs.go) says a browser must never be
// sent to a third party for an image, and the picker is not an exception the
// CSP was ever going to make. So the control plane fetches each preview during
// the search and returns it as a data: URI — the admin's browser talks only to
// this host, and the strict CSP is untouched.
//
// Previews stay best-effort end to end: any fetch that fails, overruns the
// fetcher's size cap, or isn't an image simply drops to an empty thumb, and
// the picker renders its glyph fallback. Nothing here is stored — the
// applied-artwork path (blobs.go) remains the only cache.

package artwork

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// thumbHostAllowed gates which hosts a preview may be fetched from. The URLs
// come from the provider's API response, not from a client — but a poisoned or
// proxied provider response must still not choose this server's outbound
// targets freely. The Fetcher's dialer already refuses private and loopback
// addresses; this narrows the remainder to the provider's own CDN. A var so
// tests can point it at their loopback stub (the same reason fetch_test.go
// has unguardedFetcher).
var thumbHostAllowed = func(host string) bool {
	host = strings.ToLower(host)
	return host == "steamgriddb.com" || strings.HasSuffix(host, ".steamgriddb.com")
}

// inlineSearchThumbs replaces every candidate's remote ThumbURL with a data:
// URI fetched through the guarded Fetcher, concurrently, dropping any preview
// it cannot inline.
func inlineSearchThumbs(ctx context.Context, f *Fetcher, cands []Candidate) {
	var wg sync.WaitGroup
	for i := range cands {
		if cands[i].ThumbURL == "" {
			continue
		}
		wg.Add(1)
		go func(c *Candidate) {
			defer wg.Done()
			c.ThumbURL = inlineThumb(ctx, f, c.ThumbURL)
		}(&cands[i])
	}
	wg.Wait()
}

// inlineThumb fetches one preview and returns it as a data: URI, or "" when
// anything about the fetch disqualifies it.
func inlineThumb(ctx context.Context, f *Fetcher, rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme != "https" || !thumbHostAllowed(u.Hostname()) {
		return ""
	}
	data, _, err := f.Get(ctx, u.String())
	if err != nil || len(data) == 0 {
		return ""
	}
	// Trust bytes over headers: the URI's MIME type is what the browser will
	// act on, so derive it from the payload the way the browser would.
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return ""
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
