package artwork

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// allowTestHost points the thumb host allowlist at everything for one test —
// httptest servers live on loopback, which the real allowlist (correctly)
// refuses. The SSRF dialer is separately bypassed via unguardedFetcher, and
// the real allowlist keeps its own test below.
func allowTestHost(t *testing.T) {
	t.Helper()
	prev := thumbHostAllowed
	thumbHostAllowed = func(string) bool { return true }
	t.Cleanup(func() { thumbHostAllowed = prev })
}

func TestThumbHostAllowlist(t *testing.T) {
	for host, want := range map[string]bool{
		"steamgriddb.com":      true,
		"cdn2.steamgriddb.com": true,
		"CDN2.STEAMGRIDDB.COM": true,
		"notsteamgriddb.com":   false,
		"steamgriddb.com.evil": false,
		"example.com":          false,
		"":                     false,
	} {
		if got := thumbHostAllowed(host); got != want {
			t.Errorf("thumbHostAllowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestInlineThumbProducesDataURI(t *testing.T) {
	allowTestHost(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately wrong header: the MIME in the URI must come from the
		// bytes, mirroring BlobStore's sniff-over-trust rule.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(onePixelPNG)
	}))
	t.Cleanup(srv.Close)
	f := unguardedFetcher(t, 1<<20)
	f.client = srv.Client()

	got := inlineThumb(context.Background(), f, srv.URL+"/thumb.png")
	if !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("want a sniffed image/png data URI, got %.60q", got)
	}
}

func TestInlineThumbRejectsNonImageAndBadURLs(t *testing.T) {
	allowTestHost(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	t.Cleanup(srv.Close)
	f := unguardedFetcher(t, 1<<20)
	f.client = srv.Client()

	if got := inlineThumb(context.Background(), f, srv.URL); got != "" {
		t.Fatalf("non-image body must drop the thumb, got %.60q", got)
	}
	if got := inlineThumb(context.Background(), f, "http://insecure.example/x.png"); got != "" {
		t.Fatalf("plain http must be refused, got %.60q", got)
	}
	if got := inlineThumb(context.Background(), f, "::notaurl"); got != "" {
		t.Fatalf("unparseable URL must be refused, got %.60q", got)
	}
}

// The real allowlist gates the fetch even when the fetcher itself would allow
// the address — a poisoned provider response must not choose outbound targets.
func TestInlineThumbHonoursHostAllowlist(t *testing.T) {
	f := unguardedFetcher(t, 1<<20)
	if got := inlineThumb(context.Background(), f, "https://evil.example/x.png"); got != "" {
		t.Fatalf("off-allowlist host must be refused, got %.60q", got)
	}
}

func TestInlineSearchThumbsIsBestEffort(t *testing.T) {
	allowTestHost(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "good") {
			_, _ = w.Write(onePixelPNG)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	f := unguardedFetcher(t, 1<<20)
	f.client = srv.Client()

	cands := []Candidate{
		{Ref: "1", ThumbURL: srv.URL + "/good.png"},
		{Ref: "2", ThumbURL: srv.URL + "/broken.png"},
		{Ref: "3"}, // no thumb from the provider: stays empty, no fetch
	}
	inlineSearchThumbs(context.Background(), f, cands)
	if !strings.HasPrefix(cands[0].ThumbURL, "data:image/png;base64,") {
		t.Fatalf("good thumb must inline, got %.60q", cands[0].ThumbURL)
	}
	if cands[1].ThumbURL != "" || cands[2].ThumbURL != "" {
		t.Fatalf("failed/absent thumbs must drop to empty: %+v", cands[1:])
	}
}
