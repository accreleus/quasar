package artwork

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// onePixelJPEG is a real minimal JPEG (SOI + APP0/JFIF + EOI is enough for the
// sniffer, which keys on the FFD8FF prefix).
var onePixelJPEG = append([]byte{
	0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00,
	0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
}, 0xff, 0xd9)

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
	if !b.Has(name) {
		t.Fatal("Has: blob should exist after Put")
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
		if b.Has(name) {
			t.Errorf("Has(%q): want false", name)
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

// --- orphan pruning ---------------------------------------------------------

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
	if !b.Has(keepName) {
		t.Error("referenced blob was deleted")
	}
	if b.Has(dropName) {
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
