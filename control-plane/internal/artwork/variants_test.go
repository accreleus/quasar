package artwork

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gradient(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), 128, alpha})
		}
	}
	return img
}

// webp120x180 is a lossy 120x180 WebP (x/image has no encoder to make one).
var webp120x180, _ = base64.StdEncoding.DecodeString(
	"UklGRnIBAABXRUJQVlA4IGYBAAAwDgCdASp4ALQAPu12sFYppqQjoGiBMB2JZW7f+J8G3aArf7VU8NtQD/P9ABtSD3B5B7kiyNj////0Qpf/1X//4j///0X//30+W+Z/v9Qf//////we/Zf//7Qe/////Df+zfw/////M6wT6b6IlwH6C4OoP2Hj8+dGAAD+8CTuPOf7weCnO+0e9IdiIytTMWcHRZM4qkd2NKscC/HrKIIjo9rqdVVl0MQ8XIl+jG5z37k0+u5jhu/S+jnWpw9ezUPWSQeyhFGfvdC7tutRzWSumlSmft5esNR/kUDG9Gmgzy6N3CzRCGKx4lhjQ0Mo07GqVnGMISdZMSqF/9FKsuH8Ci8r8ca8JKhCohPEfMZaW0XK76sB4LJFKfIHLiroAzpdjPY21KAqqjT96B9d/8y9At38h+/TL0A4JGeOfSguitx/mncRFKlWyTK8UmeUS9L/hMVzuUtTFVI4etAnieHtoDPxQAAA")

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode variant: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestOpenVariantScalesAndCaches(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	if err != nil {
		t.Fatal(err)
	}
	data, ct, err := b.OpenVariant(name, "320")
	if err != nil {
		t.Fatalf("OpenVariant: %v", err)
	}
	// Opaque PNG comes back as JPEG: the type follows the bytes, not the asset.
	if ct != "image/jpeg" {
		t.Errorf("content type: want image/jpeg, got %s", ct)
	}
	if w, h := decodeSize(t, data); w != 320 || h != 480 {
		t.Errorf("size: want 320x480, got %dx%d", w, h)
	}

	// A second request is served from the cached file, not decoded again.
	marker := []byte("cached variant")
	if err := os.WriteFile(b.variantPath(name, "320", "jpg"), marker, 0o644); err != nil {
		t.Fatal(err)
	}
	again, _, err := b.OpenVariant(name, "320")
	if err != nil || !bytes.Equal(again, marker) {
		t.Fatalf("second request did not read the cache: %q, %v", again, err)
	}
}

// A frame-covering copy: a wide image keeps enough width to fill a 2:3 tile.
func TestOpenVariantCoversTheTileFrame(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(1920, 620, 255)))
	data, _, err := b.OpenVariant(name, "96")
	if err != nil {
		t.Fatal(err)
	}
	if w, h := decodeSize(t, data); h != 144 || w != 446 {
		t.Errorf("size: want 446x144 (covers 96x144), got %dx%d", w, h)
	}
}

func TestOpenVariantRejectsUnknownWidths(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	for _, w := range []string{"", "0", "100", "0320", "320 ", "+320", "-96", "1e3"} {
		if _, _, err := b.OpenVariant(name, w); err != ErrBadAsset {
			t.Errorf("w=%q: want ErrBadAsset, got %v", w, err)
		}
	}
	if _, _, err := b.OpenVariant("../../etc/passwd", "96"); err != ErrBadAsset {
		t.Errorf("bad name: want ErrBadAsset, got %v", err)
	}
	if _, _, err := b.OpenVariant(sha([]byte("x"))+".png", "96"); !os.IsNotExist(err) {
		t.Errorf("missing blob: want not-exist, got %v", err)
	}
}

// An image that already fits the frame is served as stored, and nothing is
// written for it.
func TestOpenVariantServesSmallSourcesUnchanged(t *testing.T) {
	b := newBlobs(t)
	raw := encodePNG(t, gradient(300, 450, 255))
	name, _ := b.Put("image/png", raw)
	data, ct, err := b.OpenVariant(name, "320")
	if err != nil || ct != "image/png" || !bytes.Equal(data, raw) {
		t.Fatalf("want the original PNG, got %s, %d bytes, %v", ct, len(data), err)
	}
	if _, _, ok := b.readVariant(name, "320"); ok {
		t.Error("a variant was written for a source that needed none")
	}
}

func TestOpenVariantKeepsTransparencyAsPNG(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 100)))
	data, ct, err := b.OpenVariant(name, "96")
	if err != nil || ct != "image/png" {
		t.Fatalf("want image/png, got %s, %v", ct, err)
	}
	if http.DetectContentType(data) != "image/png" {
		t.Error("bytes are not a PNG")
	}
}

func TestOpenVariantReencodesWebP(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/webp", webp120x180)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, err := b.OpenVariant(name, "96")
	if err != nil || ct != "image/jpeg" || http.DetectContentType(data) != "image/jpeg" {
		t.Fatalf("want a JPEG copy of the WebP, got %s, %v", ct, err)
	}
	if w, h := decodeSize(t, data); w != 96 || h != 144 {
		t.Errorf("size: want 96x144, got %dx%d", w, h)
	}
}

func TestOpenVariantConcurrentRequestsShareOneResize(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	var wg sync.WaitGroup
	results := make([][]byte, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _, _ = b.OpenVariant(name, "480")
		}()
	}
	wg.Wait()
	for i, r := range results {
		if len(r) == 0 || !bytes.Equal(r, results[0]) {
			t.Fatalf("request %d got different bytes", i)
		}
	}
}

func TestPruneOrphansRemovesVariants(t *testing.T) {
	b := newBlobs(t)
	keep, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	drop, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 200)))
	for _, name := range []string{keep, drop} {
		if _, _, err := b.OpenVariant(name, "96"); err != nil {
			t.Fatal(err)
		}
	}
	// A variant whose source is already gone is an orphan too.
	stray := filepath.Join(b.Root(), "ab", "ab"+sha([]byte("gone"))[2:]+".png.320.jpg")
	_ = os.MkdirAll(filepath.Dir(stray), 0o755)
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := b.PruneOrphans(map[string]struct{}{keep: {}})
	if err != nil || n != 1 {
		t.Fatalf("PruneOrphans: want 1 blob removed, got %d, %v", n, err)
	}
	if _, _, ok := b.readVariant(keep, "96"); !ok {
		t.Error("a kept blob's variant was removed")
	}
	if _, _, ok := b.readVariant(drop, "96"); ok {
		t.Error("a removed blob's variant survived")
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("an orphan variant survived")
	}
}

func TestAssetRouteServesSizedVariants(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	smallRaw := encodePNG(t, gradient(300, 450, 255))
	small, _ := b.Put("image/png", smallRaw)
	// A JPEG whose header reads but whose body does not decode.
	var broken bytes.Buffer
	_ = jpeg.Encode(&broken, gradient(600, 900, 255), nil)
	brokenName, err := b.Put("image/jpeg", broken.Bytes()[:broken.Len()/2])
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(&Service{blobs: b}, slog.New(slog.DiscardHandler)).Register(mux, pass, pass)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(path string) (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, body := get("/v1/artwork/" + name + "?w=320")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("?w=320: want 200 image/jpeg, got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if w, _ := decodeSize(t, body); w != 320 {
		t.Errorf("?w=320: width %d", w)
	}
	for _, h := range []string{"X-Content-Type-Options", "Cache-Control", "Content-Security-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("?w=320: missing %s", h)
		}
	}

	for _, q := range []string{"?w=100", "?w=640", "?w=", "?w=320&w=480", "?w=0320"} {
		if resp, _ := get("/v1/artwork/" + name + q); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", q, resp.StatusCode)
		}
	}
	if resp, _ := get("/v1/artwork/" + sha([]byte("x")) + ".png?w=96"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing asset with w: want 404, got %d", resp.StatusCode)
	}

	resp, body = get("/v1/artwork/" + small + "?w=480")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !bytes.Equal(body, smallRaw) {
		t.Errorf("?w=480 on a 300px source: want the original PNG, got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// A resize that fails serves the original rather than an error.
	resp, body = get("/v1/artwork/" + brokenName + "?w=96")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, broken.Bytes()[:broken.Len()/2]) {
		t.Errorf("undecodable source: want the original bytes, got %d", resp.StatusCode)
	}
}
