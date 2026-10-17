package artwork

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
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

// exifScanLen bounds the header scans in walkJPEGHead and iccPNGChunk. EXIF
// and colour-profile data live in the segments right after SOI; an image
// whose 64th KiB still holds no image data has none we act on.
const exifScanLen = 64 << 10

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
// just cover a w×1.5w frame; 1 or more means it already does. The frame is
// covered against the image's ROTATED size: a JPEG carrying an EXIF
// orientation of 5–8 stores its pixels sideways (#713), and a copy made from
// the unrotated numbers covers the frame but comes back the wrong shape.
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
	sw, sh := cfg.Width, cfg.Height
	if o := readOrientation(f); o >= 5 { // sideways: the oriented image swaps them
		sw, sh = sh, sw
	}
	// Covering the frame rather than matching its width keeps a wider image (a
	// hero standing in for a tile, a 16:10 cover) sharp under object-fit: cover.
	scale := max(float64(w)/float64(sw), float64(w)*3/2/float64(sh))
	if scale < 1 && int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return 0, fmt.Errorf("artwork: %s is %dx%d, over the pixel limit", name, cfg.Width, cfg.Height)
	}
	return scale, nil
}

// readOrientation re-reads a blob's opening bytes for its EXIF Orientation
// tag. Any failure to read or parse means 1: the tag is an optimisation, and
// an unreadable one must not fail a copy that would otherwise be made.
func readOrientation(f *os.File) int {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 1
	}
	head := make([]byte, exifScanLen)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return 1
	}
	return jpegOrientation(head[:n])
}

// exifHeaderID starts an EXIF APP1 segment's payload, and iccProfileTag an ICC
// APP2 segment's payload.
const exifHeaderID = "Exif\x00\x00"
const iccProfileTag = "ICC_PROFILE\x00"

// walkJPEGHead calls see for each segment at the head of a JPEG, stopping at
// the first one it declines, at SOS (image data follows), or where the bytes
// stop being a well-formed segment stream. seg is the whole segment (marker,
// length and payload). Every stop is silent: these scans read an optional tag
// or profile, so a malformed or truncated head means only that nothing is
// found, never an error.
func walkJPEGHead(raw []byte, see func(marker byte, seg []byte) bool) {
	head := raw
	if len(head) > exifScanLen {
		head = head[:exifScanLen]
	}
	p := 2 // past SOI
	for p+2 <= len(head) && head[p] == 0xFF {
		m := head[p+1]
		if m == 0x00 || m == 0xFF { // a stuffed byte or padding, not a marker
			return
		}
		// Stand-alone markers carry no length; EOI ends everything and SOS is
		// followed by entropy-coded data no walk should touch.
		if m == 0xDA || m == 0xD9 {
			return
		}
		if m == 0x01 || (m >= 0xD0 && m <= 0xD7) {
			if !see(m, head[p:p+2]) {
				return
			}
			p += 2
			continue
		}
		if p+4 > len(head) {
			return
		}
		segLen := int(head[p+2])<<8 | int(head[p+3])
		if segLen < 2 || p+2+segLen > len(head) {
			return
		}
		if !see(m, head[p:p+2+segLen]) {
			return
		}
		p += 2 + segLen
	}
}

// jpegOrientation reads the EXIF Orientation tag (IFD0 tag 0x0112) from a
// JPEG's APP1 "Exif\0\0" segment. Anything absent, malformed, short or out of
// range means 1, never an error and never a panic (#713).
func jpegOrientation(raw []byte) int {
	orientation := 1
	walkJPEGHead(raw, func(marker byte, seg []byte) bool {
		payload := seg[4:]
		if marker != 0xE1 || len(payload) < len(exifHeaderID)+8 ||
			!bytes.HasPrefix(payload, []byte(exifHeaderID)) {
			return true // the first APP1 need not be the Exif one; keep looking
		}
		tiff := payload[len(exifHeaderID):]
		if tiff[0] == 0 { // the standard one-byte alignment pad
			tiff = tiff[1:]
		}
		orientation = tiffOrientation(tiff)
		return false
	})
	return orientation
}

// tiffOrientation reads value 0x0112 from IFD0 of a TIFF header (the one an
// Exif segment wraps). It answers 1 for anything it cannot read.
func tiffOrientation(tiff []byte) int {
	var read16 func([]byte) uint16
	var read32 func([]byte) uint32
	switch {
	case len(tiff) >= 8 && tiff[0] == 'I' && tiff[1] == 'I':
		read16 = func(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
		read32 = func(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
	case len(tiff) >= 8 && tiff[0] == 'M' && tiff[1] == 'M':
		read16 = func(b []byte) uint16 { return binary.BigEndian.Uint16(b) }
		read32 = func(b []byte) uint32 { return binary.BigEndian.Uint32(b) }
	default:
		return 1
	}
	off := int(read32(tiff[4:8]))
	if off < 8 || off+3 > len(tiff) {
		return 1
	}
	n := int(read16(tiff[off:]))
	if n == 0 || off+2+12*n > len(tiff) {
		return 1
	}
	e := tiff[off+2:]
	for i := 0; i < n; i++ {
		if read16(e[12*i:]) != 0x0112 {
			continue
		}
		if read16(e[12*i+2:]) != 3 { // SHORT: the only type the tag may carry
			return 1
		}
		if v := int(read16(e[12*i+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}

// oriented applies an EXIF orientation transform to a scaled copy, so the
// bytes served are upright like the browser shows the original (#713).
// Orientations 5–8 (sideways) swap the dimensions. Applying it after the
// scale costs nothing in size: the copy covers the same frame either way.
func oriented(src *image.RGBA, o int) *image.RGBA {
	if o < 2 || o > 8 { // 1, and any junk value, mean the pixels are already upright
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	sW, sH := w, h
	if o >= 5 && o <= 8 {
		sW, sH = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, sW, sH))
	for y := 0; y < sH; y++ {
		for x := 0; x < sW; x++ {
			var sx, sy int
			switch o {
			case 2: // mirror horizontal
				sx, sy = w-1-x, y
			case 3: // rotate 180
				sx, sy = w-1-x, h-1-y
			case 4: // mirror vertical
				sx, sy = x, h-1-y
			case 5: // transpose
				sx, sy = y, x
			case 6: // rotate 90 clockwise
				sx, sy = y, h-1-x
			case 7: // transverse (mirror across the anti-diagonal)
				sx, sy = w-1-y, h-1-x
			case 8: // rotate 90 counter-clockwise
				sx, sy = w-1-y, x
			}
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
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
	orientation := jpegOrientation(raw)
	sb := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0,
		max(1, int(math.Round(float64(sb.Dx())*scale))), max(1, int(math.Round(float64(sb.Dy())*scale)))))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)

	// Carrying the source's colour profile keeps the copy's colours the
	// source's (#713): a JPEG copy takes the profile's APP2 segments, a PNG
	// copy its raw iCCP chunk, both untouched — and only an RGB profile
	// travels. The copy holds RGBA pixels whatever the source decoded to, and
	// a profile declaring another data colour space (a CMYK JPEG, a grayscale
	// image) mis-describes those bytes, so such a source is copied with no
	// profile, as before profiles travelled at all. The EXIF segment is NOT
	// carried: the copy is already upright, and an orientation tag of its own
	// would rotate it twice. An opaque PNG source whose profile carries stays
	// PNG, so the profile has a chunk to live in.
	var jpegICC [][]byte
	var pngICC []byte
	if bytes.HasPrefix(raw, []byte{0xFF, 0xD8}) {
		if segs := iccJPEGSegments(raw); iccJPEGColorSpace(segs) == iccColorSpaceRGB {
			jpegICC = segs
		}
	} else if bytes.HasPrefix(raw, pngSignature) {
		if chunk := iccPNGChunk(raw); iccPNGColorSpace(chunk) == iccColorSpaceRGB {
			pngICC = chunk
		}
	}
	var buf bytes.Buffer
	ct := "image/png"
	if pngICC == nil {
		if op, ok := src.(interface{ Opaque() bool }); ok && op.Opaque() {
			ct = "image/jpeg"
			err = jpeg.Encode(&buf, oriented(dst, orientation), &jpeg.Options{Quality: 80})
		}
	}
	if ct == "image/png" {
		err = png.Encode(&buf, oriented(dst, orientation))
	}
	if err != nil {
		b.log.Warn("artwork: could not encode a sized copy, serving the original", "asset", name, "w", width, "err", err)
		return variant{}, err
	}
	encoded := buf.Bytes()
	if ct == "image/jpeg" {
		encoded = spliceJPEGSegments(encoded, jpegICC)
	} else if pngICC != nil {
		encoded = splicePNGChunks(encoded, [][]byte{pngICC})
	}
	// The bytes are a pure function of (source, w), so they are served as the
	// immutable answer whether or not the cache write below succeeds.
	out, path, file := variant{encoded, ct}, b.variantPath(name, width, extForType[ct]), encoded
	if len(encoded) >= len(raw) {
		out, path, file = variant{raw, rawType}, b.variantPath(name, width, "orig"), nil
	}
	if err := writeAtomic(path, file); err != nil {
		b.log.Warn("artwork: could not cache a sized copy", "asset", name, "w", width, "err", err)
	}
	// A prune that removed the source meanwhile has walked past this file. A
	// prune racing this check can still leave it, until the next prune.
	if ok, err := hasBlob(b, name); err != nil {
		b.log.Warn("artwork: could not check the source of a sized copy", "asset", name, "err", err)
	} else if !ok {
		_ = os.Remove(path)
		return variant{}, os.ErrNotExist
	}
	return out, nil
}

// hasBlob is BlobStore.Has; a test swaps it to inject a stat error.
var hasBlob = (*BlobStore).Has

// pngSignature opens every PNG, including everything png.Encode writes.
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}

// iccJPEGSegments collects the raw APP2 "ICC_PROFILE\0" segments of a JPEG's
// leading segment stream, in order — a profile of one segment each, or the
// pieces of a profile split across several.
func iccJPEGSegments(raw []byte) [][]byte {
	var segs [][]byte
	walkJPEGHead(raw, func(marker byte, seg []byte) bool {
		if marker == 0xE2 && bytes.HasPrefix(seg[4:], []byte(iccProfileTag)) {
			segs = append(segs, seg)
		}
		return true
	})
	return segs
}

// spliceJPEGSegments inserts raw JPEG segments into an encoder's output right
// after SOI, or right after the APP0 JFIF segment the encoder wrote when it
// wrote one. Either keeps every APPn ahead of SOF, as the format requires.
func spliceJPEGSegments(out []byte, segs [][]byte) []byte {
	if len(segs) == 0 || len(out) < 4 || out[0] != 0xFF || out[1] != 0xD8 {
		return out
	}
	at := 2
	if out[2] == 0xFF && out[3] == 0xE0 && len(out) >= 6 {
		if l := int(out[4])<<8 | int(out[5]); l >= 2 && 2+l <= len(out) {
			at = 2 + l
		}
	}
	total := 0
	for _, s := range segs {
		total += len(s)
	}
	res := make([]byte, 0, len(out)+total)
	res = append(res, out[:at]...)
	for _, s := range segs {
		res = append(res, s...)
	}
	return append(res, out[at:]...)
}

// iccPNGChunk returns the raw iCCP chunk of a PNG byte stream (length, type,
// data and CRC) when one precedes the image data.
func iccPNGChunk(raw []byte) []byte {
	head := raw
	if len(head) > exifScanLen {
		head = head[:exifScanLen]
	}
	for p := len(pngSignature); p+12 <= len(head); {
		chunkLen := int(binary.BigEndian.Uint32(head[p:]))
		typ := head[p+4 : p+8]
		if chunkLen < 0 || p+12+chunkLen > len(head) {
			return nil
		}
		if bytes.Equal(typ, []byte("iCCP")) {
			return head[p : p+12+chunkLen]
		}
		if bytes.Equal(typ, []byte("IDAT")) {
			return nil // image data begins: every ancillary chunk must be past
		}
		p += 12 + chunkLen
	}
	return nil
}

// iccColorSpaceRGB is the only data colour space an ICC profile may declare
// in its header (bytes 16..20) to travel with a copy. The copy holds RGBA
// pixels whatever the source decoded to; a profile naming another space
// ("CMYK", "GRAY") would mis-describe the very bytes it rides with.
const iccColorSpaceRGB = "RGB "

// iccJPEGColorSpace reads the data colour space of an ICC profile carried in
// JPEG APP2 segments. The profile's header lives at the start of the bytes of
// its first segment by sequence number — the payload past the ICC_PROFILE\0
// tag, one sequence byte and one count byte — and the header's own bytes
// 16..20 name the space. A segment that is too short to hold them, or no
// segment declaring sequence 1 at all, answers "" and the profile is not
// carried.
func iccJPEGColorSpace(segs [][]byte) string {
	for _, seg := range segs {
		payload := seg[4:]
		if len(payload) < 14+20 { // tag, seq, count and the first 20 header bytes
			continue
		}
		if payload[12] != 1 { // sequence numbers are one-based
			continue
		}
		return string(payload[14+16 : 14+20])
	}
	return ""
}

// iccPNGColorSpace reads the data colour space of an iCCP chunk's profile.
// The chunk data is a profile name, a NUL, a compression method byte (0) and
// a zlib stream; only enough of the stream is inflated to reach the header's
// colour-space bytes. Any shape failure — no name NUL, a compression method
// that is not 0, a stream that will not give up even its first 20 bytes —
// answers "" and the profile is not carried.
func iccPNGColorSpace(chunk []byte) string {
	if len(chunk) < 12 { // length, type and CRC around an empty data field
		return ""
	}
	data := chunk[8 : len(chunk)-4]
	name := bytes.IndexByte(data, 0)
	if name < 0 || name+2 > len(data) || data[name+1] != 0 {
		return ""
	}
	zr, err := zlib.NewReader(bytes.NewReader(data[name+2:]))
	if err != nil {
		return ""
	}
	defer zr.Close()
	head := make([]byte, 20)
	if _, err := io.ReadFull(zr, head); err != nil {
		return ""
	}
	return string(head[16:20])
}

// splicePNGChunks inserts raw PNG chunks right after the IHDR chunk, where
// iCCP belongs (after IHDR, before the first IDAT).
func splicePNGChunks(out []byte, chunks [][]byte) []byte {
	ihdr := len(pngSignature)
	if len(chunks) == 0 || len(out) < ihdr+12+4 || !bytes.HasPrefix(out, pngSignature) ||
		string(out[ihdr+4:ihdr+8]) != "IHDR" {
		return out
	}
	l := int(binary.BigEndian.Uint32(out[ihdr:]))
	if l < 0 || ihdr+12+l > len(out) {
		return out
	}
	at := ihdr + 12 + l
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	res := make([]byte, 0, len(out)+total)
	res = append(res, out[:at]...)
	for _, c := range chunks {
		res = append(res, c...)
	}
	return append(res, out[at:]...)
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
