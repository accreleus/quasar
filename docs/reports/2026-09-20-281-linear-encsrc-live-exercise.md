# #281 — linear-first Vulkan encode-src: live exercise

| | |
|---|---|
| Ticket | #281. Fixes #272. Parent spec #252 |
| Branch | `fix/281-linear-encsrc-default`, tip `21524ec` (from `origin/develop` at `fcaaae9`) |
| Not on | `develop`, `main`, `initiative/resilient-host-architecture`. No tag, no published image, no Actions build |
| Fork commit | `salty2011/gst-wayland-display@631cebb7318596e06de156010055219f4d61a9b3` |
| Fork `develop` | **fast-forwarded** `1ea8092` → `631cebb` after this exercise passed on both vendors, which is the only condition the owner authorised it on |
| Pin | `GST_WAYLAND_DISPLAY_REF` in `deploy/pins.env` **and** `deploy/Dockerfile.vulkan` = `631cebb…`, equal to fork `develop` |
| Images | `quasar-node-agent` from source `8c5b716` (contract 148 passed / 0 failed / 2 GPU-gated skips), `quasar-control-plane` `dev-901058f`, `quasar-updater` `dev-7e5f859`. Built with `deploy/build-images.sh runtime --no-prune`. Nothing published |
| Gates | `make test-rust` / `test-go` / `test-web` / `verify` / `preflight` all rc 0; leak scan clean on tree and tracker |

This records the live exercise `docs/third-party-pins.md` "Fork-bump verification policy"
requires. **It does not merge anything.** #272 and #281 stay open for the owner.

## What the change is

The compositor allocated its Vulkan encode-src image `OPTIMAL` (tiled) unless an operator set
`WOLF_VULKAN_LINEAR_ENCSRC=1`. On several AMD generations that default corrupts the picture:
the RGBA→NV12 compute pass writes a LINEAR scratch image and then copies it into the encode-src
image, and when that image is tiled the copy crosses a swizzle-mode boundary radv mishandles.
GFX12/RDNA4 is what the knob was originally added for; GFX10.3/RDNA2 (the AMD test host's
Raphael / Granite Ridge iGPU, VCN 3.1.2) is #272.

`631cebb` inverts the default: **LINEAR first, automatic fall back to tiled when the driver
refuses the allocation**, which is what NVIDIA's Vulkan-Video encoder does. No encoder default
moved, no GPU allowlist, and nothing for a self-hoster to set.

### Two things the issue text had wrong

**LINEAR does not remove the copy on this path.** #281 and a comment on #272 say the compute
shader writes the encode-src image "directly — no scratch, no copy", and that this is "one
fewer full-frame write of bandwidth per frame". Reading `wayland-display-core/src/utils/
vulkan_nv12.rs`: the Vulkan encode-src constructor `new_on_shared` hard-codes
`direct: false, encode_src: true`, its per-slot builder `create_encode_output` unconditionally
creates a LINEAR STORAGE scratch, and `record()` copies whenever a scratch exists. The truly
direct, no-copy path exists only on the **dmabuf/VA export** constructor. So LINEAR changes the
*destination tiling* of a copy that always happens — which is still a complete explanation of
the fix, and is what the fork's own code comment always said.

**The allocation ladder had to be reordered.** It was flags-outer, tiling-inner, which is
harmless while LINEAR is opt-in. Flipped naively, NVIDIA would have had its readable-superset
encode-src image (`SAMPLED|TRANSFER_SRC|MUTABLE_FORMAT`) refused *for its tiling*, and the
ladder would have settled on encode-only `OPTIMAL` — silently costing NVIDIA the images
`vulkanscale` (#501, the ABR external-resolution lever) needs, and failing any session with the
scaler in the graph. The ladder is now **tiling-outer, flags-inner**, so NVIDIA retries the
readable superset at `OPTIMAL` and lands exactly where it lands today. This was found before
the build, and it is checked below on hardware.

A per-device latch also pins the tiling once one ring slot has allocated: the function runs once
per ring slot, and a transient failure on one slot could otherwise split a ring across two
tilings and put some frames back on the corrupting path.

## The live-exercise matrix

Every line is marked **performed**, **unperformed** or **failed**. Evidence by role only.

### The AMD test host

| # | What | Result |
|---|---|---|
| 1 | Fresh stack, nothing set: effective encoder is `vulkanh264enc` | **performed** — agent log `encoder resolved … encoder="vulkan" source="dri-scan" vendor="amd"`, then `Vulkan hardware encoder: vulkanh264enc` |
| 2 | The compositor picks LINEAR by itself | **performed** — `vulkan_share: encode-src tiling=LINEAR (WOLF_VULKAN_LINEAR_ENCSRC unset = linear-first)`, and **no** readable-superset rejection warning, so the `SAMPLED\|TRANSFER_SRC\|MUTABLE_FORMAT` image was granted at LINEAR |
| 3 | Real browser session at 1080p, decoded frame captured | **performed** — clean ([before](2026-09-20-281-282-validation/amd-1080p-before-corrupt.png) / [after](2026-09-20-281-282-validation/amd-1080p-after-clean.png)) |
| 4 | Real browser session at 720p, decoded frame captured | **performed** — clean ([before](2026-09-20-281-282-validation/amd-720p-before-corrupt.png) / [after](2026-09-20-281-282-validation/amd-720p-after-clean.png)) |
| 5 | Judged by eye **and** numerically | **performed** — see "The number" below |
| 6 | At least two consecutive sessions, each with a clean teardown | **performed** — three (1080p, 720p, 1080p again); every one reached `stopped`, and after the last the host had no `quasar-sess-*` / `quasar-pulse-*` / `quasar-probe-*` container and `/run/quasar-agent` was empty |
| 7 | Mouse, keyboard and audio | **performed** — right-click opened the desktop menu, `Ctrl+Alt+T` opened a terminal and a typed command ran and printed, audio energy 0 while silent → 2.01 during a 440 Hz tone |
| 8 | Knob forcing tiled ⇒ corrupt again | **performed** — `WOLF_VULKAN_LINEAR_ENCSRC=0` logs `encode-src tiling=OPTIMAL (… forced-tiled)` and the picture is corrupt again ([frame](2026-09-20-281-282-validation/amd-1080p-after-knob-forced-tiled-corrupt.png)) |
| 9 | `QUASAR_ENCODER=va` still clean ⇒ the VA path is not regressed | **performed** — `vah264enc`, and the decoded frame is **PSNR 99.0 dB / byte-identical** to the VA frame captured before the change |
| 10 | `encode_ms` linear against tiled | **performed** — see the table below |

### The NVIDIA test host

| # | What | Result |
|---|---|---|
| 11 | Fresh stack, nothing set: the fallback warning appears **once per session** | **performed** — exactly one `vulkan_share: LINEAR encode-src allocation refused by the driver -- using tiled (OPTIMAL). This is expected on NVIDIA; no action is needed.` per session |
| 12 | The encoder is the same as in the baseline | **performed** — `vulkanh264enc` at 1080p, `vulkanav1enc` at 1440p, both unchanged |
| 13 | **The readable superset is retained** (the regression the ladder reorder exists to prevent) | **performed** — no `rejected SAMPLED\|TRANSFER_SRC\|MUTABLE_FORMAT` warning, `encode-src tiling=OPTIMAL`, and the agent reports `external_resize_supported: true` for every session, so `vulkanscale` still has readable images |
| 14 | Clean at 1080p H.264 | **performed** — **PSNR 78.3 dB** against the before-frame ([before](2026-09-20-281-282-validation/nvidia-1080p-h264-before-clean.png) / [after](2026-09-20-281-282-validation/nvidia-1080p-h264-after-clean.png)) |
| 15 | Clean at 1440p AV1 | **performed** — **PSNR 99.0 dB**, byte-identical to the before-frame ([before](2026-09-20-281-282-validation/nvidia-1440p-av1-before-clean.png) / [after](2026-09-20-281-282-validation/nvidia-1440p-av1-after-clean.png)) |
| 16 | Mouse, keyboard and audio | **performed** — as row 7; audio 0.02 → 1.99 on the tone |
| 17 | Startup time not worse than the baseline | **performed** — browser time-to-decode 1003 ms before, 1003 ms after |
| 18 | VRAM steady across at least two consecutive sessions | **performed** — see the table below |
| 19 | NVENC fallback smoke | **performed by the equivalent procedure** — see below |
| 20 | Bench run submitted to `quasar-bench` | **UNPERFORMED** — no `quasar-bench` is reachable: nothing answers on port 9400 on this workstation or on any of the three lab hosts, and `hosts.json` carries no bench entry. Recorded rather than skipped silently; it is the one item of the pins-doc four that could not be done |

### Both hosts

| # | What | Result |
|---|---|---|
| 21 | #264 readiness fault harness, rerun serially, no row regression | **performed** — AMD **104 pass / 0 fail / 1 unperformed**, NVIDIA **97 / 0 / 4**. Both are *exactly* the reference totals recorded at `23e995d` in `docs/reports/rh02-265/rerun-23e995d/` |
| 22 | HEVC from a browser | **UNPERFORMED** — the test browser cannot decode HEVC. Unchanged from the RH-02 record |
| 23 | Intel | **UNPERFORMED** — nothing is claimed; there is no Intel hardware in the lab |

## The number

Both hosts run the same deterministic picture: an application rendering
`videotestsrc pattern=smpte100` at a fixed 1920x1080 — pure vertical bars with no motion and no
noise, so **every row of a correct decode is identical**. `row_std`, the standard deviation of
the per-row mean luma, is therefore ~0 for any correct decode whatever the encoder, and needs no
reference frame at all.

That matters, because the obvious metric does not work: PSNR between `vah264enc` and
`vulkanh264enc` output bottoms out around 24 dB purely from edge ringing on the bar boundaries —
the same value a genuinely corrupt frame scores. `row_std` is immune to it.

| host | encoder | pin | `row_std` | verdict |
|---|---|---|---|---|
| AMD test host | `vah264enc` 1080p | before | 0.002 | clean (the reference) |
| AMD test host | `vah264enc` 720p | before | 0.003 | clean |
| AMD test host | `vulkanh264enc` 1080p | **before** | **4.176 / 4.547** | **corrupt** |
| AMD test host | `vulkanh264enc` 720p | **before** | **4.553** | **corrupt** |
| AMD test host | `vulkanh264enc` 1080p | **after** | **0.000** | **clean** |
| AMD test host | `vulkanh264enc` 720p | **after** | **0.000** | **clean** |
| AMD test host | `vulkanh264enc` 1080p, knob forcing tiled | after | **4.277 / 4.290 / 4.494** | **corrupt** — the knob reproduces it |
| AMD test host | `vah264enc` 1080p | after | 0.002 | clean, unchanged |
| NVIDIA test host | `vulkanh264enc` 1080p | before / after | 0.000 / 0.000 | clean both sides |

Corrupt separates from clean by roughly three orders of magnitude, and the *same* value comes
back whether the corruption is the old default or the new knob forcing the old path. That is
what makes this a controlled result rather than a coincidence.

## `encode_ms`, linear against tiled

Reported whichever way it went. Agent telemetry, median of the per-sample `encode_ms_p50` over a
steady 1080p60 session, AMD test host, two runs per arm on the same agent image and the same
picture — the only difference is `WOLF_VULKAN_LINEAR_ENCSRC`.

| encode-src tiling | run 1 | run 2 | p95 (median of samples) |
|---|---|---|---|
| **LINEAR** (the new default) | **3.003 ms** | **3.000 ms** | 3.090 / 3.109 ms |
| `OPTIMAL` (forced tiled) | 3.365 ms | 3.353 ms | 3.475 / 3.466 ms |

**LINEAR is about 0.36 ms (≈11%) faster.** That contradicts the expectation this exercise
started with — having established that LINEAR does *not* remove the copy, no improvement was
predicted and a modest regression was thought possible, since the VCN engine now reads a linear
surface. The measurement says the cheaper copy (LINEAR→LINEAR needs no retile) more than pays
for it on this GPU. The first tiled number was re-measured before being believed.

NVIDIA is unaffected by definition — it never receives a linear image — and its numbers confirm
it: `encode_ms_p50` 1.266 ms before, 1.159 ms after at 1080p H.264; 1.693 ms before, 1.659 ms
after at 1440p AV1.

## VRAM across sessions, NVIDIA test host

`nvidia-smi --query-gpu=memory.used`, read before, during and after each session.

| point | before the change | after the change |
|---|---|---|
| before session 1 | 34 MiB | 34 MiB |
| during session 1 (1080p H.264) | 779 MiB | 779 MiB |
| after session 1 teardown | 39 MiB | 39 MiB |
| during session 2 (1440p AV1) | 812 MiB | 812 MiB |
| after session 2 teardown | 39 MiB | 39 MiB |

Identical on both sides, and steady across two consecutive sessions.

## NVENC fallback smoke

`make nvenc-fallback-smoke` runs against a host's **standard** stack, and tonight's rule is that
every test stack uses ports distinct from every other install; pointing the operator's host
configuration at a temporary stack was judged more invasive than the value. So the harness's own
two load-bearing assertions were made directly, against the running agent, with
`QUASAR_VULKAN_H264=0` set for that run only and reverted afterwards:

- **G1b, the agent's own `probe-encoder` report** — `effective_encoder = nvenc`,
  `encoder_factory = nvh264enc`, `ok = true`. (`nvh264enc` rather than `nvcudah264enc` is the
  GStreamer-1.28 naming the harness itself accepts.)
- **G1c, the pipeline's structured log line for the live session** — `codec fallback:
  configured=Vulkan codec=h264 → effective=Nvenc (element=nvh264enc; …)`, carrying that
  session's id.
- A real Chrome peer decoded it: frames advanced monotonically 822 → 1185, **zero freezes, zero
  dropped**, picture clean (`row_std` 0.000).
- Teardown clean: terminal state reached, the agent container still running with an unchanged
  restart count, and **zero** `SIGSEGV` / `libnvcuvid` / `GLib-*-CRITICAL` lines in the agent
  log across the whole run.

This is marked **performed by an equivalent procedure**, not "the harness passed".

## Gates

Run serially on the final commit, and compared against a baseline taken on the untouched branch
point before any work started.

| gate | baseline (`fcaaae9`) | this branch (`21524ec`) |
|---|---|---|
| `make verify` | rc 0 — 437 pass / 0 warn / 0 fail | rc 0 — **437 / 0 / 0** |
| `make test-rust` | rc 2 — **flaky**, see below | rc 0 — 1724 passed, 0 failed, 9 ignored |
| `make test-go` | not taken | rc 0 — 43 packages ok |
| `make test-web` | not taken | rc 0 — 236 files, 3080 tests |
| `make preflight` | not taken | rc 0 |
| leak scan, tree + `--issues` | not taken | **clean** both, with the operator pattern set loaded |

### The baseline `make test-rust` was already red, and it is flaky

This is why the baseline was taken. On the **untouched branch point**, five consecutive runs of
`make test-rust` gave: 3 failures, then 1 failure, then 1, then 1, then a clean pass — and the
failing test was not the same one each time. Two independent groups are involved:

- `session::settings::tests::default_matches_env_baseline`,
  `…::effective_map_contains_encoder_and_render_node` and
  `capacity::tests::render_node_pin_zeroes_the_excluded_gpu_and_keeps_indices_stable` read the
  process-global encoder default while `capacity.rs` tests call
  `std::env::set_var("QUASAR_ENCODER", …)` with no serialisation. Rust runs tests in parallel
  threads, so one suite's `set_var` is read by another's assertion.
- `container_ownership::tests::ownership_survives_restart_and_distinct_agents_are_isolated`
  failed once on its own.

Nothing here is caused by this branch — the branch changes no Rust at all — and it is filed
separately. It is recorded because "the gate is green" would otherwise be a claim that depends
on which run you looked at.

## What went wrong on the way, and what was done

| what | diagnosis | what was done |
|---|---|---|
| The fork change did not compile: `cannot borrow profile_list as mutable more than once` | The new tiling loop keeps both `ImageCreateInfo` builders alive across iterations, so their `push_next(&mut profile_list)` borrows overlap; previously the first was dead before the second was built | Gave the encode-only builder its own, byte-identical profile chain. Duplication for the borrow checker, commented as such |
| The first fault-harness run on the AMD test host failed 3 rows in scenario 2a (`no_host_available`, `gpus=[]`) | **Environment, not the branch.** The daemon log carried `CDI: … couldn't initialize inotify: too many open files`; `fs.inotify.max_user_instances` is 128 and two full Quasar stacks plus the harness's nested engines exhausted it | Tore down the cohabiting stack and reran on a clean host: 104 / 0 / 1, the reference totals. The limit is the Unraid host's kernel setting and was **not** touched |
| Two launches returned `no_host_available` immediately after an agent recreate | Transient; the host was online, unblocked and had free slots seconds later | Added a settle wait after a recreate and retried. Not diagnosed further — it is not in this ticket's scope and it is recorded here as an observation |
| The first `encode_ms` tiled/linear comparison looked like an outlier | — | Re-measured; both arms reproduced to within 0.012 ms |

## Not verified

- A native Unraid install (the owner's own machine was not connected to at all).
- HEVC from a browser — the test browser cannot decode it.
- Intel — no hardware.
- Multi-GPU placement.
- The `quasar-bench` run — no bench service is reachable (row 20).
- A driver that *accepts* a LINEAR encode-src and then corrupts or fails at **encode** time. No
  allocation-time fallback can see that; #282 is the safety net for it.

## The fork's other branches — inventory only, nothing merged

Taken before the change, for #284, which owns them.

| branch | relationship |
|---|---|
| fork `develop` vs the old pin `310c03e` | 2 commits ahead: `b7a506d` (scope one GPU test to AMD/Intel) and `1ea8092` (`cargo clippy --fix` + `fmt` sweep). **Both read and confirmed mechanical** — the clippy sweep is lifetime elision, redundant-cast removal and `(x+7)/8` → `div_ceil(8)`, which is exactly equivalent for unsigned; the test change swaps a render-node selector and adds a skip. They are included in the new pin |
| `sync/upstream-stable` vs `develop` | 18 ahead, 0 behind |
| `fix/keymap-memfd-teardown` | 9 ahead of `develop`, 130 behind; 1 ahead of `sync/upstream-stable`, 140 behind |
| `patches/vulkan-av1-encode` | 32 ahead of `develop`, 58 behind |

None of the three was merged into anything, so this exercise attributes its picture change to one
cause. Findings are on #284.
