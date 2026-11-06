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
	Pin      struct{ Pyrowave, Granite string }
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
		}
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

// Walk each bitstream by the self-delimiting packet rule (bitstream.md, codec-pyrowave.md
// "packet"): one SOF first, then blocks that end exactly at the frame's end.
func TestBitstreamFraming(t *testing.T) {
	m := load(t)
	if len(m.Fixtures) < 4 {
		t.Fatalf("only %d fixtures", len(m.Fixtures))
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
			if w, h := int(s0&0x3FFF)+1, int(s0>>14&0x3FFF)+1; w != f.Width || h != f.Height || int(s1>>26&1) != f.SOF.ChromaResolution || f.SOF.ChromaResolution != 0 {
				t.Fatalf("SOF says %dx%d chroma %d, manifest %dx%d 4:2:0", w, h, s1>>26&1, f.Width, f.Height)
			}
			blocks := 0
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
				pos += words * 4
			}
			if blocks != f.Blocks || blocks != int(s1&0xFFFFFF) || blocks != f.SOF.TotalBlocks {
				t.Errorf("walked %d blocks, SOF total_blocks %d, manifest %d", blocks, s1&0xFFFFFF, f.Blocks)
			}

			// The reference decode is I420 of the same size, and close to the source.
			ref := checkFile(t, f.Ref)
			if want := f.Width*f.Height + 2*(f.Width/2)*(f.Height/2); len(ref) != want {
				t.Errorf("reference decode is %d bytes, I420 %dx%d is %d", len(ref), f.Width, f.Height, want)
			}
			for plane, floor := range map[string]float64{"y": 20, "cb": 20, "cr": 20} {
				if f.PSNR[plane] < floor {
					t.Errorf("PSNR %s %.2f dB under %v: the fixture or the encoder is broken", plane, f.PSNR[plane], floor)
				}
			}
		})
	}
}
