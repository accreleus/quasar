package artwork

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
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

// --- #713: EXIF orientation -------------------------------------------------

// appSegment is one JPEG APPn segment: marker, declared length, payload.
func appSegment(marker byte, payload []byte) []byte {
	seg := []byte{0xFF, marker, byte((len(payload) + 2) >> 8), byte((len(payload) + 2) & 0xFF)}
	return append(seg, payload...)
}

// jpegWithSegments encodes img as a JPEG and splices one segment per payload,
// all carrying marker, right after SOI — the position a real camera file's
// Exif and profile segments hold.
func jpegWithSegments(t *testing.T, marker byte, img image.Image, payloads [][]byte) []byte {
	t.Helper()
	var base bytes.Buffer
	if err := jpeg.Encode(&base, img, nil); err != nil {
		t.Fatal(err)
	}
	out := []byte{0xFF, 0xD8}
	for _, p := range payloads {
		if len(p)+2 > 0xFFFF {
			t.Fatal("the test segment does not fit a length word")
		}
		out = append(out, appSegment(marker, p)...)
	}
	return append(out, base.Bytes()[2:]...)
}

// exifPayload is a well-formed Exif APP1 payload for the given orientation:
// the segment id, the alignment pad, and a little-endian TIFF header holding
// one IFD0 entry.
func exifPayload(o uint16) []byte {
	tiff := []byte{
		'I', 'I', 42, 0, // byte order and magic
		8, 0, 0, 0, // IFD0 starts right after the header
		1, 0, // one entry
		0x12, 0x01, 3, 0, 1, 0, 0, 0, // tag 0x0112, SHORT, one value
		byte(o), byte(o >> 8), 0, 0,
		0, 0, 0, 0, // no next IFD
	}
	return append([]byte("Exif\x00\x00\x00"), tiff...)
}

// TestJpegOrientation pins the parser itself: the eight values, the shapes
// that must fall back to 1, and finding the Exif segment behind another APP1.
func TestJpegOrientation(t *testing.T) {
	// A big-endian TIFF holding orientation o.
	mm := func(o uint16) []byte {
		tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1,
			0x01, 0x12, 0, 3, 0, 0, 0, 1, byte(o >> 8), byte(o), 0, 0, 0, 0, 0, 0}
		return append([]byte("Exif\x00\x00\x00"), tiff...)
	}
	wrap := func(payload ...[]byte) []byte {
		out := []byte{0xFF, 0xD8}
		for _, p := range payload {
			out = append(out, appSegment(0xE1, p)...)
		}
		return append(out, onePixelJPEG[2:]...)
	}
	for name, c := range map[string]struct {
		raw  []byte
		want int
	}{
		"empty":                 {nil, 1},
		"not a jpeg":            {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), 1},
		"orientation 1":         {wrap(exifPayload(1)), 1},
		"orientation 6":         {wrap(exifPayload(6)), 6},
		"orientation 8, MM":     {wrap(mm(8)), 8},
		"after a jfif segment":  {wrap(append(append([]byte{}, []byte("JFIF\x00x")...), 0), exifPayload(2)), 2},
		"behind a foreign APP1": {wrap([]byte("http://ns.adobe.com/xap"), exifPayload(4)), 4},
		"bad byte order":        {wrap([]byte("Exif\x00\x00\x00XX\x2a\x00\x08\x00\x00\x00\x01\x00")), 1},
		"header alone":          {wrap([]byte("Exif\x00\x00\x00" + "II\x2a\x00\x08\x00\x00\x00")), 1},
		"ifd offset off the end": {wrap([]byte("Exif\x00\x00\x00" +
			"II\x2a\x00\xff\xff\x00\x00\x01\x00")), 1},
		"count over the data": {wrap([]byte("Exif\x00\x00\x00" +
			"II\x2a\x00\x08\x00\x00\x00\x05\x00\x00\x00\x00\x00")), 1},
		"other tags only": {wrap([]byte("Exif\x00\x00\x00" +
			"II\x2a\x00\x08\x00\x00\x00\x01\x00\x99\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")), 1},
		"orientation 0": {wrap(exifPayload(0)), 1},
		"orientation 9": {wrap(exifPayload(9)), 1},
	} {
		if got := jpegOrientation(c.raw); got != c.want {
			t.Errorf("%s: jpegOrientation = %d, want %d", name, got, c.want)
		}
	}
}

// Orientation 1, and any value the tag cannot carry, mean the pixels are
// already upright: oriented must answer the very image it was given, not a
// copy of it.
func TestOrientedLeavesAlreadyUprightPixelsAlone(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 3, 2))
	for i := range src.Pix {
		src.Pix[i] = uint8(i*37 + 11)
	}
	for _, o := range []int{-1, 0, 1, 9, 12} {
		if got := oriented(src, o); got != src {
			t.Errorf("orientation %d: the pixels were copied", o)
		}
	}
	// And a moving orientation still transforms: 4 mirrors vertically.
	if got := oriented(src, 4); got == src || got.At(0, 0) != src.At(0, 1) {
		t.Error("orientation 4: the mirror did not run")
	}
}

// bands is an opaque image whose left third is light and the rest dark, so an
// uprightness check survives the JPEG encoder's lossy 4:2:0 subsampling: it
// only moves chroma.
func bands(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		v := uint8(30)
		if x < w/3 {
			v = 220
		}
		for y := 0; y < h; y++ {
			img.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	return img
}

// A source whose EXIF says it is stored sideways comes out upright, rotated
// first, and the copy is sized against the rotated shape.
func TestOpenVariantAppliesEXIFOrientation(t *testing.T) {
	b := newBlobs(t)
	raw := jpegWithSegments(t, 0xE1, bands(600, 900), [][]byte{exifPayload(6)})
	name, err := b.Put("image/jpeg", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, immutable, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || ct != "image/jpeg" || !immutable {
		t.Fatalf("want the JPEG copy, immutable, got %s, %v, %v", ct, immutable, err)
	}
	// coverScale reads 900x600 rotated, so the copy is 720x480 and covers
	// 320x480 — not the unrotated 320x480 that would still fit but show tall.
	if w, h := decodeSize(t, data); w != 720 || h != 480 {
		t.Fatalf("size: want 720x480 upright and swapped, got %dx%d", w, h)
	}
	// Row y of the upright copy comes from column y of the stored pixels, so
	// the light third lands at the top instead of the left.
	src, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode copy: %v", err)
	}
	if r, _, _, _ := src.At(300, 150).RGBA(); r>>8 < 205 { // 220 minus encoder slop
		t.Errorf("the copy is not upright: pixel (300,150) is %d", r>>8)
	}
	if r, _, _, _ := src.At(300, 450).RGBA(); r>>8 > 45 { // 30 plus encoder slop
		t.Errorf("the copy is rotated twice: pixel (300,450) is %d", r>>8)
	}
	// The EXIF segment must NOT travel with the copy: its orientation would
	// rotate the already-upright pixels a second time.
	if bytes.Contains(data, []byte("Exif")) {
		t.Error("the copy carries the source's EXIF segment")
	}
}

// A malformed EXIF segment means orientation 1: the copy is sized and made as
// if the image were stored upright, never an error and never a panic.
func TestOpenVariantTreatsMalformedEXIFAsNormal(t *testing.T) {
	b := newBlobs(t)
	raw := jpegWithSegments(t, 0xE1, gradient(600, 900, 255), [][]byte{exifPayload(9)}) // 9 is out of range
	name, err := b.Put("image/jpeg", raw)
	if err != nil {
		t.Fatal(err)
	}
	if data, ct, _, err := b.OpenVariant(t.Context(), name, "320"); err != nil || ct != "image/jpeg" {
		t.Fatalf("want the JPEG copy, got %s, %v", ct, err)
	} else if w, h := decodeSize(t, data); w != 320 || h != 480 {
		t.Errorf("size: the copy followed a broken orientation tag: %dx%d", w, h)
	}
}

// --- #713: colour profiles --------------------------------------------------

// TestOpenVariantCarriesTheJPEGColourProfile: a profile the source split
// across two APP2 segments travels with the copy, raw, and the copy decodes.
func TestOpenVariantCarriesTheJPEGColourProfile(t *testing.T) {
	b := newBlobs(t)
	profile := make([]byte, 3072) // two segments' worth
	for i := range profile {
		profile[i] = byte(i * 7)
	}
	copy(profile[16:20], "RGB ") // the header's data colour space
	id := []byte(iccProfileTag)
	raw := jpegWithSegments(t, 0xE2, gradient(600, 900, 255), [][]byte{
		append(append(append([]byte{}, id...), 1, 2), profile[:2048]...),
		append(append(append([]byte{}, id...), 2, 2), profile[2048:]...),
	})
	name, err := b.Put("image/jpeg", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, _, err := b.OpenVariant(t.Context(), name, "320")
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("want the JPEG copy, got %s, %v", ct, err)
	}
	var chunks []byte
	walkJPEGHead(data, func(marker byte, seg []byte) bool {
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], id) {
			chunks = append(chunks, seg[4+14:]...) // past ICC_PROFILE\0 and seq/count
		}
		return true
	})
	if !bytes.Equal(chunks, profile) {
		t.Errorf("the copy does not carry the profile: %d of %d bytes", len(chunks), len(profile))
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the copy with a profile does not decode: %v", err)
	}
}

// pngWithICC returns img encoded as a PNG with an iCCP chunk carrying
// profile (compressed, as the chunk format demands) inserted after IHDR.
func pngWithICC(t *testing.T, img image.Image, profile []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if _, err := zw.Write(profile); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := append([]byte("ICC\x00\x00"), z.Bytes()...)
	base := encodePNG(t, img)
	chunk := make([]byte, 12+len(data))
	binary.BigEndian.PutUint32(chunk[:4], uint32(len(data)))
	copy(chunk[4:8], "iCCP")
	copy(chunk[8:], data)
	binary.BigEndian.PutUint32(chunk[8+len(data):], crc32.ChecksumIEEE(chunk[4:8+len(data)]))
	out := append([]byte{}, base[:33]...) // signature plus the 13-byte IHDR chunk
	return append(append(out, chunk...), base[33:]...)
}

// An opaque PNG source that carries a profile keeps PNG output — the profile
// needs a chunk to live in — and the chunk travels with the copy raw.
func TestOpenVariantCarriesThePNGColourProfile(t *testing.T) {
	b := newBlobs(t)
	profile := make([]byte, 1024)
	for i := range profile {
		profile[i] = byte(i * 13)
	}
	copy(profile[16:20], "RGB ") // the header's data colour space
	raw := pngWithICC(t, gradient(600, 900, 255), profile)
	name, err := b.Put("image/png", raw)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, _, err := b.OpenVariant(t.Context(), name, "96")
	if err != nil || ct != "image/png" {
		t.Fatalf("want the PNG copy, got %s, %v", ct, err)
	}
	if chunk := iccPNGChunk(data); !bytes.Equal(chunk, iccPNGChunk(raw)) {
		t.Errorf("the copy does not carry the source's profile: %d vs %d bytes",
			len(iccPNGChunk(data)), len(iccPNGChunk(raw)))
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the copy with a profile does not decode: %v", err)
	}
}

// A profile whose data colour space is not RGB does not travel. A copy holds
// RGBA pixels whatever the source decoded to, so a CMYK or GRAY header would
// mis-describe the data it rides with; the source is copied with no profile
// at all, exactly as before profiles travelled — an opaque PNG goes to JPEG
// again, one with transparency stays PNG.
func TestOpenVariantDropsANonRGBColourProfile(t *testing.T) {
	b := newBlobs(t)

	// A JPEG whose profile declares CMYK: the copy is still JPEG, without the
	// APP2 segments.
	jpegProfile := make([]byte, 128)
	copy(jpegProfile[16:20], "CMYK")
	jraw := jpegWithSegments(t, 0xE2, gradient(600, 900, 255), [][]byte{
		append(append(append([]byte{}, iccProfileTag...), 1, 1), jpegProfile...)})
	jname, err := b.Put("image/jpeg", jraw)
	if err != nil {
		t.Fatal(err)
	}
	data, ct, _, err := b.OpenVariant(t.Context(), jname, "320")
	if err != nil || ct != "image/jpeg" {
		t.Fatalf("want the JPEG copy, got %s, %v", ct, err)
	}
	id := []byte(iccProfileTag)
	walkJPEGHead(data, func(marker byte, seg []byte) bool {
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], id) {
			t.Error("the copy carries the source's CMYK profile")
			return false
		}
		return true
	})
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("the copy without a profile does not decode: %v", err)
	}

	// A grayscale-tagged profile on a PNG: the copy carries no chunk, and the
	// opaque one loses the stay-PNG rule along with the profile.
	pngProfile := make([]byte, 128)
	copy(pngProfile[16:20], "GRAY")
	for name, tc := range map[string]struct {
		raw  []byte
		want string
	}{
		"opaque png":            {pngWithICC(t, gradient(600, 900, 255), pngProfile), "image/jpeg"},
		"png with transparency": {pngWithICC(t, gradient(600, 900, 128), pngProfile), "image/png"},
	} {
		pname, err := b.Put("image/png", tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		data, ct, _, err := b.OpenVariant(t.Context(), pname, "320")
		if err != nil || ct != tc.want {
			t.Errorf("%s: want a %s copy, got %s, %v", name, tc.want, ct, err)
			continue
		}
		if iccPNGChunk(data) != nil {
			t.Errorf("%s: the copy carries the source's GRAY profile", name)
		}
	}
}

// --- #734: the resize's source check ----------------------------------------

// A stat error other than not-exist keeps the cached copy: only a definite
// "source gone" may remove one.
func TestResizeKeepsTheCopyWhenTheSourceCannotBeChecked(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/png", encodePNG(t, gradient(600, 900, 255)))
	if err != nil {
		t.Fatal(err)
	}
	hasBlob = func(*BlobStore, string) (bool, error) { return false, errors.New("stat broke") }
	t.Cleanup(func() { hasBlob = (*BlobStore).Has })
	if _, _, immutable, err := b.OpenVariant(t.Context(), name, "96"); err != nil || !immutable {
		t.Fatalf("want the copy served anyway, immutable, got %v, %v", immutable, err)
	}
	if _, err := os.Stat(b.variantPath(name, "96", "jpg")); err != nil {
		t.Fatalf("the copy was not kept after a stat error: %v", err)
	}

	// A definite "source gone" removes the file and answers not-exist. Drop
	// the cached copy first so the resize (and with it the check) happens.
	hasBlob = func(*BlobStore, string) (bool, error) { return false, nil }
	if err := os.Remove(b.variantPath(name, "96", "jpg")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := b.OpenVariant(t.Context(), name, "96"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want not-exist when the source is gone, got %v", err)
	}
	if _, err := os.Stat(b.variantPath(name, "96", "jpg")); !os.IsNotExist(err) {
		t.Errorf("a removed source's copy survived: %v", err)
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
