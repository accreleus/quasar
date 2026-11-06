# PyroWave golden fixtures (#1053)

Encoded PyroWave bitstreams with the reference library's own decode of each, for a few
deterministic synthetic pictures. The WGSL decoder (B05/B06) is checked against the
reference decodes and the agent's `pyrowaveenc` (B07) against the bitstreams. The contract
is `protocol/codec-pyrowave.md`; the spec is #1048, the spike #1049.

**Upstream:** `Themaister/pyrowave@7fe0e2667cd3076b8b2bf04bcb80bc8d691b800b`, Granite
`53090429539bf3404250d18fe58db971f0907c04` (the pins in `deploy/Dockerfile.vulkan` and
`docs/third-party-pins.md`). **Bump the pin only together with regenerated fixtures**:
`control-plane/internal/pyrowavefixtures` fails when the manifest's pin and the Dockerfile's differ.

## Regenerate / check

```sh
# Needs a real GPU: the encoder is Vulkan compute (lavapipe cannot force the subgroup sizes it needs).
# Runs in the platform image's toolchain role (`scripts/dev/dev.sh image`), never on the dev host.
# On an NVIDIA host whose userspace the node agent provisioned into a volume, add NVIDIA_VOLUME=<volume>.
DOCKER='docker -c <lab-host context>' GPU_ARGS='--device nvidia.com/gpu=all --device /dev/dri' \
  NVIDIA_VOLUME=<driver volume> scripts/dev/pyrowave-fixtures/gen.sh   # rewrite testdata/pyrowave/
# (same environment) gen.sh --check               # regenerate to a temp dir, fail on any differing byte
make test-go                                      # hashes, pin and bitstream framing (this directory)
```

`gen.sh` compiles `pyrowave_fixtures.c` against the image's `libpyrowave-shared`, encodes
with the CPU-input path at a fixed per-frame byte budget, writes the bitstream and the
library's decode, and `manifest.py` hashes everything. Pictures are drawn with integer
maths only, so the sources are identical on every machine. They are synthetic, drawn by
`pyrowave_fixtures.c`; no third-party image content is in this directory.

## Files

| file | content |
|---|---|
| `manifest.json` | per fixture: size, budget, SOF fields, block count and sizes, sha256 of every file, PSNR of the reference decode against the source; the pin; the encoder device |
| `<picture>_<W>x<H>.src.yuv` | the source picture, I420 (Y, Cb, Cr planes, 8-bit, no header) |
| `<picture>_<W>x<H>_<rate>.pwv1` | the bitstream in upstream's PWV1 disk format: 24-byte `PWV1Header`, a `u32` frame size, then the frame (SOF packet, then the 32×32 blocks) |
| `<picture>_<W>x<H>_<rate>.ref.yuv` | the reference decode of that bitstream, I420 |

Pictures: `detail` (noise texture, 1-px gratings, checker, diagonals, saturated hard-edged
shapes, a gradient) and `flat` (a neutral field, a ramp, a flat patch: nearly every block
is zero). Sizes: `320x192` (a multiple of 32) and `200x114` (not: edge blocks, and an odd
chroma height of 57). Rates are the per-frame byte budget handed to the codec's exact rate
control, in bytes per pixel: `detail` at `lo` 0.2, `hi` 0.8 and `xhi` 1.5 (the one with blocks
larger than the ~1111 bytes of PyroWave payload a 1200-byte datagram holds under amendment 55,
for the oversized-block split; the largest is 1180 bytes, so none needs more than two parts); `flat` at `lo` 0.1 and `hi` 0.8
(`hi` is far above what it needs, so that stream is complete and well under budget). 4:2:0,
authored as BT.709 limited range. Total about 1.1 MB.

The frame is the library's single packet. The contract's datagram packing is B07's;
`blocks_over_1111` in the manifest counts blocks that would need the oversized-block split
at a 1200-byte datagram.

## Reproducibility

The encoder leaves the alignment bytes at the end of a block (and the unused high bits of the
last sign byte) uninitialised, so two raw runs differ in a few of those bytes and nowhere else;
the decoder ignores them. The generator zeroes them (`canonicalize()`, which also asserts that
every block's computed size rounds up to its `payload_words`). With that, repeated runs on the
generating GPU are byte-identical, bitstreams and reference decodes (three runs on the RTX 5090).
**Another GPU does not reproduce them**: the same generator on the AMD iGPU (RADV) gave different
bitstreams for every fixture (the bitstream is decodable, not specified byte for byte, and the
encoder's float maths differs), with frame sizes within 0.4%, and reference decodes that agree
with the NVIDIA ones at 54 to 93 dB (max error up to 22 code values on `detail_320x192_lo`, 1 to 11
on most). So the files are pinned as generated on the device recorded in `manifest.json`, and
`gen.sh --check` passes only on that GPU class.

## Using them

- **Decoder tests:** feed the `.pwv1` frame to the decoder, compare with `.ref.yuv` by PSNR or
  max error. The decode is **not bit-exact** across devices (`bitstream.md`), so use the
  spike's bar (PSNR ≥ 45 dB and max error ≤ 1 against the reference); never compare hashes.
- **Encoder tests:** the bitstream is the library's output on one device, not a normative
  stream. Compare by decoding and by PSNR against the source, not by bytes, unless the
  test runs on the generating GPU class (`encoder_device` in the manifest).
