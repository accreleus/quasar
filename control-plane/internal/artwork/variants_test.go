package artwork

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
)

// fakeDecode backs the "QFAKE" format, which only a test writes to disk (Put
// sniffs it as no image): its header reads as 600x900 and decoding calls this.
var fakeDecode func() (image.Image, error)

func init() {
	image.RegisterFormat("qfake", "QFAKE",
		func(io.Reader) (image.Image, error) { return fakeDecode() },
		func(io.Reader) (image.Config, error) { return image.Config{Width: 600, Height: 900}, nil })
}

// putRaw stores bytes Put would refuse, under a .png name.
func putRaw(t *testing.T, b *BlobStore, raw []byte) string {
	t.Helper()
	name := sha(raw) + ".png"
	if err := os.MkdirAll(filepath.Dir(b.pathFor(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.pathFor(name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

// losslessWebP encodes an opaque image as the simplest VP8L stream: no
// transforms, an 8-bit code per colour channel and a one-symbol alpha code.
// x/image has no WebP encoder.
func losslessWebP(img *image.NRGBA) []byte {
	var out []byte
	var acc uint64
	var n uint
	put := func(v uint64, width uint) { // VP8L packs bits from the low end
		acc |= v << n
		for n += width; n >= 8; n -= 8 {
			out = append(out, byte(acc))
			acc >>= 8
		}
	}
	bit := func(c bool) uint64 {
		if c {
			return 1
		}
		return 0
	}
	r := img.Bounds()
	put(0x2f, 8)
	put(uint64(r.Dx()-1), 14)
	put(uint64(r.Dy()-1), 14)
	put(0, 1+3+1+1+1) // alpha hint, version, no transform, no colour cache, no meta codes
	eightBit := func(alphabet int) {
		put(0, 1)    // normal code
		put(12-4, 4) // code-length code lengths, in order 17 18 0 1 2 3 4 5 16 6 7 8
		for i := range 12 {
			put(bit(i == 2 || i == 11), 3) // only lengths 0 and 8 are used, one bit each
		}
		put(0, 1) // lengths for the whole alphabet follow
		for s := range alphabet {
			put(bit(s < 256), 1) // 1 is length 8, 0 is unused
		}
	}
	eightBit(256 + 24) // green, then the unused length prefixes
	eightBit(256)      // red
	eightBit(256)      // blue
	put(1, 1)          // alpha: simple code,
	put(0, 1)          // one symbol,
	put(1, 1)          // 8 bits wide:
	put(255, 8)        // opaque
	put(1, 1)          // distance: simple code,
	put(0, 1)          // one symbol,
	put(0, 1)          // 1 bit wide:
	put(0, 1)          // unused
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			for _, v := range []uint8{c.G, c.R, c.B} {
				put(uint64(bits.Reverse8(v)), 8) // a prefix code is read from its top bit
			}
		}
	}
	if n > 0 {
		out = append(out, byte(acc))
	}
	size := len(out)
	if size%2 == 1 {
		out = append(out, 0) // RIFF pads a chunk to even length
	}
	le := binary.LittleEndian
	file := le.AppendUint32([]byte("RIFF"), uint32(12+len(out)))
	file = append(file, "WEBPVP8L"...)
	file = le.AppendUint32(file, uint32(size))
	return append(file, out...)
}

func serveAssets(t *testing.T, b *BlobStore) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	pass := func(next http.Handler) http.Handler { return next }
	NewHandler(&Service{blobs: b}, slog.New(slog.DiscardHandler)).Register(mux, pass, pass)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

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
func TestOpenVariantDecodesLossyWebP(t *testing.T) {
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

func TestOpenVariantServesAnOpaqueWebPAsJPEG(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/webp", losslessWebP(gradient(400, 600, 255)))
	if err != nil {
		t.Fatal(err)
	}
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || ct != "image/jpeg" || !immutable || http.DetectContentType(data) != "image/jpeg" {
		t.Fatalf("want a JPEG copy, immutable, got %s, %v, %v", ct, immutable, err)
	}
	if w, h := decodeSize(t, data); w != 96 || h != 144 {
		t.Errorf("size: want 96x144, got %dx%d", w, h)
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
	raw := brokenJPEG(t, 600, 900)
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

// A width that needs no copy keeps the immutable original after another width
// failed, and a header that does not parse falls back at every width.
func TestOpenVariantFailureLeavesAFittingWidthImmutable(t *testing.T) {
	b := newBlobs(t)
	name, _ := b.Put("image/jpeg", brokenJPEG(t, 500, 800))
	for i, step := range []struct {
		w         string
		immutable bool
	}{{"560", true}, {"96", false}, {"560", true}} {
		if _, _, immutable, err := b.OpenVariant(t.Context(), name, step.w); err != nil || immutable != step.immutable {
			t.Errorf("step %d, w=%s: want immutable=%v, got %v, %v", i, step.w, step.immutable, immutable, err)
		}
	}

	raw := []byte("not an image header")
	garbled := putRaw(t, b, raw)
	for w := range variantWidths {
		if data, _, immutable, err := b.OpenVariant(t.Context(), garbled, w); err != nil || immutable || !bytes.Equal(data, raw) {
			t.Errorf("unparseable header, w=%s: want the original, not immutable, got %v, %v", w, immutable, err)
		}
	}
}

// A read error is not a property of the blob: that request gets the original,
// and the next one makes the copy.
func TestOpenVariantRetriesAfterAReadError(t *testing.T) {
	b := newBlobs(t)
	raw := encodePNG(t, gradient(600, 900, 255))
	name, _ := b.Put("image/png", raw)
	openBlob = func(string) (*os.File, error) { return nil, syscall.EMFILE }
	t.Cleanup(func() { openBlob = os.Open })
	data, _, immutable, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || immutable || !bytes.Equal(data, raw) {
		t.Fatalf("want the original, not immutable, got %d bytes, %v, %v", len(data), immutable, err)
	}
	openBlob = os.Open
	if _, ct, immutable, err := b.OpenVariant(t.Context(), name, "96"); err != nil || ct != "image/jpeg" || !immutable {
		t.Fatalf("the read error was remembered: got %s, %v, %v", ct, immutable, err)
	}
}

// A decoder panic would otherwise end the process: singleflight re-panics it
// on a goroutine of its own. The blob is then remembered like any other that
// does not decode.
func TestAssetRouteSurvivesADecoderPanic(t *testing.T) {
	b := newBlobs(t)
	var decodes atomic.Int32
	fakeDecode = func() (image.Image, error) { decodes.Add(1); panic("decoder bug") }
	t.Cleanup(func() { fakeDecode = nil })
	raw := []byte("QFAKE that panics")
	name := putRaw(t, b, raw)
	srv := serveAssets(t, b)
	for i := range 2 {
		resp, err := http.Get(srv.URL + "/v1/artwork/" + name + "?w=96")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, raw) || resp.Header.Get("Cache-Control") != "public, max-age=300" {
			t.Fatalf("request %d: want the original with a short max-age, got %d %q", i, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
	}
	if n := decodes.Load(); n != 1 {
		t.Errorf("decoded %d times, want 1", n)
	}
}

// A copy finished after a prune removed its source is not left behind.
func TestOpenVariantDropsACopyWhoseSourceWasPruned(t *testing.T) {
	b := newBlobs(t)
	// Padded so that the copy is smaller and a JPEG is written.
	name := putRaw(t, b, append([]byte("QFAKE"), make([]byte, 64<<10)...))
	fakeDecode = func() (image.Image, error) {
		if _, err := b.PruneOrphans(nil); err != nil {
			t.Error(err)
		}
		return gradient(600, 900, 255), nil
	}
	t.Cleanup(func() { fakeDecode = nil })
	if _, _, _, err := b.OpenVariant(t.Context(), name, "96"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want not-exist, got %v", err)
	}
	for _, ext := range []string{"jpg", "png", "orig"} {
		if _, err := os.Stat(b.variantPath(name, "96", ext)); !os.IsNotExist(err) {
			t.Errorf("the .%s copy of a pruned source was kept", ext)
		}
	}
}

// brokenJPEG is a JPEG whose header reads but whose body does not decode.
func brokenJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, gradient(w, h, 255), nil); err != nil {
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
	broken := brokenJPEG(t, 600, 900)
	brokenName, err := b.Put("image/jpeg", broken)
	if err != nil {
		t.Fatal(err)
	}
	srv := serveAssets(t, b)

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
	for _, q := range []string{"?w=100", "?w=640", "?w=", "?w=320&w=480", "?w=96&w=96", "?w=0320",
		"?w=320;x", "?w=%zz", "?w", "?w=320&%zz", "?w=96&w", "?x;w=96"} {
		if resp, _ := get("/v1/artwork/" + name + q); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: want 404, got %d", q, resp.StatusCode)
		}
	}
	// A query naming no w is ignored however malformed, as before amendment 37.
	for _, q := range []string{"?a;b", "?x=%zz", "?v=2"} {
		resp, _ := get("/v1/artwork/" + name + q)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" ||
			resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Errorf("%s: want the original, immutable; got %d %s %q", q, resp.StatusCode,
				resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
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
