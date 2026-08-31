package artwork

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The only image types we will ever store or serve. Anything else is rejected
// before a single byte reaches disk — an artwork cache is not a general file
// drop, and serving back whatever a remote host sent is how an "image" endpoint
// turns into stored-XSS (an SVG with a <script>, an HTML file labelled
// image/png). SVG is deliberately ABSENT: it is an executable document format.
var extForType = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

// assetName is the ONLY shape a cached blob's name can take: the lowercase hex
// SHA-256 of its own bytes plus an extension from extForType. Content-addressed
// naming is what makes remote-controlled path traversal structurally impossible
// — nothing a remote sends (URL path, Content-Disposition, redirect target) has
// any influence on the name, because the name is computed from the bytes we
// actually stored. Every read validates against this before touching the
// filesystem.
var assetName = regexp.MustCompile(`^[0-9a-f]{64}\.(jpg|png|webp)$`)

// ErrBadAsset is returned for a name that is not a valid content-addressed blob
// name. Callers turn this into a 404, never a 500: an invalid name is an
// attempted traversal or a stale link, not a server fault.
var ErrBadAsset = errors.New("artwork: invalid asset name")

// ErrUnsupportedType is returned when the bytes are not one of the three image
// types we store.
var ErrUnsupportedType = errors.New("artwork: unsupported image type")

// BlobStore is the local, content-addressed cache of artwork bytes.
//
// Local because art must not be hotlinked: a self-hosted box must not depend
// on a third party at browse time, and every hotlinked <img> reports the
// deployment's library to that party per page view. Not under web/dist: that
// bind mount is rebuilt on deploy (`rm -rf dist` swaps the inode, #131), which
// would silently delete every cached asset — the artwork root is its own
// persistent volume.
type BlobStore struct {
	root string
}

// NewBlobStore creates (or adopts) the cache directory at root.
func NewBlobStore(root string) (*BlobStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artwork: blob store root is empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("artwork: create cache dir: %w", err)
	}
	return &BlobStore{root: filepath.Clean(root)}, nil
}

// Root reports the cache directory (used by tests and diagnostics).
func (b *BlobStore) Root() string { return b.root }

// pathFor maps a validated asset name to its on-disk path. Blobs are sharded by
// the first two hex characters so a large catalogue does not produce one
// directory with tens of thousands of entries.
//
// The name MUST already have passed assetName — the regexp anchors on 64 hex
// digits plus a fixed extension, which admits no '/', no '.', and no '..', so
// the join cannot escape root.
func (b *BlobStore) pathFor(name string) string {
	return filepath.Join(b.root, name[:2], name)
}

// Put stores bytes and returns their content-addressed name.
//
// declaredType is what the source CLAIMED (an HTTP Content-Type, or an upload's
// header). It is checked, but it is not trusted on its own: the type is also
// SNIFFED from the leading bytes and the two must agree. A remote that labels
// an HTML page `image/png` fails here rather than becoming a file we later
// serve back to a browser. The stored extension comes from the sniffed type,
// so the name always describes the real content.
func (b *BlobStore) Put(declaredType string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrUnsupportedType
	}
	sniffed := normalizeMediaType(http.DetectContentType(data))
	ext, ok := extForType[sniffed]
	if !ok {
		return "", fmt.Errorf("%w: sniffed %q", ErrUnsupportedType, sniffed)
	}
	if declared := normalizeMediaType(declaredType); declared != "" && declared != sniffed {
		return "", fmt.Errorf("%w: declared %q but bytes are %q", ErrUnsupportedType, declared, sniffed)
	}

	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "." + ext
	dst := b.pathFor(name)

	// Content-addressed: identical bytes are the same file, so an existing blob
	// is already correct and re-writing it would only risk truncating a file
	// another request is serving.
	if _, err := os.Stat(dst); err == nil {
		return name, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("artwork: create shard dir: %w", err)
	}
	// Write to a temp file in the same directory then rename: a reader can only
	// ever observe a complete blob, never a half-written one.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("artwork: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("artwork: write blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("artwork: close blob: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("artwork: chmod blob: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("artwork: publish blob: %w", err)
	}
	return name, nil
}

// Open returns a blob's bytes and its content type. An invalid name is
// ErrBadAsset (→ 404), never a filesystem error.
func (b *BlobStore) Open(name string) (data []byte, contentType string, err error) {
	if !assetName.MatchString(name) {
		return nil, "", ErrBadAsset
	}
	raw, err := os.ReadFile(b.pathFor(name))
	if err != nil {
		return nil, "", err
	}
	ext := name[strings.LastIndexByte(name, '.')+1:]
	for mt, e := range extForType {
		if e == ext {
			return raw, mt, nil
		}
	}
	return nil, "", ErrBadAsset
}

// Has reports whether a blob exists (and has a valid name).
func (b *BlobStore) Has(name string) bool {
	if !assetName.MatchString(name) {
		return false
	}
	_, err := os.Stat(b.pathFor(name))
	return err == nil
}

// PruneOrphans deletes every cached blob that `referenced` does not name.
//
// Blobs are content-addressed and therefore SHARED: two apps that matched the
// same art point at one file, so clearing one app's artwork must not delete it.
// Reclaiming instead happens here, sweeping from the full on-disk set — which
// is also what cleans up after a re-match, where the old blob simply stops
// being referenced by anything.
//
// It returns the number removed. A file it cannot parse as an asset name is
// left alone: this function deletes only things it is certain it created.
func (b *BlobStore) PruneOrphans(referenced map[string]struct{}) (int, error) {
	removed := 0
	err := filepath.WalkDir(b.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !assetName.MatchString(name) {
			return nil // not ours — leave it
		}
		if _, keep := referenced[name]; keep {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("artwork: prune: %w", err)
	}
	return removed, nil
}

// normalizeMediaType strips parameters and casing from a media type
// ("image/jpeg; charset=utf-8" → "image/jpeg").
func normalizeMediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// copyLimited reads at most limit bytes from r, and reports an error if the
// source had more. Used by every ingest path so neither a hostile Content-Length
// nor a chunked response without one can fill the disk.
func copyLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("artwork: image exceeds the %d byte limit", limit)
	}
	return data, nil
}
