package artwork

import (
	"bytes"
	"context"
	"errors"
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

// variantName is a derived file on disk: the source asset's full name, the
// width, then the encoded type, or "orig" for the empty marker that says no
// copy is smaller than the source. It sits in the source's shard directory and
// is never reachable through Open, which only accepts assetName.
var variantName = regexp.MustCompile(`^([0-9a-f]{64}\.(?:jpg|png|webp))\.[0-9]+\.(?:jpg|png|orig)$`)

// maxConcurrentResizes bounds resize memory on an unauthenticated route. One
// 16 MP decode holds up to ~320 MiB (a progressive CMYK JPEG keeps all its
// coefficients; a 16-bit PNG is 128 MiB), plus the source bytes and the scaled
// copy: about 0.4 GiB a slot. A request waiting for a slot holds none of it.
const maxConcurrentResizes = 2

// OpenVariant answers `?w=`: the blob scaled down to the smallest size that
// still covers a 2:3 frame of a width in variantWidths, made on first request
// and cached beside the original. The original is returned when no smaller
// copy covers the frame or the copy would not be smaller in bytes. An unknown
// width or invalid name is ErrBadAsset, a cancelled ctx is ctx.Err().
//
// immutable is false only when a resize failed and the original stands in for
// the copy, so a browser does not keep it under this URL for a year.
func (b *BlobStore) OpenVariant(ctx context.Context, name, width string) (data []byte, contentType string, immutable bool, err error) {
	w, ok := variantWidths[width]
	if !ok || !assetName.MatchString(name) {
		return nil, "", false, ErrBadAsset
	}
	if data, ct, ok := b.readVariant(name, width); ok {
		return data, ct, true, nil
	}
	// A header error is never remembered: it may be a read error that passes,
	// and an unparseable header costs no decode to find again.
	scale, err := b.coverScale(name, w)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, "", false, err
	case err != nil:
		b.log.Warn("artwork: cannot resize asset, serving the original", "asset", name, "err", err)
		return b.fallback(name)
	case scale >= 1:
		data, ct, err := b.Open(name)
		return data, ct, true, err
	}
	// Only here, so a width that needs no copy stays immutable after a failure at another.
	if _, bad := b.undecodable.Load(name); bad {
		return b.fallback(name)
	}
	for {
		ch := b.resizes.DoChan(name+"@"+width, func() (any, error) { return b.resize(ctx, name, width, scale) })
		select {
		case <-ctx.Done():
			return nil, "", false, ctx.Err()
		case res := <-ch:
			switch {
			case res.Err == nil:
				v := res.Val.(variant)
				return v.data, v.contentType, true, nil
			case errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded):
				if ctx.Err() == nil {
					continue // the request that started this resize left while queued
				}
				return nil, "", false, ctx.Err()
			case errors.Is(res.Err, os.ErrNotExist):
				return nil, "", false, res.Err
			}
			return b.fallback(name)
		}
	}
}

func (b *BlobStore) fallback(name string) ([]byte, string, bool, error) {
	data, ct, err := b.Open(name)
	return data, ct, false, err
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
	if _, err := os.Stat(b.variantPath(name, width, "orig")); err == nil {
		if data, ct, err := b.Open(name); err == nil {
			return data, ct, true
		}
	}
	return nil, "", false
}

// openBlob is os.Open; a test swaps it to inject a read error.
var openBlob = os.Open

// coverScale reads only the header and returns the factor that makes the image
// just cover a w×1.5w frame; 1 or more means it already does.
func (b *BlobStore) coverScale(name string, w int) (float64, error) {
	f, err := openBlob(b.pathFor(name))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, fmt.Errorf("artwork: read header of %s: %w", name, err)
	}
	// Covering the frame rather than matching its width keeps a wider image (a
	// hero standing in for a tile, a 16:10 cover) sharp under object-fit: cover.
	scale := max(float64(w)/float64(cfg.Width), float64(w)*3/2/float64(cfg.Height))
	if scale < 1 && int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return 0, fmt.Errorf("artwork: %s is %dx%d, over the pixel limit", name, cfg.Width, cfg.Height)
	}
	return scale, nil
}

// resize takes a slot, then reads, decodes, scales and encodes: JPEG when the
// source is opaque, PNG when it has transparency. ctx only bounds the wait;
// once a slot is taken the copy is finished and cached for whoever asks next.
func (b *BlobStore) resize(ctx context.Context, name, width string, scale float64) (_ variant, err error) {
	if data, ct, ok := b.readVariant(name, width); ok { // finished since the caller looked
		return variant{data, ct}, nil
	}
	select {
	case b.resizeSlots <- struct{}{}:
	case <-ctx.Done():
		return variant{}, ctx.Err()
	}
	defer func() { <-b.resizeSlots }()
	if err := ctx.Err(); err != nil { // select picks at random when both are ready
		return variant{}, err
	}
	raw, rawType, err := b.Open(name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.log.Warn("artwork: could not read asset to resize, serving the original", "err", err)
		}
		return variant{}, err
	}
	// A decode failure or a panic past this point is a property of the bytes and
	// is remembered. The panic must be recovered here: singleflight re-panics it
	// on the DoChan goroutine, where it kills the process.
	defer func() {
		if p := recover(); p != nil {
			b.undecodable.Store(name, struct{}{})
			b.log.Error("artwork: resize panicked, serving the original", "asset", name, "w", width, "panic", p)
			err = fmt.Errorf("artwork: resize %s: panic: %v", name, p)
		}
	}()
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		b.undecodable.Store(name, struct{}{})
		b.log.Warn("artwork: cannot decode asset, serving the original", "asset", name, "err", err)
		return variant{}, err
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
		b.log.Warn("artwork: could not encode a sized copy, serving the original", "asset", name, "w", width, "err", err)
		return variant{}, err
	}
	// The bytes are a pure function of (source, w), so they are served as the
	// immutable answer whether or not the cache write below succeeds.
	out, path, file := variant{buf.Bytes(), ct}, b.variantPath(name, width, extForType[ct]), buf.Bytes()
	if buf.Len() >= len(raw) {
		out, path, file = variant{raw, rawType}, b.variantPath(name, width, "orig"), nil
	}
	if err := writeAtomic(path, file); err != nil {
		b.log.Warn("artwork: could not cache a sized copy", "asset", name, "w", width, "err", err)
	}
	// A prune that removed the source meanwhile has walked past this file. A
	// prune racing this check can still leave it, until the next prune.
	if !b.Has(name) {
		_ = os.Remove(path)
		return variant{}, os.ErrNotExist
	}
	return out, nil
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
