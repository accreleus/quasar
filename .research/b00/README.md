# B00 spike: PyroWave encode cost and a WGSL decoder (accreleus/quasar#1049)

This is throwaway research for spec #1048. It lives on branch `research/b00-pyrowave`, which never merges.

- `index.html` + `decoder.js`: a WebGPU port of PyroWave's decoder (dequantise, inverse DWT, YCbCr→RGB) that runs from `file://` with no server.
- `fixtures/`: three golden frames, each encoded and reference-decoded by upstream's own C/Vulkan tools.
- `results/`: native timings from both lab GPUs and Chrome runs on three adapters.
- `tools/`: everything used to make the fixtures and the numbers.

Upstream: Themaister/pyrowave `7fe0e2667cd3076b8b2bf04bcb80bc8d691b800b` (2026-10-27), with Granite at the `53090429` pin from `checkout_granite.sh`. Both are MIT.

## Timing it on the Mac (Apple M5 Pro)

1. Get only this directory:
   ```sh
   git clone --depth 1 --filter=blob:none --sparse -b research/b00-pyrowave https://github.com/accreleus/quasar.git b00
   cd b00 && git sparse-checkout set .research/b00
   open -a "Google Chrome" .research/b00/index.html
   ```
   Dragging `index.html` onto a Chrome window works too.
2. Optional, for raw GPU timestamps: open `chrome://flags/#enable-webgpu-developer-features`, set it to Enabled, then relaunch Chrome. Without the flag, Chrome rounds each timestamp to 100 µs. The page repeats every timed pass up to 200 times and divides, so the error stays at about 0.5 µs or less either way.
3. Plug in power and close other GPU-heavy tabs. The page runs everything on load: each fixture (1080p, 1440p, 4K) on every feature path the adapter offers. Wait until the status line says `done` (under a minute).
4. Record:
   - a screenshot of the whole page;
   - the JSON, from the DevTools console: `copy(JSON.stringify(B00_RESULT, null, 2))`.

   The "Run all fixtures × all paths" button runs everything again.

How to read the page:
- **Header line:** the adapter, plus whether `subgroups` (with subgroup size range), `shader-f16` and `timestamp-query` exist.
- **Correctness table:** each fixture × path, compared with the C reference decode by PSNR on Y/Cb/Cr and by max |Δ| on Y. "match" means at least 45 dB on every plane. Every run so far reads about 64–65 dB, with max |Δ| = 1 code value.
- **Timing table:** GPU µs per pass. `dequant`, `idwt L4`…`L0`, and `idwt (all levels)` are each timed alone. `decode (dequant + idwt)` is the decoder's GPU cost per frame. `yuv2rgb` is the draw. `×N` is the repetition count; it drops automatically on slow GPUs so that no pass exceeds about 50 ms.
- "Own file" decodes any `.pwv1` made with `pyrowave-encode` (4:2:0 only). The reference `.y4m` is optional.

## What the WGSL port does

| Stage | Upstream (GLSL/Vulkan) | This port (WGSL) |
|---|---|---|
| Packet walk | C++ `push_packet` | JS `packFrame`: one `offsets[block_index]` table plus the packed payload words, uploaded with `writeBuffer` |
| Dequant | `wavelet_dequant.comp`: one 128-lane workgroup per 32×32 block; 8-bit, 16-bit and 32-bit SSBO views | Same mapping. Bytes and halfwords are unpacked from `array<u32>`, since WGSL has no 8-bit storage. A per-block band table replaces 40 per-dispatch descriptors, so all bands go in one dispatch. |
| iDWT | `idwt.comp`: `textureGather` on a mirrored-repeat sampler, fp16 shared memory, 13 dispatches | Storage buffers with explicit mirroring. Upstream's sampler trick works out to whole-sample symmetric extension of the interleaved signal, so it's equivalent. Each 32×32 output tile is lifted in fp32 workgroup memory, one parallel step at a time. One dispatch per level (z = component), 5 in total. |
| YCbCr→RGB | `yuv2rgb.frag` | Same matrices and range handling, as a render pass reading the planes buffer (bilinear chroma) |
| Precision | `PYROWAVE_PRECISION=1`: fp32 maths; fp16 storage for levels 0–1, fp32 for levels 2–4 | Same with `shader-f16`. Without it, everything is fp32. |

### Feature paths

| Path | Used when | What differs |
|---|---|---|
| fast | `subgroups` + `shader-f16` | `subgroupInclusiveAdd` for the 16-lane byte-offset scan and the 128-lane sign-bit scan. Level 0–1 coefficients stored as fp16. |
| subgroups, fp32 storage | `subgroups` only (Chrome on Linux/NVIDIA) | Same scans, fp32 storage |
| shared-memory scans, fp16 storage | `shader-f16` only | See fallback |
| fallback | neither | Byte-offset scan: 4-step Hillis–Steele in workgroup memory. Sign scan: 7-step Hillis–Steele (14 barriers instead of 2). fp32 storage, which doubles level 0–1 memory. The iDWT is identical on every path. |

Two details of the subgroup path:
- WGSL guarantees no mapping from subgroup lanes to `local_invocation_index`, and has no portable `gl_SubgroupID`. The fast path therefore builds its own linear lane index: the elected lane takes a workgroup-atomic ticket, `subgroupBroadcastFirst` shares it, and the index is `ticket * subgroup_size + subgroup_invocation_id`, as upstream does.
- The 16-lane scan uses subgroups only when `adapter.info.subgroupMinSize >= 16` (an override constant). Otherwise it uses workgroup memory. SwiftShader (size 4) covers that case.

## Results

### Native encode/decode (upstream `pyrowave-bench`, 10 000 iterations, GPU timestamps)

Synthetic game-like 4:2:0 frames (`tools/gen_frames.py`): fractal-noise terrain and foliage, saturated hard-edged shapes, HUD text, thin-line grids and a sky gradient. They were rendered at 4K and Lanczos-downscaled. The foliage noise is close to worst case for a wavelet codec, so treat these PSNR numbers as a floor. The bitrate is the per-frame byte budget at 60 fps. PSNR is plain PSNR against the source, averaged over 3 frames.

| res | Mbit/s @60 | bytes/frame | RTX 5090 encode µs | RTX 5090 decode µs | AMD iGPU encode µs | AMD iGPU decode µs | PSNR Y | PSNR Cb | PSNR Cr | YCbCr 6:1:1 |
|---|---|---|---|---|---|---|---|---|---|---|
| 1080p | 100 | 208294 | 108 | 49 | 2076 | 1315 | 37.94 | 40.91 | 41.50 | 38.76 |
| 1080p | 200 | 416624 | 110 | 53 | 2172 | 1347 | 44.76 | 48.85 | 49.70 | 45.89 |
| 1080p | 300 | 624966 | 111 | 56 | 2279 | 1382 | 48.23 | 54.00 | 54.15 | 49.69 |
| 1080p | 400 | 833305 | 113 | 58 | 2369 | 1413 | 52.57 | 57.82 | 59.13 | 54.05 |
| 1440p | 100 | 208310 | 130 | 69 | 3334 | 2259 | 35.98 | 37.17 | 38.42 | 36.44 |
| 1440p | 200 | 416617 | 128 | 73 | 3426 | 2292 | 40.31 | 43.79 | 44.68 | 41.29 |
| 1440p | 300 | 624954 | 130 | 78 | 3539 | 2326 | 44.62 | 48.48 | 49.46 | 45.70 |
| 1440p | 400 | 833221 | 138 | 81 | 3694 | 2361 | 47.28 | 52.13 | 52.67 | 48.56 |
| 2160p | 100 | 208309 | 227 | 146 | 6733 | 5059 | 31.47 | 33.10 | 34.74 | 32.09 |
| 2160p | 200 | 416585 | 218 | 138 | 6859 | 5131 | 36.06 | 38.32 | 39.75 | 36.81 |
| 2160p | 300 | 624984 | 224 | 143 | 7059 | 5194 | 39.18 | 42.63 | 43.65 | 40.17 |
| 2160p | 400 | 833301 | 215 | 144 | 7076 | 5245 | 41.61 | 45.50 | 46.66 | 42.73 |

- **RTX 5090:** NVIDIA lab host, driver 615.78.08, Vulkan 1.4.
- **AMD iGPU:** AMD lab host, Raphael 2-CU iGPU, Mesa RADV 25.2.8.
- **Encode** covers DWT + quant + rate-control analyse/resolve + packing, without the readback. On the 5090 at 1080p/200: DWT 17, quant 45, analyse 11, resolve 4, packing 27 µs. Readback of the bitstream to host memory is another ~73 µs.
- **Size:** the rate control is exact, so the output lands within 0.01% of the budget at every point.
- Raw data: `results/native-*.csv`.

### WGSL decoder in Chrome 141 (headless, real GPUs on the lab hosts; reps ≤ 200)

| adapter | path | 1080p decode µs | 1440p decode µs | 4K decode µs | 1080p dequant / iDWT / rgb µs |
|---|---|---|---|---|---|
| RTX 5090 (Vulkan) | subgroups, fp32 | **57** (native 53) | 82 (native 73) | **149** (native 143) | 17 / 41 / 6 |
| RTX 5090 (Vulkan) | fallback | 58 | 83 | 148 | 17 / 41 / 6 |
| AMD iGPU (RADV) | fast (subgroups + f16) | 7214 (native 1347) | 12767 | 28315 | 573 / 6629 / 593 |
| AMD iGPU (RADV) | subgroups, fp32 | 7635 | 13507 | 29640 | 624 / 6975 / 593 |
| AMD iGPU (RADV) | f16, shared scans | 8000 | 13715 | 29822 | 1333 / 6676 / 591 |
| AMD iGPU (RADV) | fallback | 8328 | 14233 | 31105 | 1332 / 7002 / 597 |

Correctness: every path on all three adapters (SwiftShader too) matches the C reference within 1 code value. PSNR against the reference decode is 63.9–65.5 dB.

What the numbers say:
- **Dequant** runs at native speed everywhere. On the 5090: 17 vs 21 µs native. On the iGPU: 573 vs 620 µs.
- **Subgroups** speed up dequant 2.3× on the iGPU, and make no difference on the 5090.
- **fp16 storage** saves about 5% of iDWT time on the iGPU. Chrome does not expose `shader-f16` on NVIDIA/Vulkan at all, so the fp32-storage path is a real target, not just a fallback.
- **Dispatch count matters.** Dawn puts a barrier between dispatches, at about 5–8 µs each on the 5090. Going from 13 iDWT dispatches to 5 took the 1080p iDWT from 114 µs to 47 µs, with the kernel otherwise unchanged.
- **iDWT on the 2-CU iGPU** is about 9× native. With the lifting steps switched off (`?skiplift=1`, diagnostics only) it still takes 3.6 ms at 1080p. So half the cost is storage-buffer load/store throughput: upstream reads R16F images through `textureGather` and writes R8 planes. The lever, unexplored here, is textures for the coefficient and plane storage (`r16float` storage needs `texture-formats-tier1`).
- **CPU parse + `writeBuffer`** (JS, first call) takes 0.2–6.6 ms. That is noisy, and a real client should assemble the payload buffer while datagrams arrive rather than walk the frame afterwards.

Raw data: `results/chrome141-*.json`. Screenshots: `results/chrome141-*.png`.

## Reproducing

Native, in a throwaway container with a Vulkan GPU:
1. Clone upstream at the pin and run `checkout_granite.sh`, or do a full recursive Granite clone at its pin.
2. `git apply tools/upstream-bench-headless.patch`. It changes two things: `bench.cpp` takes the per-frame byte budget from `PW_BYTES`, and the devel build uses Granite's `headless` platform instead of SDL.
3. Build: `cmake -G Ninja -DCMAKE_BUILD_TYPE=Release -DPYROWAVE_DEVEL=ON && ninja pyrowave-bench pyrowave-encode pyrowave-decode`.
4. Run `python3 tools/gen_frames.py /root/frames 3`, then `tools/sweep.sh`.

Fixtures: `tools/make_fixture.py NAME in.pwv1 ref_dec.y4m fixtures/NAME.js` (frame 0 only).

Headless correctness: `node tools/headless.mjs <this dir> <out dir>` inside `mcr.microsoft.com/playwright:v1.56.0-noble`, with `npm i playwright@1.56.0` run first.
- With no flags, it uses SwiftShader. Those timings are meaningless; only the correctness is checked.
- `HW=1` uses the real GPU through Vulkan.

## Not done here

- 4:4:4 decoding.
- Packet-loss concealment.
- Datagram reassembly.
- Presenting through the shared WebGPU presenter: the page draws into an offscreen texture and copies it to a 2D canvas, and that copy is not timed.
- The fragment-shader iDWT path that upstream prefers on Mali and Adreno.
