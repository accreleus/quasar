package artwork

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
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
	"testing/synctest"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// gradient has a noisy blue channel so that, like real art, it does not
// compress to almost nothing as a PNG and its scaled copy is the smaller file.
func gradient(w, h int, alpha uint8) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			noise := (uint32(x)*73856093 ^ uint32(y)*19349663) * 2654435761
			img.SetNRGBA(x, y, color.NRGBA{uint8(x), uint8(y), uint8(noise >> 24), alpha})
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
	data, ct, _, err := b.OpenVariant(t.Context(), name, "320")
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
	again, _, _, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || !bytes.Equal(again, marker) {
		t.Fatalf("second request did not read the cache: %q, %v", again, err)
	}
}

// A frame-covering copy: a wide image keeps enough width to fill a 2:3 tile.
func TestOpenVariantCoversTheTileFrame(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(1920, 620, 255)))
	data, _, _, err := b.OpenVariant(t.Context(), name, "96")
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
		if _, _, _, err := b.OpenVariant(t.Context(), name, w); err != ErrBadAsset {
			t.Errorf("w=%q: want ErrBadAsset, got %v", w, err)
		}
	}
	if _, _, _, err := b.OpenVariant(t.Context(), "../../etc/passwd", "96"); err != ErrBadAsset {
		t.Errorf("bad name: want ErrBadAsset, got %v", err)
	}
	if _, _, _, err := b.OpenVariant(t.Context(), sha([]byte("x"))+".png", "96"); !os.IsNotExist(err) {
		t.Errorf("missing blob: want not-exist, got %v", err)
	}
}

// An image that already fits the frame is served as stored, and nothing is
// written for it.
func TestOpenVariantServesSmallSourcesUnchanged(t *testing.T) {
	b := newBlobs(t)
	raw := encodePNG(t, gradient(300, 450, 255))
	name, _ := b.Put("image/png", raw)
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || ct != "image/png" || !bytes.Equal(data, raw) || !immutable {
		t.Fatalf("want the original PNG, immutable, got %s, %d bytes, %v, %v", ct, len(data), immutable, err)
	}
	if _, _, ok := b.readVariant(name, "320"); ok {
		t.Error("a variant was written for a source that needed none")
	}
}

func TestOpenVariantKeepsTransparencyAsPNG(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 100)))
	data, ct, _, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || ct != "image/png" {
		t.Fatalf("want image/png, got %s, %v", ct, err)
	}
	if http.DetectContentType(data) != "image/png" {
		t.Error("bytes are not a PNG")
	}
}

// The fixture is so small that any copy is bigger, so the decode shows as the
// marker rather than as a JPEG.
func TestOpenVariantDecodesWebP(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/webp", webp120x180)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || ct != "image/webp" || !immutable || !bytes.Equal(data, webp120x180) {
		t.Fatalf("want the original WebP, immutable, got %s, %v, %v", ct, immutable, err)
	}
	if _, err := os.Stat(b.variantPath(name, "96", "orig")); err != nil {
		t.Errorf("the WebP was not decoded and scaled: %v", err)
	}
}

// The requests queue behind full slots, so every one is waiting before the
// resize can start. With one shared resize they all get the same buffer; with
// one resize each, every request would hold its own.
func TestOpenVariantConcurrentRequestsShareOneResize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBlobs(t)
		name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
		holdSlots(b)
		var wg sync.WaitGroup
		results := make([][]byte, 8)
		for i := range results {
			wg.Go(func() { results[i], _, _, _ = b.OpenVariant(t.Context(), name, "480") })
		}
		synctest.Wait()
		releaseSlots(b)
		wg.Wait()
		for i, r := range results {
			if len(r) == 0 || &r[0] != &results[0][0] {
				t.Fatalf("request %d did not share the first request's resize", i)
			}
		}
	})
}

func holdSlots(b *BlobStore) {
	for range maxConcurrentResizes {
		b.resizeSlots <- struct{}{}
	}
}

func releaseSlots(b *BlobStore) {
	for range maxConcurrentResizes {
		<-b.resizeSlots
	}
}

// A request whose client leaves while it waits for a slot returns at once and
// decodes nothing; a second request for the same copy, still waiting, takes
// over and gets it.
func TestOpenVariantCancelledWhileQueuedDoesNotDecode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBlobs(t)
		name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
		holdSlots(b)

		ctx, cancel := context.WithCancel(t.Context())
		var leaderErr, followerErr error
		var follower []byte
		var wg sync.WaitGroup
		wg.Go(func() { _, _, _, leaderErr = b.OpenVariant(ctx, name, "320") })
		synctest.Wait()
		wg.Go(func() { follower, _, _, followerErr = b.OpenVariant(t.Context(), name, "320") })
		synctest.Wait()

		cancel()
		synctest.Wait()
		if !errors.Is(leaderErr, context.Canceled) {
			t.Fatalf("cancelled request: want context.Canceled, got %v", leaderErr)
		}
		if _, _, ok := b.readVariant(name, "320"); ok {
			t.Fatal("a copy was made for a request that left while queued")
		}

		releaseSlots(b)
		wg.Wait()
		if followerErr != nil || http.DetectContentType(follower) != "image/jpeg" {
			t.Fatalf("waiting request: want the JPEG copy, got %d bytes, %v", len(follower), followerErr)
		}
	})
}

// A source that does not decode is remembered: the next request is answered
// with the original without queueing for a slot, and pruning the blob forgets it.
func TestOpenVariantRemembersUndecodableSources(t *testing.T) {
	b := newBlobs(t)
	raw := brokenJPEG(t)
	name, err := b.Put("image/jpeg", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, _, immutable, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || immutable || !bytes.Equal(data, raw) {
		t.Fatalf("want the original, not immutable, got %d bytes, %v, %v", len(data), immutable, err)
	}

	// Full slots and a dead context: any attempt to resize would fail.
	holdSlots(b)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if data, _, _, err := b.OpenVariant(ctx, name, "320"); err != nil || !bytes.Equal(data, raw) {
		t.Fatalf("second request went back to the resize queue: %v", err)
	}
	releaseSlots(b)

	if _, err := b.PruneOrphans(nil); err != nil {
		t.Fatal(err)
	}
	if _, bad := b.undecodable.Load(name); bad {
		t.Error("a pruned blob is still remembered")
	}
}

// When the copy would not be smaller in bytes, the original is served and an
// empty marker records that, so later requests do not decode again.
func TestOpenVariantKeepsTheOriginalWhenTheCopyIsBigger(t *testing.T) {
	b := newBlobs(t)
	// Two-colour stripes: a tiny paletted PNG, a large JPEG once scaled.
	img := image.NewPaletted(image.Rect(0, 0, 600, 900), color.Palette{color.Black, color.White})
	for y := 0; y < 900; y++ {
		for x := 0; x < 600; x++ {
			img.SetColorIndex(x, y, uint8(x/2%2))
		}
	}
	raw := encodePNG(t, img)
	name, _ := b.Put("image/png", raw)
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || ct != "image/png" || !immutable || !bytes.Equal(data, raw) {
		t.Fatalf("want the original PNG, immutable, got %s, %v, %v", ct, immutable, err)
	}
	if fi, err := os.Stat(b.variantPath(name, "320", "orig")); err != nil || fi.Size() != 0 {
		t.Fatalf("want an empty marker, got %v, %v", fi, err)
	}

	holdSlots(b)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if data, _, immutable, err := b.OpenVariant(ctx, name, "320"); err != nil || !immutable || !bytes.Equal(data, raw) {
		t.Fatalf("the marker was not used: %v, %v", immutable, err)
	}
	releaseSlots(b)
}

// A copy that cannot be cached is still the answer.
func TestOpenVariantServesTheCopyWhenTheCacheWriteFails(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	// A directory where the copy goes makes the rename fail, even as root.
	if err := os.Mkdir(b.variantPath(name, "320", "jpg"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || ct != "image/jpeg" || !immutable {
		t.Fatalf("want the JPEG copy, immutable, got %s, %v, %v", ct, immutable, err)
	}
	if w, h := decodeSize(t, data); w != 320 || h != 480 {
		t.Errorf("size: want 320x480, got %dx%d", w, h)
	}
}

// brokenJPEG is a JPEG whose header reads but whose body does not decode.
func brokenJPEG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(600, 900, 255), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()[:buf.Len()/2]
}

func TestPruneOrphansRemovesVariants(t *testing.T) {
	b := newBlobs(t)
	keep, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	drop, _ := b.Put("image/png", encodePNG(t, gradient(600, 900, 200)))
	for _, name := range []string{keep, drop} {
		if _, _, _, err := b.OpenVariant(t.Context(), name, "96"); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{keep, drop} { // "no smaller copy" markers
		if err := os.WriteFile(b.variantPath(name, "480", "orig"), nil, 0o644); err != nil {
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
	if _, err := os.Stat(b.variantPath(keep, "480", "orig")); err != nil {
		t.Error("a kept blob's marker was removed")
	}
	for _, p := range []string{b.variantPath(drop, "96", "jpg"), b.variantPath(drop, "480", "orig")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("a removed blob's %s survived", filepath.Base(p))
		}
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
	broken := brokenJPEG(t)
	brokenName, err := b.Put("image/jpeg", broken)
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
	for _, h := range []string{"X-Content-Type-Options", "Content-Security-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("?w=320: missing %s", h)
		}
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("?w=320: Cache-Control %q", cc)
	}
	if resp, _ := get("/v1/artwork/" + name); resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("no query: want 200 image/png, got %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// "w=320;x" and a bad escape fail to parse; r.URL.Query() would read them as no w.
	for _, q := range []string{"?w=100", "?w=640", "?w=", "?w=320&w=480", "?w=0320", "?w=320;x", "?w=320&%zz", "?w=96&w"} {
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

	// A resize that fails serves the original rather than an error, and does
	// not let a browser keep it as the copy.
	resp, body = get("/v1/artwork/" + brokenName + "?w=96")
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, broken) {
		t.Errorf("undecodable source: want the original bytes, got %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("undecodable source: Cache-Control %q, want a short max-age without immutable", cc)
	}
	if resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("undecodable source: headers %v", resp.Header)
	}
}
