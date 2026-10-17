package artwork

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

// onePixelPNG is a real, minimal PNG. Tests use genuine image bytes because the
// whole ingest path turns on SNIFFING those bytes — a fake payload would test
// nothing that matters.
var onePixelPNG = []byte{
	0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00, 0x05,
	0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00,
	0x00, 0x00, 'I', 'E', 'N', 'D', 0xae, 0x42, 0x60, 0x82,
}

// onePixelJPEG is a real 1x1 JPEG: the pixel check reads the frame header, so
// a bare SOI/APP0/EOI that the sniffer accepts is no longer enough.
var onePixelJPEG = func() []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)), nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
}()

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newBlobs(t *testing.T) *BlobStore {
	t.Helper()
	b, err := NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	b.log = slog.New(slog.DiscardHandler)
	return b
}

// --- content addressing -----------------------------------------------------

// The stored name must be the SHA-256 of the bytes, because that is what makes
// remote-controlled filenames structurally impossible and the served URL
// immutable.
func TestPutIsContentAddressed(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/png", onePixelPNG)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := sha(onePixelPNG) + ".png"; name != want {
		t.Fatalf("name: want %q, got %q", want, name)
	}
	if ok, err := b.Has(name); !ok || err != nil {
		t.Fatalf("Has: blob should exist after Put (err %v)", err)
	}
	data, ct, err := b.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ct != "image/png" {
		t.Fatalf("content type: want image/png, got %q", ct)
	}
	if string(data) != string(onePixelPNG) {
		t.Fatal("Open returned different bytes than Put stored")
	}
}

// Identical bytes must collapse to one file — two apps matching the same art
// share a blob, which is also why Clear must never delete blobs.
func TestPutIsIdempotent(t *testing.T) {
	b := newBlobs(t)
	n1, err := b.Put("image/png", onePixelPNG)
	if err != nil {
		t.Fatalf("Put 1: %v", err)
	}
	n2, err := b.Put("image/png", onePixelPNG)
	if err != nil {
		t.Fatalf("Put 2: %v", err)
	}
	if n1 != n2 {
		t.Fatalf("same bytes produced two names: %q vs %q", n1, n2)
	}
}

// --- type validation --------------------------------------------------------

// A response that CLAIMS image/png but carries HTML must be rejected before it
// reaches disk. Storing it would put attacker-controlled markup behind an
// endpoint we later serve back to a browser.
func TestPutRejectsMislabelledHTML(t *testing.T) {
	b := newBlobs(t)
	html := []byte("<!DOCTYPE html><html><script>alert(1)</script></html>")
	if _, err := b.Put("image/png", html); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType for HTML labelled image/png, got %v", err)
	}
}

// SVG is an executable document format and is not an accepted image type here,
// however it is labelled.
func TestPutRejectsSVG(t *testing.T) {
	b := newBlobs(t)
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if _, err := b.Put("image/svg+xml", svg); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType for SVG, got %v", err)
	}
}

// A declared type that disagrees with real image bytes is still a rejection —
// we do not silently "correct" it, because the disagreement itself is the
// signal that something is wrong with the source.
func TestPutRejectsDeclaredTypeMismatch(t *testing.T) {
	b := newBlobs(t)
	if _, err := b.Put("image/jpeg", onePixelPNG); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType for PNG declared as JPEG, got %v", err)
	}
}

// An absent Content-Type is fine — the sniffed bytes are authoritative.
func TestPutAcceptsEmptyDeclaredType(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("", onePixelJPEG)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !strings.HasSuffix(name, ".jpg") {
		t.Fatalf("want a .jpg name from sniffed JPEG bytes, got %q", name)
	}
}

func TestPutRejectsEmpty(t *testing.T) {
	b := newBlobs(t)
	if _, err := b.Put("image/png", nil); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType for empty body, got %v", err)
	}
}

// --- name validation / traversal --------------------------------------------

// Every one of these must be ErrBadAsset, never a filesystem read. This is the
// path-traversal guard: the asset name comes straight off a URL path.
func TestOpenRejectsTraversalAndJunk(t *testing.T) {
	b := newBlobs(t)
	// Plant a file outside the cache to prove a traversal would have had a
	// target if the guard were absent.
	outside := filepath.Join(filepath.Dir(b.Root()), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, name := range []string{
		"../secret.txt",
		"../../etc/passwd",
		"..%2fsecret.txt",
		"/etc/passwd",
		"secret.txt",
		"",
		".",
		"..",
		strings.Repeat("a", 64) + ".svg",  // valid hash shape, banned extension
		strings.Repeat("a", 63) + ".png",  // too short
		strings.Repeat("a", 65) + ".png",  // too long
		strings.Repeat("A", 64) + ".png",  // uppercase hex
		strings.Repeat("a", 64) + ".png/", // trailing slash
		sha(onePixelPNG) + "/../x.png",
	} {
		if _, _, err := b.Open(name); !errors.Is(err, ErrBadAsset) {
			t.Errorf("Open(%q): want ErrBadAsset, got %v", name, err)
		}
		if ok, err := b.Has(name); ok || err != nil {
			t.Errorf("Has(%q): want false, nil; got %v, %v", name, ok, err)
		}
	}
}

// A well-formed name for a blob that does not exist is a plain not-exist error
// the handler turns into 404 — not a 500.
func TestOpenMissingBlob(t *testing.T) {
	b := newBlobs(t)
	_, _, err := b.Open(sha([]byte("nope")) + ".png")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

// Only a not-exist stat error means missing. A shard path that is a file makes
// stat fail with ENOTDIR (also as root, unlike a chmod), which must come back
// as an error, not as "missing".
func TestHasReturnsAStatErrorThatIsNotNotExist(t *testing.T) {
	b := newBlobs(t)
	name := sha([]byte("x")) + ".png"
	if err := os.WriteFile(filepath.Join(b.Root(), name[:2]), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Has(name); ok || err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Has = %v, %v; want false and a non-not-exist error", ok, err)
	}
}

// --- orphan pruning ---------------------------------------------------------

// A kill between writeAtomic's create and its rename leaves a `.tmp-*` file
// forever otherwise. A prune removes ones older than the window; a fresh one
// is left alone, since its writer may still be running (and the rename will
// turn it into a file the sweep keeps).
func TestPruneOrphansSweepsStaleTempFiles(t *testing.T) {
	b := newBlobs(t)
	name, err := b.Put("image/png", onePixelPNG)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	shard := filepath.Join(b.Root(), name[:2])
	stale := filepath.Join(shard, ".tmp-crashleft")
	if err := os.WriteFile(stale, []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * tmpFileMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(shard, ".tmp-writenow")
	if err := os.WriteFile(fresh, []byte("in flight"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PruneOrphans(map[string]struct{}{name: {}}); err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale temp file survived the prune")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a fresh temp file was pruned: %v", err)
	}
}

// An entry whose age cannot be read at all is left alone, like a fresh one:
// this sweep deletes only what it is certain about. Removing the shard's
// search permission lets the walk list its names but makes the lstat behind
// d.Info() fail for every one of them.
func TestPruneOrphansLeavesTempFilesItCannotDate(t *testing.T) {
	b := newBlobs(t)
	shard := filepath.Join(b.Root(), "ab")
	if err := os.MkdirAll(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(shard, ".tmp-crashleft")
	if err := os.WriteFile(left, []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shard, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(shard, 0o700) }) // so the temp dir can go again
	if _, err := b.PruneOrphans(nil); err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if err := os.Chmod(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); err != nil {
		t.Errorf("a temp file the prune could not date was removed: %v", err)
	}
}

func TestPruneOrphans(t *testing.T) {
	b := newBlobs(t)
	keepName, err := b.Put("image/png", onePixelPNG)
	if err != nil {
		t.Fatalf("Put keep: %v", err)
	}
	dropName, err := b.Put("image/jpeg", onePixelJPEG)
	if err != nil {
		t.Fatalf("Put drop: %v", err)
	}
	// A foreign file must survive: this function deletes only blobs it created.
	foreign := filepath.Join(b.Root(), "README")
	if err := os.WriteFile(foreign, []byte("not ours"), 0o600); err != nil {
		t.Fatalf("seed foreign: %v", err)
	}

	n, err := b.PruneOrphans(map[string]struct{}{keepName: {}})
	if err != nil {
		t.Fatalf("PruneOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed: want 1, got %d", n)
	}
	if ok, _ := b.Has(keepName); !ok {
		t.Error("referenced blob was deleted")
	}
	if ok, _ := b.Has(dropName); ok {
		t.Error("orphan blob survived")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("a non-asset file was deleted")
	}
}

// --- size cap ---------------------------------------------------------------

func TestCopyLimited(t *testing.T) {
	if _, err := copyLimited(strings.NewReader("12345"), 10); err != nil {
		t.Fatalf("under limit: %v", err)
	}
	if _, err := copyLimited(strings.NewReader("12345"), 5); err != nil {
		t.Fatalf("exactly at limit must pass: %v", err)
	}
	if _, err := copyLimited(strings.NewReader("123456"), 5); err == nil {
		t.Fatal("over limit: want an error")
	}
}

func TestNewBlobStoreRejectsEmptyRoot(t *testing.T) {
	if _, err := NewBlobStore("  "); err == nil {
		t.Fatal("want an error for an empty root")
	}
}

// --- #623: decoded-size limit -----------------------------------------------

// A few KB of zeros compress to almost nothing, so this passes the byte cap
// while declaring 25 MP — the shape of a decompression bomb.
func TestPutRejectsImagesOverThePixelLimit(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, 5000, 5000))); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > int(DefaultMaxImageBytes) {
		t.Fatalf("fixture is %d bytes; it must fit the byte cap to prove the pixel cap", buf.Len())
	}
	b, _ := NewBlobStore(t.TempDir())
	if _, err := b.Put("image/png", buf.Bytes()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for a 25 MP PNG, got %v", err)
	}
}

func TestPutRejectsAnUnreadableHeader(t *testing.T) {
	b, _ := NewBlobStore(t.TempDir())
	// Right magic, no header behind it.
	if _, err := b.Put("image/png", onePixelPNG[:12]); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("want ErrUnsupportedType for a truncated PNG, got %v", err)
	}
}

func TestWebPSize(t *testing.T) {
	head := func(chunk string, body []byte) []byte {
		d := append([]byte("RIFF\x00\x00\x00\x00WEBP"+chunk+"\x00\x00\x00\x00"), body...)
		return append(d, make([]byte, 30)...)
	}
	vp8x := head("VP8X", append([]byte{0, 0, 0, 0}, 0x0f, 0x27, 0, 0xe7, 0x03, 0)) // 10000 x 1000
	vp8l := head("VP8L", []byte{0x2f, 0x63, 0x00, 0x00, 0x00})                     // 100 x 1: 14-bit size-1 fields
	vp8 := head("VP8 ", []byte{0, 0, 0, 0x9d, 0x01, 0x2a, 0x58, 0x02, 0x20, 0x03}) // 600 x 800
	for name, c := range map[string]struct {
		in   []byte
		w, h int
	}{
		"vp8x":      {vp8x, 10000, 1000},
		"vp8l":      {vp8l, 100, 1},
		"vp8":       {vp8, 600, 800},
		"short":     {[]byte("RIFF\x00\x00\x00\x00WEBPVP8X"), 0, 0},
		"bad start": {head("VP8 ", []byte{0, 0, 0, 1, 2, 3, 0, 0, 0, 0}), 0, 0},
	} {
		if w, h := webpSize(c.in); w != c.w || h != c.h {
			t.Errorf("%s: webpSize = %dx%d, want %dx%d", name, w, h, c.w, c.h)
		}
	}
	b, _ := NewBlobStore(t.TempDir())
	huge := head("VP8X", append([]byte{0, 0, 0, 0}, 0xff, 0xff, 0, 0xff, 0xff, 0)) // 65536 x 65536
	if _, err := b.Put("image/webp", huge); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest for a 4 GP WebP, got %v", err)
	}
}
