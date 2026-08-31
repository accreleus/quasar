# Third-party media-plugin pins (and how to flip them to a release)

Quasar builds its GStreamer plugins **from source at specific commits**, because the features
we depend on are not yet in tagged upstream releases. All pins live as **build ARGs** in
`deploy/Dockerfile.vulkan` — the single image lineage (targets `runtime`/`dev`/`nv`, all on the
`ghcr.io/accreleus/quasar-base` family) — so flipping to a release once it merges is a one-line
change (or a `--build-arg`), with no Dockerfile surgery.

> **Rule (changed 2026-08-20):** **`deploy/pins.env` is the source of truth.** It is plain
> `KEY=VALUE`, so CI and `deploy/toolchain-hash.sh` can read pins without parsing Docker syntax.
> `deploy/Dockerfile.vulkan` declares each pin exactly once, at global scope (before the first
> `FROM`), and `deploy/build-images.sh` asserts on every build that the two files agree and that
> no pin has a second declaration — a mismatch is a hard build failure, not a doc-drift problem.
> Stages that need to read a pin (for a provenance `LABEL`) re-declare it **bare**, inheriting
> the global default; never re-add a `=value`.
>
> Keep this doc in sync when a pin moves — it carries the *why*, which neither file does.
>
> **Cost note:** the pins marked TOOLCHAIN in `pins.env` feed the `quasar-gst-toolchain:<hash>`
> artefact tag. Moving one means a ~40-minute toolchain rebuild (once, then reused by every
> agent image); moving `DOCKER_VERSION` or `SAMPLY_VERSION` costs a ~5-minute image build.

> **History:** the Ubuntu-based `Dockerfile.dev` / `Dockerfile.nv` (gst-1.24, pins `cbfdfe5` /
> `c49af96`) and their GW-02 in-compositor-NV12 runtime knobs were cut over and deleted
> 2026-07-17 — recover the old pin tables from git history if ever needed.

## `deploy/Dockerfile.vulkan` pins (VK-01, unified Vulkan-encode image)

This is the **canonical agent image** (`quasar-node-agent`); the earlier `Dockerfile.agent.prod`
(AMD VA, Ubuntu) has been deleted and `Dockerfile.nv` (NVENC, Ubuntu) is legacy-pending-cutover
(see below). It builds a patched GStreamer from source to get `vulkanh264enc`
(GPU-vendor-agnostic Vulkan video encode), per the adoption spec
`docs/research/vulkan-encode-unified-pipeline-adoption-spec.md`. Its `build`/`runtime`/`nv`
stages build on **`ghcr.io/accreleus/quasar-base`** (the shared Fedora 43 base from the
`quasar-images` repo, which owns the Fedora digest pin), overridable via the
`QUASAR_BASE_IMAGE` build ARG. That ref is itself a pin — `QUASAR_BASE_IMAGE` in
`deploy/pins.env` — and `deploy/build-images.sh` + `deploy/redeploy.sh` read it from there
(via `deploy/lib/pins.sh`) rather than each carrying their own literal. `:latest` is the
channel `quasar-images` publishes from its `stable` branch; a release overrides the ARG with
an exact `@sha256:` digest, which `scripts/release/release-preflight.sh` requires.

| Build ARG / value | Pinned to | Why |
|---|---|---|
| `GST_VERSION` | **`1.28.4`** (git branch, cloned `--depth 1`) | `vulkanh264enc` only exists from GStreamer 1.28; earlier Quasar images build 1.24 from source (wayland version needs), this one builds a full GStreamer release because the patches below apply to `gst-plugins-bad` itself |
| `GST_WAYLAND_DISPLAY_REPO` + `GST_WAYLAND_DISPLAY_REF` | **`https://github.com/salty2011/gst-wayland-display.git`** @ **`310c03eca4b90b653885def4e3348034067c8c72`** (branch `develop`, bumped 2026-08-25 for the #423/#487 fork batch. **The CUDA slot-owns-one-ref fix** is what the batch's own acceptance gate bought: once `cuda_element_churn_is_critical_free` could actually execute (it had self-skipped forever — the `dev` stage dropped the GStreamer registry *before* its `gst-inspect` sweep, re-baking a GPU-less registry in which `cudaconvert` never registers), it immediately caught a second CUDA context over-unref. With `cudaconvert` downstream the slot's NULL→ctx transition happens inside the re-entrant `set_context` frame, so both the inner and the outer wrapper claimed the same transferred reference; dropping the redundant outer wrapper over-unreffed the context, and pipeline dispose then hit `g_object_unref: assertion G_IS_OBJECT failed`. Transfer attribution is undecidable per-frame across that nesting, so the model is now ownership by position: the slot owns exactly one reference from the moment it is non-NULL, released once in element dispose, and every wrapper takes its own. Verified: at the previous pin the gate reproduces the CRITICAL; at this pin both churn tests pass. Intermediate pin `4695271e` (same day) carried **#487 park-and-retry** — a toplevel that commits a mapped buffer before the `wl_output` exists is left parked in `pending_windows` instead of being `swap_remove`d and dropped on the early return, and `apply_output_mode` sends it the deferred initial configure (`configure_pending_toplevels`), because a client blocked on that configure will never commit again on its own. The ordering has never fired in production — the output is created ~140 ms before any client connects — but that is a timing accident, not an invariant, and the failure mode is a silent invisible app. **#423 churn acceptance gate** — `element_churn_is_critical_free` / `cuda_element_churn_is_critical_free` in `gst-plugin-wayland-display/tests/encode.rs` drive 8 full element lifecycles with `log_set_always_fatal(LEVEL_CRITICAL)`, turning "teardown emits no GLib CRITICAL" into a standing test. The two over-unrefs themselves were already fixed by `ded71c0` and `49294d4`, so the tests pass at the previous pin too — they are a forward gate, not a delta. Previous pin `880e9b3abd8c7f98e16b8725170fd82758b1591f` (2026-08-22, #501; second bump the same day pins the h265 GPU test at profile=main, test-only: adds the `vulkanscale` element, a GPU scaler for `memory:VulkanImage` NV12/P010 that gives the Vulkan encode path the live external-resolution lever; encode-src images now request the SAMPLED|MUTABLE_FORMAT superset, measured free. Previous pin `77b303bc` was render-size + ui-scale: live `render-width`/`render-height`/`ui-scale` properties, fractional-scale support, and composite-at-render-size, so the compositor can render below the client's requested resolution and scale the UI independently; also drags in three previously-unpinned backports since the old `507887d` pin — `ffafb7c`, `efd85db`, `f9ea08a` — already present on the fork's `develop` and now captured by the pin bump) | **Changed 2026-07-26: the image now builds the Quasar fork's `develop` branch, not upstream + a patch stack.** `develop` = upstream `43d4c25` (games-on-whales PR #37 `feat/vulkan-encode`) plus the ten Quasar commits that were previously applied here as loose `git apply` steps, one commit per former patch, each carrying its provenance. The branch tip was verified **byte-identical** to the old clone-then-apply-ten result at the flip, so the compiled output did not change. Pins a full SHA, never a branch name: the ARG bump is the deliberate gate. Fork `develop` is the persistent integration branch (same policy as the quasar repo) — compositor work branches off it and merges back with no PR. Note the fork's `stable` also carries an HDR commit (`7d736ce`) that is deliberately NOT in `develop` |
| `GST_INTERPIPE_REF` | **`0c454917`** | The gow-fork pin (query forwarding across the interpipe boundary; stock RidgeRun v1.1.10 lacks it) |
| Vendored patch (gst-interpipe) | `deploy/patches/interpipe/interpipesink-add-listener-caps-leak.patch` | Quasar-authored (issue #418). `gst_inter_pipe_sink_add_listener()`'s `if (!sinkcaps) goto add_to_list;` shortcut jumps past the common `gst_caps_unref (srccaps)` tail, leaking one transfer-full `GstCaps` every time a listener attaches before the sink has negotiated caps. With our `interpipesrc` caps construct-property + `allow-renegotiation=false`, the leaked object is the appsrc's own private caps copy, so it survives teardown at ref-count 1 with no live `GstObject` holding it (invisible to a `filters=GstObject` leaks tracer). Applied to the `${GST_INTERPIPE_REF}` checkout before `meson setup`. Standalone repro on Tower: 8 leaks / 8 cycles unpatched, 0 / 8 patched |
| Vendored patch (gst-interpipe), **2nd** | `deploy/patches/interpipe/interpipesink-are-caps-compatible-intersect-leak.patch` | Quasar-authored (issue #428). `gst_inter_pipe_sink_are_caps_compatible()` computes `renegotiated_caps = gst_caps_intersect (listener_caps, sinkcaps)` and returns on **both** paths without unreffing it; `gst_caps_intersect()` is (transfer full), so every call leaks exactly one `GstCaps`. Restructured to a single exit: the compatible/incompatible decision and its log line both happen before the unref. Reached from `gst_inter_pipe_sink_add_listener()` only when the sink has already negotiated caps, other listeners are attached, and the listener's caps differ from the sink's — rare in Quasar's single-format topology, which is why it is low priority, but unbounded where it does fire and invisible to a `leaks(filters=GstObject)` census for the same reason as #418 (a `GstCaps` is a `GstMiniObject`, and after teardown no live `GstObject` holds it). The two interpipe patches touch different functions (~line 345 vs ~line 980) and are order-independent; the Dockerfile globs `deploy/patches/interpipe/*.patch`, so adding one needs no Dockerfile edit. Verified in the dev container: meson build clean, suite 15/15 green (`gst_test_caps_renegotiation` and `gst_test_invalid_caps` exercise this path) |
| `GST_PLUGINS_RS_REF` | **`cee45224cb2d10e1523af4f256d0dd64c8b29491`** (`0.15.3-cee4522`, 2026-07-03; on branch `0.15`) | Pairs with GStreamer 1.28 (0.13 pairs with 1.24 — used by the other two Dockerfiles; 0.14 pairs with 1.26). Do not mix a 0.13/0.14 gst-plugins-rs checkout into this image; it will not build against 1.28 headers. **Pinned to an exact SHA** (was a bare `--branch 0.15` clone) after the 2026-07-05 reproducibility triage — `0.15` is a *moving* branch, so a `--depth 1` clone baked a build-time-dependent HEAD (an un-pinned input). Verified as the SHA in the known-good image via `gst-inspect-1.0 rtpgccbwe`. Flip by editing the ARG default + this row together |
| `CARGO_C_VERSION` | **`0.10.23`** (`+cargo-0.97.1`, installed `--locked`) | `cargo-c` provides `cargo cinstall` (used for both the gst-plugins-rs `rtp` plugin and `gst-wayland-display`). **Pinned** (was an un-versioned `cargo install cargo-c`) in the same 2026-07-05 triage. `--version` freezes the tool, `--locked` freezes its transitive deps. 0.10.23 is the version validated against `RUST_VERSION=1.94.0` and baked into the known-good image (hermes build logs) |
| Vendored patches (GStreamer) | `deploy/patches/vulkan/vkh264enc-dpb-pool-in-new-sequence.patch`, `deploy/patches/vulkan/vulkanh265enc.patch`, `deploy/patches/vulkan/vkh264enc-rc-fix.patch`, `deploy/patches/vulkan/vkh264enc-num-slices.patch`, `deploy/patches/vulkan/gstreamer-vulkan-global-submit-lock.patch`, `deploy/patches/vulkan/vkh264enc-intra-refresh.patch` | The DPB-pool patch is fetched **verbatim** from `games-on-whales/gst-wayland-display@43d4c25`'s `patches/` directory (PR #37). `vulkanh265enc.patch` **started** verbatim from that same PR #37 but is now **Quasar-modified** (2026-07-24 NVIDIA bitstream-conformance fix in `vkh265enc.c` — resolution spec `docs/design/plans/2026-07-24-vulkanh265enc-conformance-resolution-spec.md`, Option A1: honor the driver-returned SPS so the bitstream CTB grid matches what the driver encodes) — regenerated against the DPB baseline, **not** re-fetchable verbatim without dropping the fix. The last four are Quasar-authored (rc-fix = live-bitrate writability, num-slices = multi-slice for lossy-WiFi resilience, global-submit-lock = Tower Xid A/B diagnostic, intra-refresh = periodic intra refresh for #227). Applied **in that order** to the GStreamer 1.28.4 checkout (intra-refresh applies last). See `deploy/patches/vulkan/README.md` for what each does — the DPB-pool patch is **mandatory** for `vulkanh264enc` to survive the `interpipesrc` swap boundary; intra-refresh needs Vulkan headers ≥ 1.4.321 + a driver exposing `VK_KHR_video_encode_intra_refresh` |
| Live-resize output-state patch (GStreamer), **9th, applies last** | `deploy/patches/vulkan/vulkan-enc-output-state-on-resize.patch` | Quasar-authored (#501), applied **after all eight** other GStreamer patches. `vulkanh264enc` and `vulkanh265enc` early-returned from `new_sequence()` on an unchanged video profile, which a pure resolution change always is, so a live external-resolution step (the `vulkanscale` lever) left the src caps, the coded extent and the DPB pool at the launch size. h264 published stale caps width/height with a correctly re-derived level, and since `gsth264parse.c` lets upstream caps override the SPS, downstream caps lied; h265 lost the opposite half (stale SPS `pic_width_in_luma_samples`). The fix keys each early return on the input size too, so a size change restarts the Vulkan session and re-runs the first-start path (limit check, coded extent, DPB pool, and `gst_video_encoder_set_output_state()` via `new_parameters()`). `vulkanav1enc` already did this and is not touched. Proven live on devbox for h264 and av1 (1080 to 720 and back, caps follow, 270 frames, no bus error); **h265 is compile-proven only**: it does not start on driver 610.57.04 (`Video profile format not supported`, pre-existing). See `deploy/patches/vulkan/README.md` |
| Bitstream-pool patch (GStreamer), **10th, applies last** | `deploy/patches/vulkan/vkenc-bitstream-buffer-pool.patch` | Quasar-authored (#507), applied **after all nine** other GStreamer patches. `gst_vulkan_encoder_picture_init()` allocated a fresh host-visible Vulkan bitstream buffer (`vkCreateBuffer` + `vkAllocateMemory` + `vkBindBufferMemory`, at least 1 MiB) for every frame and freed it again in `gst_vulkan_encoder_picture_clear()`, both on the streaming thread, so the cost could not overlap the encode engine. Measured on the devbox RTX 5090 at 1440p: 0.70 ms to allocate plus 0.53 ms to free per HEVC frame, which was the entire non-encode per-frame cost (command recording, submit and query fetch together measure under 0.05 ms). The fix is a minimal private `GstBufferPool` subclass. Four properties of it are load-bearing: the pooled buffer never leaves the encoder (the encoded bytes are copied into a plain buffer, because `GstBaseParse` downstream keeps the `GstMemory` of buffers it received long after the encoder has cleared the picture - pushing the pooled buffer downstream was a reproducible `SIGSEGV` on the first live resize); a recycled buffer is zeroed on return, because `_write_headers()` leaves gaps that fresh Vulkan memory happens to zero and stale bytes there corrupt the next IDR (an H.264 capture decoded 240 of 300 frames without it), bounded to the prefix the previous frame actually wrote plus a 64 KiB floor so the clearing does not cost more than the allocation did; a buffer that cannot be cleared is dropped rather than reused; and the pool is configured for the size the buffers really are, since `default_reset_buffer()` resizes recycled buffers to the configured size and the allocation helper floors at 1 MiB. The pool also re-chains the `pNext` of its `GstVulkanVideoProfile` copy, because it is allowed to outlive the encoder whose profile it copied. Codec-agnostic (it is in the shared encoder library): 1440p rose from 110 to 131 fps for HEVC, 373 to 620 fps for H.264 and 325 to 657 fps for AV1, and 2160p HEVC from 49 to 58 fps, with 300-frame captures byte-identical to the unpatched library for all three codecs, 300-of-300 strict decodes, and the fork's live-resize GPU suite passing 5 of 5. An oversize frame now errors loudly instead of being silently truncated as it was before. The residual HEVC cost is driver-side engine time, not submission overhead - see `deploy/patches/vulkan/README.md` for the evidence and for the knobs that were ruled out |
| Compositor patch (gst-wayland-display) | `deploy/patches/vulkan/gst-wayland-display-vulkan-pts.patch` | Quasar-authored; applied to the `gst-wayland-display@43d4c25` checkout (not GStreamer), after checkout and before `cargo cinstall`. Fixes the `vulkan=true` output path stamping recycled ring-slot buffers with stale PTS (~4 recurring non-monotonic values → libwebrtc can't assemble → ~60% decode-stage frame drop). Root cause + A/B validation in `docs/reports/VULKAN-WORKLOG.md` (2026-07-06); to be reported on PR #37 |
| NVIDIA Vulkan synchronization patch (gst-wayland-display) | `deploy/patches/vulkan/gst-wayland-display-vulkan-nvidia-sync.patch` | Quasar-authored against `43d4c25` after the PTS + cadence patches. Uses GStreamer's queue submission lock, validates graphics+compute capability, and replaces guessed Rust ABI offsets with a header-compiled bridge while preserving PR #37's layout/fan-out behavior. Queue locking and node-agent Vulkan-device continuity are Tower stabilization hypotheses pending live A/B validation; GStreamer 1.28.4 already manages multi-family image sharing. |
| Linear encode-src fallback patch (gst-wayland-display) | `deploy/patches/vulkan/gst-wayland-display-linear-encsrc-fallback.patch` | Quasar-authored against `43d4c25` with the other six `gst-wayland-display-*` patches applied first (nvidia-sync touches the same `alloc_encode_src_buffer`). Implements the tiled (`OPTIMAL`) retry that `WOLF_VULKAN_LINEAR_ENCSRC`'s comment promises but the code lacked: a rejected LINEAR encode-src alloc (NVIDIA Vulkan-Video, verified RTX 5090 2026-07-24) hard-failed the session's Vulkan output instead of falling back. To be reported on PR #37 |
| Per-element Vulkan device patch (gst-wayland-display) — **8th, applies last** | `deploy/patches/vulkan/gst-wayland-display-per-element-vulkan-device.patch` | Quasar-authored against `43d4c25`, applied **after all seven** other `gst-wayland-display-*` patches (it touches `vulkan_share.rs`, which `nvidia-sync` **and** `linear-encsrc-fallback` also touch). Moves the owned `GstVulkanInstance`+`GstVulkanDevice` out of `vulkan_share.rs`'s two process-global `OnceLock` slots onto per-`waylanddisplaysrc`-element storage (an `Arc<VulkanShare>` threaded to the compositor thread like `app_surface_commits`/`renderer_degraded`), so N concurrent Vulkan-encode sessions each own an isolated `VkDevice` (per-session failure domain; no device leak per session). Storage-location change only — device creation is byte-identical, no vendor branch, **RADV unchanged**; `repr(C)` overlays + C bridge untouched. Multi-session Vulkan spec §2a (`docs/design/plans/2026-07-25-vulkan-multisession-spec.md`); offer as contribution #6 on PR #37 |
| G1 buffer-reuse gate patch (gst-wayland-display) — **9th, applies last** | `deploy/patches/vulkan/gst-wayland-display-vulkan-parent-buffer-gate.patch` | Quasar-authored against `43d4c25` (re-authored from `origin/fix/vulkan-ring-gate`'s G1), applied **after all eight** other `gst-wayland-display-*` patches (it touches `vulkan_nv12.rs`'s `to_gst_buffer`, which the `vulkan-pts` patch also touches). `VulkanNv12::to_gst_buffer` hands the encode path a **child** `GstBuffer` header carrying `GstParentBufferMeta` over the cached encode-src ring slot (not a bare clone that BaseSrc's copy-on-write can silently free while the encoder still reads the memory), and `convert()` drops-and-re-emits the last frame rather than reuse a still-referenced slot — making the auto-pinned ring depth (`WOLF_VULKAN_RING=2`, double-buffered, auto-pinned for HEVC — `RING=1`'s single slot starves under this same gate) safe and closing a candidate green-bars buffer-reuse artifact. `encode_src`-only; VA/RGBx/DMABuf byte-identical, **RADV unchanged**. Excludes the ring-gate branch's `exit(75)` teardown (that is spec §2b). Multi-session Vulkan spec §2c; smoothness A/B (present-σ) on Tower is the acceptance gate |
| CUDA refcount-fix patch (gst-wayland-display) — **10th, applies last** | `deploy/patches/vulkan/gst-wayland-display-cuda-pool-config-leak.patch` | Quasar-authored against `43d4c25`, applied **after all nine** other `gst-wayland-display-*` patches (three of them also touch `waylandsrc/imp.rs`). Fixes **two** refcount errors in the CUDA `decide_allocation` path, which must land together: (1) `CUDABufferPool::get_updated_size()` never freed the **(transfer full)** `GstStructure` from `gst_buffer_pool_get_config()`, and that copy re-refs the config's `cuda-stream` field → a `GstCudaStream` → `gst_object_ref()` on its `GstCudaContext`, so **one context reference leaked per session**, pinning the app-injected ZC-02 context forever (~500 MiB VRAM + one `cuda-EvtHandlr` driver thread per session); (2) the query's **borrowed** caps was adopted with `Caps::from_glib_full`, leaving it one ref short — harmless only while (1) held a compensating ref, so fixing (1) alone retains every session's pipelines. The leak was invisible to `leaks(filters=GstObject)` (a `GstStructure` is not refcounted, `GstCudaStream` is a `GstMiniObject`) and unreachable from any teardown hook — hence the node-agent singleton (`cuda_share.rs`, develop `3185a4f`) could only **bound** it; instrumented builds also **disproved** the earlier "missing CUDA teardown analogue" theory (`CUDAContext::drop` and `GsCUDABuf::drop` both run). Validated on Tower: 3 sequential nvenc sessions → 0 alive `GstCudaStream`, context ref-count flat at 1, VRAM flat. **CUDA-only** (`--features cuda` ⇒ the `nv` target): VA/Vulkan/software images do not compile either site, **RADV unchanged**. Upstream finding #8 on PR #37 |

### Executable downloads: version pins are not integrity pins

Two build inputs are fetched as **executables** rather than compiled from a pinned source ref, so
a version alone says nothing about the bytes. Both are checked against a sha256 in
`deploy/pins.env` before they are run or unpacked.

| Pin | Value | Why the digest, not just the version |
|---|---|---|
| `RUSTUP_VERSION` + `RUSTUP_INIT_SHA256_X86_64` / `_AARCH64` | `1.29.0` | The build used to `curl https://sh.rustup.rs \| sh`. That URL serves an **unversioned** script, so there was nothing to pin and whatever the origin returned was piped into a shell in the toolchain stage. It now fetches `static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/<triple>/rustup-init`, which is immutable per version, and `sha256sum -c`s it first. Both digests were confirmed against the vendor's own published `rustup-init.sha256`. |
| `DOCKER_VERSION` + `DOCKER_CLI_SHA256_X86_64` / `_AARCH64` | `27.5.1` | This static CLI is what the agent runs **against the mounted host docker socket**, so a substituted tarball is host-level code execution. Docker publishes no checksum beside these tarballs; the digests were recorded from the fetched bytes, which freezes them — a later change to a released version's tarball becomes a build failure instead of a silent swap. |

Bumping either version means re-recording **both** architectures' digests in the same commit;
`deploy/build-images.sh`'s `check_pins_agree` only checks that `pins.env` and the Dockerfile ARG
defaults agree, never that a digest is correct.

The **base image** (`QUASAR_BASE_IMAGE`) is still a mutable tag on the daily path. `deploy/toolchain-hash.sh`
now hashes the base's *identity*: a `@sha256:` reference as-is, otherwise `QUASAR_BASE_DIGEST` when
the caller resolved one (the GHA `toolchain` job does), otherwise the tag string with a loud warning —
because a rebuilt base under an unchanged tag would otherwise leave the toolchain hash unchanged and
silently reuse a 40-minute artefact built on a different base. Release builds still override the ARG
with an exact digest, which `scripts/release/release-preflight.sh` requires.

### NVIDIA driver installer digests (`REVIEWED_DRIVER_DIGESTS`)

The node-agent's driver-volume provisioner downloads NVIDIA's `.run` and executes it
(`sh <file> --extract-only`) inside a container with host networking, `NET_ADMIN`, devices and the
docker socket. NVIDIA publishes no signature or digest for these files, so the reviewed table in
`node-agent/src/nvidia_volume.rs` is the only pin that covers a **first** provision, and it is empty
until someone stages an entry. Adding one:

1. Fetch `https://download.nvidia.com/XFree86/Linux-x86_64/<version>/NVIDIA-Linux-x86_64-<version>.run`.
2. `sha256sum` it, and cross-check the value against an independent record of the same file (another
   host that already provisioned that version, or a distribution's own recorded hash).
3. Add `("<version>", "<sha256>")` to `REVIEWED_DRIVER_DIGESTS` and ship a new agent.

With no entry the provisioner accepts the fetched payload on first use and pins it per host, logging
WARN `drvvol-trust-on-first-use` — NVIDIA publishes no digest to review against, so refusing by
default would break first provision on the CUDA-only hosts this feature exists to rescue. A reviewed
entry upgrades that host-local trust to a check that covers the *first* provision everywhere, and
`QUASAR_NVIDIA_DRIVER_TRUST_ON_FIRST_USE=0` refuses unvouched payloads outright. Once an entry exists
it is authoritative: a mismatch is refused and neither operator hatch overrides it. See
`docs/configuration.md` for the knobs.

### How to flip these pins

**Compositor (gst-wayland-display) changes — the 2026-07-26 model.** The build source is the fork's
`develop` branch, so:

1. `git clone git@github.com:salty2011/gst-wayland-display.git` (or use the existing clone; `origin`
   is the fork, `upstream` is games-on-whales).
2. Branch off `develop`, make the change as a real commit, merge back into `develop`, push.
3. Bump `GST_WAYLAND_DISPLAY_REF` in `deploy/Dockerfile.vulkan` to the new `develop` SHA and update
   the row above. Nothing is rebuilt until that bump, which is the gate.
4. Re-diff the corresponding file in `deploy/patches/vulkan/` so the authored record and the branch
   stay in step, and update that README section.

`deploy/patches/vulkan/gst-wayland-display-*.patch` are **no longer applied at build time**. They
are kept as the authored record and as the source for upstream submissions (each maps 1:1 to a
commit on `develop`). The GStreamer patches in that directory (`vkh264enc-*`, `vulkanh265enc`,
`gstreamer-vulkan-global-submit-lock`) ARE still applied at build time — only the
`gst-wayland-display-*` ones moved into the branch.

**Moving to a newer upstream** is now a rebase instead of re-diffing ten patch files by hand:
`git fetch upstream && git rebase --onto upstream/feat/vulkan-encode 43d4c25 develop`, resolve as
real commits, push, bump the ARG.

**Upstream submissions** are cherry-picks off `develop` onto a branch based on the relevant upstream
branch — see `docs/reports/2026-07-26-gwd-upstream-submission-drafts.md` for the nine PRs currently
open (#42–#50) and which commits they correspond to.

### Fork-bump verification policy

**A `GST_WAYLAND_DISPLAY_REF` or `GST_INTERPIPE_REF` bump — or a new vendored patch against
either — does not merge on a green build.** Both live under the encoder in the compositor and
swap path, where the defects that matter are refcount and lifetime bugs: they do not fail a
build, they do not fail a unit test, and they surface as a leak, a black frame or a teardown
CRITICAL several sessions later. So the gate is a live exercise, not a compile. Folded in from
#271 and #266; first exercised for the 2026-08-25 #423/#487/#428 batch.

**Which pins this governs.** The same live-exercise gate applies to any of the following moving,
not only the two named above — they all feed the same `/opt/gst` build and the same compositor/
encode process, so the failure mode (a refcount or lifetime bug invisible to a build or a unit
test) is identical regardless of which pin moved:

- `GST_WAYLAND_DISPLAY_REF` (the compositor fork) and `GST_INTERPIPE_REF` (the launcher<->game
  swap boundary) — the two pins this section was first written for.
- `GST_VERSION` (GStreamer itself) — a minor bump changes the code every vendored patch in
  `deploy/patches/vulkan/` applies against; a patch that still applies cleanly
  (`git apply --check`) is not evidence it still does the right thing at runtime.
- `QUASAR_BASE_IMAGE` in `deploy/pins.env` (the Fedora base from `quasar-images`) — a base bump
  can change glibc, Mesa, or the Vulkan loader under every element above it without touching a
  single line in this repo.

A `GST_PLUGINS_RS_REF` bump (the `rtp` plugin / `rtpgccbwe`) is lower risk in the same family —
it is exercised continuously by ABR, so a live exercise here is still the right instinct but not
gated with the same weight as the four above; use judgement rather than skipping it silently.

**What "a live exercise" means, concretely.** All four items below, run on the **`gpu-test`
host** (the one host with an NVIDIA GPU — see CLAUDE.md; a claim of "verified" without a run
on real GPU hardware does not count), with the evidence pasted into the merge commit or the
issue:

1. **Image rebuild on the `gpu-test` host via `deploy/build-images.sh`**, contract validation passing. Never a
   hand-typed `docker build`, and never a relaxed assertion in `deploy/image-contract.json`. This
   step exists because a hand-typed build can silently take the wrong `--target` or skip a
   `deploy/pins.env` cross-check that catches a doc/Dockerfile pin drift before it ships.
2. **One bench run submitted to `quasar-bench`** (`make bench-run HOST=devbox`) with verdict
   `nominal` and `regressed=0`. This exists because it is the only step in this list that catches
   a per-frame cost regression — nothing else here asserts on latency or smoothness at all.
3. **VRAM steady-state across at least two consecutive sessions** — `nvidia-smi --query-gpu=
   memory.used --format=csv` (or the admin host-observability panel) read after each teardown,
   with no growth beyond noise. This exists because three of the defects behind these pins (#418,
   #428, the CUDA pool-config leak) were refcount leaks whose ONLY visible symptom was this number
   drifting session over session — nothing else in this list would have caught them.
4. **A fallback-NVENC smoke: `make nvenc-fallback-smoke HOST=devbox ARGS='--app <name>'`**
   (`scripts/harness/run-nvenc-fallback-smoke.sh`, wrapped by `scripts/dx/nvenc_fallback_smoke.sh`). It
   requires `QUASAR_VULKAN_H264=0` already set on the running agent container (set it in
   `deploy/.env`, then `docker compose up -d --force-recreate quasar-node-agent` — the harness
   checks this precondition and fails loudly with the fix if it isn't met, it does not flip the
   knob itself) and then asserts, from the agent's own `probe-encoder` report AND its structured
   "codec fallback" log line for the live session (not by inference): the session's effective
   encoder really is `nvcudah264enc`; a real Chrome-for-Testing peer decodes it (monotonic frame
   growth, zero freezes); and teardown is clean (confirmed terminal state, the agent container
   still running afterward with an unchanged restart count, no SIGSEGV/`libnvcuvid`/GLib-CRITICAL
   in the agent log around teardown). This exists because the compositor is shared by both encode
   paths, so a compositor bump that is clean on Vulkan (the default, and the only path items 1-3
   exercise) can still be broken on the vendor path that `resolve_effective_encoder` falls back
   to — and because Vulkan being the default is exactly why that path can rot silently: nothing
   else routinely exercises it. Revert `QUASAR_VULKAN_H264` after the run (`deploy/.env` /
   restart again) to return the host to its normal Vulkan-default state.

**What does NOT count as proof.** None of the following satisfy this policy, however green:
a passing `cargo build` or `cargo clippy` inside `quasar-agent-dev`; a passing `cargo test` /
`go test ./...` run (none of the defects this policy exists for — #418, #428, #423, the CUDA
pool-config leak — ever failed a unit test); an image that merely **builds** without the
contract validation in step 1 also passing; `git apply --check` succeeding for the vendored
patches (that proves the patch still applies, not that it still does the right thing against the
new code underneath it); or a bench run against the WRONG image — tag every bench run with the
pin actually read off the deployed image (`docker inspect quasar-nv:latest --format
'{{index .Config.Labels "org.quasar.pins.gwd"}}'`), never off the git checkout, for the reason in
the "Tag a bench run" note below.

Watch the agent log for `GLib-*-CRITICAL` across every session in the run — that is the #423
acceptance condition observed live, and it is cheap to check while the sessions are happening.

**Rollback if the exercise fails.** Revert the `GST_WAYLAND_DISPLAY_REF` / `GST_INTERPIPE_REF` /
`GST_VERSION` / `QUASAR_BASE_IMAGE` ARG (and the matching `deploy/pins.env` row) to the last pin
that passed this same four-item exercise, not merely to "whatever was in `develop` before" — a
prior pin that was never itself live-exercised is not a known-good rollback target. Re-run
`deploy/build-images.sh` on devbox at the reverted pin and re-validate the contract before
redeploying; do not redeploy an un-rebuilt image on the assumption that reverting the pin file
alone is sufficient. If the failing bump already merged to `develop` and a host is running it,
redeploy the reverted pin's image explicitly (`deploy/redeploy.sh`) rather than waiting for the
next `develop` deploy to fix it — nothing else on the deploy path re-checks this policy.

**Judge the bench on a paired A/B, not on one run against the pinned baseline.** The first
exercise of this policy (2026-08-25, the `310c03e` bump) made the reason concrete. A single
scored cell came back `REGRESSED` on headline g2g at +15.6%, which reads as a damning result
until you run the control: the *old* images, redeployed on the same box the same evening, flagged
`regression` too. The nightly cell's own history over the preceding week was 64, 70, 79.7, 67.3 ms
— a spread wider than any effect worth gating on — so one absolute number carries almost no
information. `B1 frame assembly` in particular has been flagging since at least 2026-08-23 on
unmodified images and is a stale-baseline artifact, not a live defect. The paired result was
old `880e9b3` 76.95 / 85.75 / 70.7 ms against new `310c03e` 81.1 / 84.5 ms: overlapping ranges,
no supportable regression. Run at least two cells per side, back to back, redeploying between
them, and compare side means — and re-baseline when the pinned baseline is this far from what
the box now does.

**Tag a bench run with the pin read off the deployed image, never off the git checkout.** In that
same session a run submitted as `git_quasar=753e4cc9` was in fact measuring the previous build,
because another agent had advanced the checkout while the containers still ran the earlier images.
`docker inspect quasar-nv:latest --format '{{index .Config.Labels "org.quasar.pins.gwd"}}'` is the
authoritative answer to "what is actually running".


Same procedure as above: edit the `ARG` defaults in `deploy/Dockerfile.vulkan` and this table
together. If `GST_WAYLAND_DISPLAY_REF` moves, re-fetch **only** the DPB-pool patch verbatim
from the new commit's `patches/` directory (do not hand-edit a diff to "port" it forward);
`vulkanh265enc.patch` is now Quasar-modified (see the vendored-patches note above) — re-diff it
against the new DPB baseline and re-apply the 2026-07-24 conformance fix rather than re-fetching it
verbatim (which would silently drop the fix). Re-diff the Quasar-authored
`gst-wayland-display-vulkan-pts.patch` against the new compositor checkout (confirm it still
applies — `git apply --check`), and update `deploy/patches/vulkan/README.md`'s provenance tables too. If `GST_VERSION` moves to a later
GStreamer release, re-verify the ten GStreamer patches still apply cleanly (`git apply --check`),
in the order `deploy/Dockerfile.vulkan` applies them, before building. A patch written against 1.28.4's `vkh264enc.c` is not guaranteed to apply
unmodified to a later minor.


## Image consolidation onto the gst-1.28 lineage — all images now build `43d4c25` (#367, 2026-07-07)

Re-pinning the retired 1.24-based Ubuntu images to `43d4c25` in place was
**structurally impossible**: at that commit gst-wayland-display depends on **gstreamer-rs
0.25**, whose `gstreamer-sys` build script requires GStreamer **>= 1.28** headers — the
Ubuntu-24.04 dev/nv images build against apt GStreamer **1.24** (live failure: `error:
failed to run custom build command for gstreamer-sys v0.25.2`).

So pin convergence is achieved not by bumping the 1.24 images, but by **consolidating every
image onto the gst-1.28 `Dockerfile.vulkan` lineage** — where `GST_WAYLAND_DISPLAY_REF`
is already `43d4c25`. `Dockerfile.vulkan` is now a **multi-target** file with one source of
truth for the pins, patches, and meson enable-set:

| `--target` | Image tag | Built FROM | Role |
|---|---|---|---|
| `runtime` | `quasar-node-agent:latest` | `quasar-base` | vendor-neutral node-agent (AMD/Intel VA + Vulkan) |
| `dev` | `quasar-agent-dev:latest` | `build` | build/test env + session app image; **never deployed** |
| `nv` | `quasar-nv:latest` | **`runtime`** | `runtime` + the NVIDIA CUDA runtime libraries (~275 MB) |

**Restructured 2026-07-26** (spec: `docs/design/plans/2026-07-26-image-lineage-consolidation-spec.md`):

- **`CUDA_ENABLE` now defaults to `1` and is no longer a per-target choice.** The `build` stage
  is always CUDA-instantiated, so ONE `/opt/gst` and ONE agent binary serve both vendors. This
  is safe on non-NVIDIA hosts: without `libcuda.so.1` the nvcodec plugin loads, warns once and
  registers **zero** features, and the `--features cuda` agent binary carries no
  `libcuda`/`libgstcuda` `DT_NEEDED` (they are dlopened) so it starts clean on a driver-less
  host. `CUDA_ENABLE=0` is a diagnostic-only escape hatch; `deploy/validate-image.sh` rejects
  the resulting image for the `runtime`/`nv` roles.
- **`nv` is now `FROM runtime`, not a sibling `FROM quasar-base`.** It previously carried a
  hand-copied duplicate of `runtime`'s package list, the two drifted, and the shipped nv image
  lost `pulseaudio`, `ddcutil`, `mesa-va-drivers`, `vulkan-tools` and both freeworld packages —
  muting every Tower session for days. Never re-introduce a parallel package list; a
  non-NVIDIA-specific package belongs in `runtime`.
- **The node-agent binary is built `--features cuda` in the `build` stage.** It previously was
  not, which forced the Tower deployment to compile the agent inside the container and is why
  6.15 GB of toolchain was baked into a production image.
- `GST_VERSION` is now a real ARG (was a hardcoded `--branch 1.28.4`).

`nv`'s base stays **Fedora 43** (not `nvidia/cuda:base-ubuntu`) — it inherits it from `runtime`,
which avoids a cross-distro glibc/libstdc++ ABI mismatch on the `/opt/gst` COPY. The driver is
injected by the nvidia container runtime at run time; only the `NVIDIA_DRIVER_CAPABILITIES` env
conventions and the CUDA runtime libs are set in the `nv` stage.

Build with **`deploy/build-images.sh`**, not a hand-typed `docker build` — it always passes an
explicit `--target` (a bare build takes the LAST stage regardless of `-t`, which mis-tagged a
2026-07-12 Tower deploy), rejects a `--build-arg` for an ARG this file does not declare, and
contract-validates the result before promoting `:latest`:

```bash
deploy/build-images.sh runtime nv        # the two deployable images, validated
deploy/build-images.sh all               # + dev + control
deploy/build-images.sh nv --gwd-ref <sha>   # test a compositor re-pin
```

The three consolidation ARGs (`CUDA_ENABLE`, `CUDA_PKG_VERSION`) join the existing pin ARGs
(`GST_WAYLAND_DISPLAY_REF`, `GST_INTERPIPE_REF`, `GST_PLUGINS_RS_REF`, `CARGO_C_VERSION`,
`RUST_VERSION`, `DOCKER_VERSION`) already documented in the `Dockerfile.vulkan` pins table above.

### Legacy cutover — DONE (2026-07-17)

`Dockerfile.dev` / `Dockerfile.nv` are **deleted**. The operator tags are unchanged —
`quasar-agent-dev:latest` and `quasar-nv:latest` are now built FROM `Dockerfile.vulkan`
(`--target dev` / `--target nv`) by `scripts/dev/dev.sh image` and `deploy/build-images.sh`,
so compose defaults, skills, seed scripts, and existing app-catalog `runtime_spec.image`
rows all kept working without changes. The `quasar-dev128`/`quasar-nv128` transitional
names are retired.

The Quasar-authored `gst-wayland-display-vulkan-pts.patch` applies to the `43d4c25` checkout,
i.e. every image on this lineage (`runtime`/`dev`/`nv`) — no longer vulkan-only.
