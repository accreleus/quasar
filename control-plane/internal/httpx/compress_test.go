package httpx_test

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/httpx"
)

// body returns the response body, transparently gunzipping when the response
// declares gzip. It fails the test on a truncated / invalid gzip stream, which
// is the failure mode that matters most: a half-written stream renders as a
// blank page with no console error.
func body(t *testing.T, res *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if res.Header.Get("Content-Encoding") != "gzip" {
		return string(raw)
	}
	zr, err := gzip.NewReader(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gzip close (truncated stream?): %v", err)
	}
	return string(out)
}

func serve(h http.Handler, r *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	httpx.Compress(h).ServeHTTP(rec, r)
	return rec.Result()
}

func gzipReq(method, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Accept-Encoding", "gzip, deflate, br")
	return r
}

// jsHandler writes a large compressible JavaScript body.
func jsHandler(size int) http.Handler {
	payload := strings.Repeat("export const a = 1;\n", size/20+1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte(payload))
	})
}

func TestCompressesLargeJavaScript(t *testing.T) {
	res := serve(jsHandler(300_000), gzipReq(http.MethodGet, "/assets/index.js"))

	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := res.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, must contain Accept-Encoding", got)
	}
	// A stale Content-Length describing the UNcompressed size truncates the
	// response in the browser. It must be gone.
	if got := res.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want empty when compressed", got)
	}
	if n := len(body(t, res)); n < 300_000 {
		t.Errorf("decompressed body = %d bytes, want >= 300000", n)
	}
}

func TestCompressionActuallyShrinks(t *testing.T) {
	rec := httptest.NewRecorder()
	httpx.Compress(jsHandler(300_000)).ServeHTTP(rec, gzipReq(http.MethodGet, "/assets/index.js"))
	if wire := rec.Body.Len(); wire > 60_000 {
		t.Errorf("gzipped wire size = %d bytes, expected well under 60000", wire)
	}
}

func TestCSSAndJSONAndHTMLCompress(t *testing.T) {
	for _, ct := range []string{
		"text/css",
		"application/json",
		"text/html; charset=utf-8",
		"image/svg+xml",
		"application/javascript",
	} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(strings.Repeat("x", 50_000)))
		})
		res := serve(h, gzipReq(http.MethodGet, "/x"))
		if got := res.Header.Get("Content-Encoding"); got != "gzip" {
			t.Errorf("Content-Type %q: Content-Encoding = %q, want gzip", ct, got)
		}
	}
}

// Recompressing an already-compressed payload burns CPU and grows the body.
// Artwork blobs and woff2 fonts are the ones that matter here.
func TestAlreadyCompressedTypesPassThrough(t *testing.T) {
	for _, ct := range []string{
		"image/png",
		"image/jpeg",
		"image/webp",
		"font/woff2",
		"video/mp4",
		"application/zstd",
	} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(strings.Repeat("x", 50_000)))
		})
		res := serve(h, gzipReq(http.MethodGet, "/x"))
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("Content-Type %q: Content-Encoding = %q, want none", ct, got)
		}
		if n := len(body(t, res)); n != 50_000 {
			t.Errorf("Content-Type %q: body = %d bytes, want 50000", ct, n)
		}
	}
}

func TestNoAcceptEncodingMeansNoCompression(t *testing.T) {
	res := serve(jsHandler(300_000), httptest.NewRequest(http.MethodGet, "/assets/index.js", nil))
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	// Vary must still be set: an intermediate cache that stored this
	// uncompressed response must not serve it to a gzip-capable client as if
	// encoding-independent.
	if got := res.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, must contain Accept-Encoding even uncompressed", got)
	}
}

// Small bodies fit in a single packet; compressing them adds CPU and header
// bytes for nothing.
func TestSmallBodiesAreNotCompressed(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><title>Quasar</title>"))
	})
	res := serve(h, gzipReq(http.MethodGet, "/"))
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none for a tiny body", got)
	}
	if got := body(t, res); got != "<!doctype html><title>Quasar</title>" {
		t.Errorf("body = %q, mangled", got)
	}
}

// A handler that never sets Content-Type relies on net/http sniffing the first
// 512 bytes. If the middleware lets the sniff see GZIPPED bytes the response is
// mislabelled application/octet-stream and the browser downloads it instead of
// executing it — the same class of failure as the /assets/* MIME trap in
// SPAHandler.
func TestSniffedContentTypeUsesPlainBytes(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><html><body>" + strings.Repeat("y", 50_000) + "</body></html>"))
	})
	res := serve(h, gzipReq(http.MethodGet, "/"))
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q, want text/html (sniffed from plain bytes)", got)
	}
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

func TestStatusCodeAndHeadersPreserved(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"a":"` + strings.Repeat("b", 50_000) + `"}`))
	})
	res := serve(h, gzipReq(http.MethodPost, "/v1/x"))
	if res.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want 201", res.StatusCode)
	}
	if got := res.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control = %q, lost", got)
	}
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

func TestNoContentAndNotModifiedUntouched(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})
		res := serve(h, gzipReq(http.MethodGet, "/v1/x"))
		if res.StatusCode != code {
			t.Errorf("status = %d, want %d", res.StatusCode, code)
		}
		if got := res.Header.Get("Content-Encoding"); got != "" {
			t.Errorf("status %d: Content-Encoding = %q, want none", code, got)
		}
	}
}

// A handler that already encoded its own body must not be double-encoded.
func TestPreEncodedBodyPassesThrough(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write([]byte(strings.Repeat("z", 50_000)))
	})
	rec := httptest.NewRecorder()
	httpx.Compress(h).ServeHTTP(rec, gzipReq(http.MethodGet, "/v1/x"))
	if rec.Body.Len() != 50_000 {
		t.Errorf("wire body = %d bytes, want 50000 (untouched)", rec.Body.Len())
	}
}

// hijackRecorder is an httptest.ResponseRecorder that also satisfies
// http.Hijacker, standing in for a real connection.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

// The agent WS and the signaling relay depend on gorilla/websocket hijacking the
// connection. A middleware that wraps the ResponseWriter without forwarding
// Hijack breaks every WebSocket in the product — the single worst way this
// change could fail.
func TestWebSocketUpgradeCanStillHijack(t *testing.T) {
	var hijackErr error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			hijackErr = errNotHijacker
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			hijackErr = err
			return
		}
		_ = conn.Close()
	})

	r := gzipReq(http.MethodGet, "/agent/ws")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Connection", "Upgrade")

	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	httpx.Compress(h).ServeHTTP(rec, r)

	if hijackErr != nil {
		t.Fatalf("handler could not hijack: %v", hijackErr)
	}
	if !rec.hijacked {
		t.Fatal("Hijack never reached the underlying ResponseWriter")
	}
}

var errNotHijacker = errNotHijackerType{}

type errNotHijackerType struct{}

func (errNotHijackerType) Error() string {
	return "ResponseWriter does not implement http.Hijacker"
}

// Streaming handlers flush mid-response. The flush must reach the client with
// the bytes written so far, not sit in the gzip window forever.
func TestFlushPropagates(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: " + strings.Repeat("q", 50_000) + "\n\n"))
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter lost http.Flusher")
			return
		}
		f.Flush()
	})
	rec := httptest.NewRecorder()
	httpx.Compress(h).ServeHTTP(rec, gzipReq(http.MethodGet, "/v1/stream"))
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying ResponseWriter")
	}
}

// The web client's bandwidth probe re-fetches the main bundle and divides bytes
// by duration to choose a stream tier, discarding samples under 50 KB.
// Compressing it shrinks the sample ~3x and biases the estimate low — the #146
// regression. It must be served identity-encoded.
func TestBandwidthProbeIsNotCompressed(t *testing.T) {
	res := serve(jsHandler(300_000), gzipReq(http.MethodGet, "/assets/index.js?probe=1785408072638"))
	if got := res.Header.Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want none for the bandwidth probe", got)
	}
	if n := len(body(t, res)); n < 300_000 {
		t.Errorf("probe body = %d bytes, want the full uncompressed bundle (>= 300000)", n)
	}
}

// Same asset WITHOUT the probe marker still compresses — the exemption must be
// scoped to the probe, not to /assets/*.
func TestSameAssetWithoutProbeStillCompresses(t *testing.T) {
	res := serve(jsHandler(300_000), gzipReq(http.MethodGet, "/assets/index.js"))
	if got := res.Header.Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
}

func TestHeadRequestNotCorrupted(t *testing.T) {
	res := serve(jsHandler(300_000), gzipReq(http.MethodHead, "/assets/index.js"))
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}
