package artwork

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WebP decoder for image.Decode
)

// variantWidths is the closed set a client may ask for with `?w=`
// (control-api.md §GET /v1/artwork/{asset}, amendment 37). Keyed by the exact
// query string so a non-canonical spelling ("0320") is not a member.
var variantWidths = map[string]int{"96": 96, "320": 320, "480": 480, "560": 560}

// variantName is a derived copy on disk: the source asset's full name, the
// width, then the encoded type. It sits in the source's shard directory and is
// never reachable through Open, which only accepts assetName.
var variantName = regexp.MustCompile(`^([0-9a-f]{64}\.(?:jpg|png|webp))\.[0-9]+\.(?:jpg|png)$`)

// maxConcurrentResizes bounds decode memory: the route is unauthenticated and a
// 16 MP source decodes to about 64 MiB.
const maxConcurrentResizes = 2

// OpenVariant returns the blob scaled down to the smallest size that still
// covers a 2:3 frame of a width in variantWidths, made on first request and
// cached beside the original. An unknown width or invalid name is ErrBadAsset.
// A source that already fits the frame is returned as is. Any other error is a
// failed resize, and the caller may fall back to Open.
func (b *BlobStore) OpenVariant(name, width string) ([]byte, string, error) {
	w, ok := variantWidths[width]
	if !ok || !assetName.MatchString(name) {
		return nil, "", ErrBadAsset
	}
	if data, ct, ok := b.readVariant(name, width); ok {
		return data, ct, nil
	}
	raw, ct, err := b.Open(name)
	if err != nil {
		return nil, "", err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("artwork: read header of %s: %w", name, err)
	}
	// Covering the frame rather than matching its width keeps a wider image (a
	// hero standing in for a tile, a 16:10 cover) sharp under object-fit: cover.
	scale := max(float64(w)/float64(cfg.Width), float64(w)*3/2/float64(cfg.Height))
	if scale >= 1 {
		return raw, ct, nil
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return nil, "", fmt.Errorf("artwork: %s is %dx%d, over the pixel limit", name, cfg.Width, cfg.Height)
	}
	v, err, _ := b.resizes.Do(name+"@"+width, func() (any, error) {
		if data, ct, ok := b.readVariant(name, width); ok {
			return variant{data, ct}, nil
		}
		b.resizeSlots <- struct{}{}
		defer func() { <-b.resizeSlots }()
		return b.makeVariant(name, width, raw, scale)
	})
	if err != nil {
		return nil, "", err
	}
	out := v.(variant)
	return out.data, out.contentType, nil
}

type variant struct {
	data        []byte
	contentType string
}

func (b *BlobStore) variantPath(name, width, ext string) string {
	return filepath.Join(b.root, name[:2], name+"."+width+"."+ext)
}

func (b *BlobStore) readVariant(name, width string) ([]byte, string, bool) {
	for _, mt := range []string{"image/jpeg", "image/png"} {
		if data, err := os.ReadFile(b.variantPath(name, width, extForType[mt])); err == nil {
			return data, mt, true
		}
	}
	return nil, "", false
}

// makeVariant decodes, scales and encodes: JPEG when the source is opaque, PNG
// when it has transparency.
func (b *BlobStore) makeVariant(name, width string, raw []byte, scale float64) (variant, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return variant{}, fmt.Errorf("artwork: decode %s: %w", name, err)
	}
	sb := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0,
		max(1, int(math.Round(float64(sb.Dx())*scale))), max(1, int(math.Round(float64(sb.Dy())*scale)))))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)

	var buf bytes.Buffer
	ct := "image/png"
	if o, ok := src.(interface{ Opaque() bool }); ok && o.Opaque() {
		ct = "image/jpeg"
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 80})
	} else {
		err = png.Encode(&buf, dst)
	}
	if err != nil {
		return variant{}, fmt.Errorf("artwork: encode %s at %s: %w", name, width, err)
	}
	if err := writeAtomic(b.variantPath(name, width, extForType[ct]), buf.Bytes()); err != nil {
		return variant{}, err
	}
	return variant{buf.Bytes(), ct}, nil
}

// writeAtomic writes through a temp file and a rename so a reader only ever
// sees a complete file.
func writeAtomic(dst string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("artwork: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("artwork: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("artwork: close: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("artwork: chmod: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("artwork: publish: %w", err)
	}
	return nil
}
