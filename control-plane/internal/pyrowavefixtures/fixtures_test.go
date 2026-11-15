// Package pyrowavefixtures holds the integrity test for testdata/pyrowave (#1053): the golden
// PyroWave bitstreams and reference decodes the WebGPU decoder and the agent's pyrowaveenc are
// tested against. The files are read by other suites too, so this is the one place that proves
// they are the files the manifest says, framed as the bitstream spec says, for the pinned library.
package pyrowavefixtures

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

var dir = filepath.Join("..", "..", "..", "testdata", "pyrowave")

type file struct {
	File   string `json:"file"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Pin        struct{ Pyrowave, Granite string }
	DecoderBar struct {
		MinPSNR   float64 `json:"min_psnr_db"`
		MaxAbsErr int     `json:"max_abs_err"`
	} `json:"decoder_bar"`
	Pictures map[string]struct {
		Width, Height int
		Src           file
	}
	Fixtures []struct {
		Name          string
		Width, Height int
		FrameBytes    int `json:"frame_bytes"`
		Blocks        int
		SOF           struct {
			TotalBlocks      int `json:"total_blocks"`
			ChromaResolution int `json:"chroma_resolution"`
			ColorPrimaries   int `json:"color_primaries"`
			TransferFunction int `json:"transfer_function"`
			YCbCrTransform   int `json:"ycbcr_transform"`
			YCbCrRange       int `json:"ycbcr_range"`
			ChromaSiting     int `json:"chroma_siting"`
		}
		EncoderBar struct {
			MinPSNR map[string]float64 `json:"min_psnr_db"`
		} `json:"encoder_bar"`
		PWV1 file
		Ref  file
		Src  string
		PSNR map[string]float64 `json:"psnr_ref_vs_src_db"`
	}
}

func load(t *testing.T) manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func checkFile(t *testing.T, f file) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, f.File))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if len(b) != f.Bytes || hex.EncodeToString(sum[:]) != f.SHA256 {
		t.Errorf("%s: %d bytes sha256 %x, manifest says %d bytes %s (regenerate with scripts/dev/pyrowave-fixtures/gen.sh)",
			f.File, len(b), sum, f.Bytes, f.SHA256)
	}
	return b
}

func TestManifestHashesAndNoStrayFiles(t *testing.T) {
	m := load(t)
	want := map[string]bool{"manifest.json": true, "README.md": true}
	for _, p := range m.Pictures {
		checkFile(t, p.Src)
		want[p.Src.File] = true
	}
	for _, f := range m.Fixtures {
		checkFile(t, f.PWV1)
		checkFile(t, f.Ref)
		want[f.PWV1.File], want[f.Ref.File] = true, true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
		if !want[e.Name()] {
			t.Errorf("%s is in testdata/pyrowave but not in the manifest", e.Name())
		}
	}
	if len(got) != len(want) {
		sort.Strings(got)
		t.Errorf("directory holds %v, manifest names %d files", got, len(want))
	}
}

// The manifest's pin must be the one deploy/Dockerfile.vulkan builds: a pin bump without
// regenerated fixtures fails here (docs/third-party-pins.md).
func TestPinMatchesDockerfile(t *testing.T) {
	m := load(t)
	df, err := os.ReadFile(filepath.Join("..", "..", "..", "deploy", "Dockerfile.vulkan"))
	if err != nil {
		t.Fatal(err)
	}
	for arg, have := range map[string]string{"PYROWAVE_REF": m.Pin.Pyrowave, "GRANITE_REF": m.Pin.Granite} {
		got := ""
		if mm := regexp.MustCompile(`(?m)^ARG ` + arg + `=([0-9a-f]{40})$`).FindSubmatch(df); mm != nil {
			got = string(mm[1])
		}
		if got != have {
			t.Errorf("%s in Dockerfile.vulkan is %q, fixtures were generated at %s: regenerate", arg, got, have)
		}
	}
}

// blockCount is the number of block indices of a 4:2:0 frame: bitstream.md "Block index ordering".
func blockCount(w, h int) int {
	align := func(v int) int { return max((v+31)&^31, 128) }
	aw, ah := align(w), align(h)
	n := 0
	for level := 4; level >= 0; level-- {
		for comp := 0; comp < 3; comp++ {
			if level == 0 && comp != 0 {
				continue
			}
			bands := 3
			if level == 4 {
				bands = 4
			}
			n += bands * (((aw >> (level + 1)) + 31) / 32) * (((ah >> (level + 1)) + 31) / 32)
		}
	}
	return n
}

// canonical checks one block against bitstream.md "Decoding 8x8 blocks": its used size rounds up
// to payload_words, and the alignment bytes and the last sign byte's unused bits are zero (the
// generator's canonicalize()).
func canonical(blk []byte) error {
	w0 := binary.LittleEndian.Uint32(blk)
	n := bits.OnesCount32(w0 & 0xFFFF)
	off, nz := 8+3*n, 0
	for i := 0; i < n; i++ {
		cw := int(blk[8+2*i]) | int(blk[9+2*i])<<8
		base := int(blk[8+2*n+i] & 0xF)
		for sub := 0; sub < 8; sub++ {
			planes := (cw>>(2*sub))&3 + base
			if off+planes > len(blk) {
				return fmt.Errorf("payload overruns the block")
			}
			var any byte
			for _, b := range blk[off : off+planes] {
				any |= b
			}
			off += planes
			nz += bits.OnesCount8(any)
		}
	}
	used := off + (nz+7)/8
	if (used+3)/4*4 != len(blk) {
		return fmt.Errorf("uses %d bytes, payload_words says %d", used, len(blk))
	}
	for i, b := range blk[used:] {
		if b != 0 {
			return fmt.Errorf("padding byte %d is %#x", used+i, b)
		}
	}
	if nz%8 != 0 && blk[used-1]>>(nz%8) != 0 {
		return fmt.Errorf("unused sign bits are %#x", blk[used-1])
	}
	return nil
}

func psnr(a, b []byte) (db float64, maxErr int) {
	var sum float64
	for i := range a {
		d := int(a[i]) - int(b[i])
		sum += float64(d * d)
		maxErr = max(maxErr, max(d, -d))
	}
	if sum == 0 {
		return 99, 0
	}
	return math.Round(10*math.Log10(255*255/(sum/float64(len(a))))*100) / 100, maxErr
}

// Walk each bitstream by the self-delimiting packet rule (bitstream.md, codec-pyrowave.md
// "packet"): one SOF first, then blocks that end exactly at the frame's end.
func TestBitstreamFraming(t *testing.T) {
	m := load(t)
	if len(m.Fixtures) < 4 {
		t.Fatalf("only %d fixtures", len(m.Fixtures))
	}
	if m.DecoderBar.MinPSNR != 45 || m.DecoderBar.MaxAbsErr != 1 {
		t.Errorf("decoder_bar is %+v, want 45 dB and 1 code value", m.DecoderBar)
	}
	for _, f := range m.Fixtures {
		t.Run(f.Name, func(t *testing.T) {
			b := checkFile(t, f.PWV1)
			if len(b) < 36 || string(b[:4]) != "PWV1" || b[21] != 1 {
				t.Fatal("not a PWV1 file with header_version 1")
			}
			if n := int(binary.LittleEndian.Uint32(b[24:])); n != f.FrameBytes || 28+n != len(b) {
				t.Fatalf("frame size %d, manifest %d, file %d", n, f.FrameBytes, len(b))
			}
			// The file header's picture fields equal the frame's SOF; reserved ones are zero.
			h0, h1 := binary.LittleEndian.Uint32(b[4:]), binary.LittleEndian.Uint32(b[8:])
			frame := b[28:]
			s0, s1 := binary.LittleEndian.Uint32(frame), binary.LittleEndian.Uint32(frame[4:])
			if s0>>31 != 1 || h0&^0x0FFFFFFF != 0 || h0 != s0&0x0FFFFFFF || h1 != s1&0xFC000000 {
				t.Fatalf("SOF %08x %08x vs file header %08x %08x", s0, s1, h0, h1)
			}
			if w, h := int(s0&0x3FFF)+1, int(s0>>14&0x3FFF)+1; w != f.Width || h != f.Height {
				t.Fatalf("SOF says %dx%d, manifest %dx%d", w, h, f.Width, f.Height)
			}
			// 4:2:0, sRGB primaries and transfer, BT.709, LIMITED range, centre siting: the
			// configuration codec-pyrowave.md hello_ack describes, and what the README says.
			if got := [...]uint32{s1 >> 26 & 1, s1 >> 27 & 1, s1 >> 28 & 1, s1 >> 29 & 1, s1 >> 30 & 1, s1 >> 31 & 1}; got != [...]uint32{0, 0, 0, 0, 1, 0} {
				t.Errorf("SOF chroma/primaries/transfer/transform/range/siting = %v, want [0 0 0 0 1 0]", got)
			}
			sof := f.SOF
			if sof.ChromaResolution != 0 || sof.ColorPrimaries != 0 || sof.TransferFunction != 0 || sof.YCbCrTransform != 0 || sof.YCbCrRange != 1 || sof.ChromaSiting != 0 {
				t.Errorf("manifest sof %+v does not describe the stream", sof)
			}

			total, seen, blocks := blockCount(f.Width, f.Height), map[uint32]bool{}, 0
			for pos := 8; pos < len(frame); blocks++ {
				if len(frame)-pos < 8 {
					t.Fatalf("truncated packet header at %d", pos)
				}
				w0 := binary.LittleEndian.Uint32(frame[pos:])
				if w0>>31 != 0 {
					t.Fatalf("second SOF at %d", pos)
				}
				words := int(w0 >> 16 & 0xFFF)
				if words < 2 || pos+words*4 > len(frame) {
					t.Fatalf("block at %d: payload_words %d overruns the frame", pos, words)
				}
				idx := binary.LittleEndian.Uint32(frame[pos+4:]) >> 8
				if int(idx) >= total || seen[idx] {
					t.Errorf("block at %d: index %d is out of range (%d blocks) or repeated", pos, idx, total)
				}
				seen[idx] = true
				if err := canonical(frame[pos : pos+words*4]); err != nil {
					t.Errorf("block %d at %d: %v", idx, pos, err)
				}
				pos += words * 4
			}
			if blocks != f.Blocks || blocks != int(s1&0xFFFFFF) || blocks != f.SOF.TotalBlocks {
				t.Errorf("walked %d blocks, SOF total_blocks %d, manifest %d", blocks, s1&0xFFFFFF, f.Blocks)
			}

			// The reference decode is I420 of the same size, and its quality against the SOURCE is
			// recomputed here: the manifest's number is a claim, the files are the evidence.
			ref := checkFile(t, f.Ref)
			src, err := os.ReadFile(filepath.Join(dir, f.Src)) // its hash is TestManifestHashesAndNoStrayFiles's
			if err != nil {
				t.Fatal(err)
			}
			cw, ch := f.Width/2, f.Height/2
			if want := f.Width*f.Height + 2*cw*ch; len(ref) != want || len(src) != want {
				t.Fatalf("reference decode %d bytes, source %d, I420 %dx%d is %d", len(ref), len(src), f.Width, f.Height, want)
			}
			planes := map[string][2]int{"y": {0, f.Width * f.Height}, "cb": {f.Width * f.Height, f.Width*f.Height + cw*ch}, "cr": {f.Width*f.Height + cw*ch, len(ref)}}
			for name, r := range planes {
				db, _ := psnr(src[r[0]:r[1]], ref[r[0]:r[1]])
				if math.Abs(db-f.PSNR[name]) > 0.01 {
					t.Errorf("PSNR %s of the files is %.2f dB, manifest says %.2f", name, db, f.PSNR[name])
				}
				if db < 20 {
					t.Errorf("PSNR %s %.2f dB under 20: the fixture or the encoder is broken", name, db)
				}
				// encoder_bar: the recorded value minus 1 dB, the recorded value capped at 60.
				if want := math.Round((math.Min(f.PSNR[name], 60)-1)*100) / 100; math.Abs(f.EncoderBar.MinPSNR[name]-want) > 0.01 {
					t.Errorf("encoder_bar %s is %.2f, want %.2f", name, f.EncoderBar.MinPSNR[name], want)
				}
			}
		})
	}
}
