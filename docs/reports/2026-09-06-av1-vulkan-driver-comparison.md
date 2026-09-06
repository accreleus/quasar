# Vulkan AV1 corruption: live comparison

## Finding

The reported corruption reproduces on the production Unraid host with NVIDIA
**595.99.02 open**, while the GPU-test VM with **610.57.04** produces clean output.
These are sequential tests on the **same physical RTX 5090**, handed between host
and VM by the operator's existing VM automation.

The deployed Vulkan plugin binaries are byte-identical. A reduced test reproduces
inter-frame corruption without Steam, the compositor, Quasar signaling, WebRTC,
a browser, or network transport. Forcing every frame to be a keyframe eliminates
the corruption in that test.

This establishes a driver/runtime-dependent failure in the Vulkan AV1 encode
path. It does **not** yet establish whether NVIDIA's driver or our use of its API
needs correction. The OS and driver-library delivery also differ; this was not
a driver-only swap on one OS. No speculative encoder patch or global codec
disable was deployed.

## Provenance

Both agents report GStreamer 1.28.4 and compositor fork pin
`310c03eca4b90b653885def4e3348034067c8c72`. Both copies of
`/opt/gst/lib64/gstreamer-1.0/libgstvulkan.so` have SHA-256:

```text
70c4b75ba6eda166b2144880807ab6f9edfd88c9cedf2b77670172e5a7e97a10
```

The AV1 implementation is Quasar's
[`vulkanav1enc.patch`](../../deploy/patches/vulkan/vulkanav1enc.patch), applied to
GStreamer by `deploy/Dockerfile.vulkan`. The compositor fork is built separately;
switching that repository does not select a different AV1 encoder. Agent source
revisions differ: production `2ced17e28d1f`, GPU-test
`dedb1ba6fea086cde6f0008006678480a658a2d7`. The binary comparison and standalone
test remove that uncertainty from the encoder-plugin comparison.

The supplied `~/code/quasar_development/gst-wayland-display` path was absent.
The existing `/home/hermes/code/gst-wayland-display` has the expected `salty2011`
origin and `games-on-whales` upstream, but was at `64451ee`, not the deployed pin.
It contains no copy of Quasar's AV1 patch and was inspected without modification.

## Live browser results

The same local headless Chrome executable drove each deployment's SPA, requesting
AV1 at 2560×1440, 120 fps, 10,000 kbps, and capturing native video pixels every
15 seconds. Each test stopped its own session. Agent logs confirm `vulkanav1enc`,
CBR, quality 0, one reference frame and keyframe period 120 on both hosts,
including the post-PLAYING rate-control rearm.

| Environment | Session | Result |
|---|---|---|
| GPU-test VM, 610.57.04 | `659b5ee0-d328-4036-b5c1-79a7e2282447` | Clean Steam text and QR; 6,595 decoded frames |
| Production, 595.99.02 | `948445cb-245f-42a7-bde3-09e61561c6eb` | Same severe text/QR corruption as operator; 5,570 decoded frames |
| Production, H.264 control | `2268601e-407b-4e2b-ab9f-2f71fefcee36` | Clean text and QR; effective width 1920, 7,078 decoded frames |

H.264 negotiated 1920 pixels despite the requested profile, so this is not an
exact resolution-matched control. The operator independently confirmed clean
H.264 and HEVC streams. Our headless HEVC attempt never obtained a nonzero video
width and is **not** counted as successful validation: the throwaway helper's
frame-count-only PASS label was insufficient. Login screenshots remain private
rather than publishing QR codes.

## Minimal reproduction

Inside an existing GPU-enabled agent container with working driver discovery:

```sh
gst-launch-1.0 -e \
  videotestsrc num-buffers=120 pattern=checkers-8 ! \
  video/x-raw,format=NV12,width=1280,height=720,framerate=60/1 ! \
  vulkanupload ! \
  vulkanav1enc num-ref-frames=1 quality=0 rate-control=cqp \
    qp-i=80 qp-p=80 idr-period=60 ! \
  av1parse ! filesink location=/run/quasar-agent/av1-cqp-probe.obu
```

The VM's ad-hoc `docker exec` additionally needed `__EGL_VENDOR_LIBRARY_DIRS`,
`__EGL_EXTERNAL_PLATFORM_CONFIG_DIRS`, `GBM_BACKENDS_PATH` and
`VK_ADD_DRIVER_FILES` pointed into its provisioned driver volume. Such execs do
not inherit the environment established internally by the agent. Omitting those
paths misleadingly reported a missing AV1 element; both the actual agent and the
correctly configured probe found it.

Copy the OBU file out and decode on the workstation. Compare every decoded frame
to the first decoded frame of that same stream, without a PNG/RGB round trip:

```sh
ffmpeg -v error -i probe.obu -frames:v 1 -pix_fmt yuv420p \
  -f rawvideo -y first.yuv
ffmpeg -v error -i probe.obu \
  -f rawvideo -pixel_format yuv420p -video_size 1280x720 -i first.yuv \
  -filter_complex 'psnr=stats_file=psnr.txt' -f null -
```

The input is static. This measures temporal stability, not source PSNR or general
perceptual quality. The reduced probe uses CQP, establishing that the failure is
not confined to the live CBR configuration.

| Test | Frame 2 PSNR | Frame 3 PSNR | Frame 120 PSNR | Result |
|---|---:|---:|---:|---|
| 595.99.02, period 60 | 50.28 dB | 25.12 dB | 18.30 dB | Strong colour corruption |
| 610.57.04, period 60 | 50.28 dB | 49.94 dB | 49.91 dB | Stable checkerboard |
| 595.99.02, **period 1 only change** | Infinite | Infinite | Infinite | All 120 decoded frames identical |

A further 595 test with quality 2 and three reference frames still corrupted.
That changes two properties together and does not isolate either one. Both
drivers advertised AV1 capability flags `0xd` and syntax flags `0x7`. Emitted
inter-frame headers request `primary_ref_frame=0`: CDF inheritance and reconstructed
reference state are useful next probes, not proven causes.

Sanitized synthetic evidence:

- [First frame](2026-09-06-av1-evidence/first-frame.png)
- [595 frame 120](2026-09-06-av1-evidence/595-frame-120.png)
- [610 frame 120](2026-09-06-av1-evidence/610-frame-120.png)
- [595 metrics](2026-09-06-av1-evidence/595-psnr.txt)
- [610 metrics](2026-09-06-av1-evidence/610-psnr.txt)
- [595 all-keyframe metrics](2026-09-06-av1-evidence/595-allkey-psnr.txt)

## Driver-update validation limits

The attempted Unraid 610 package installed a proprietary kernel module that could
not initialize Blackwell. The operator restored 595 open successfully. For kernel
`6.18.47-Unraid`, the inspected
[Unraid package release](https://github.com/unraid/unraid-nvidia-driver/releases/tag/6.18.47-Unraid)
offered an open 595.99.02 package, but no open 610.57.04 package. A higher version
is not a valid remediation unless the required module variant exists for the
host kernel.

After automatic GPU return and recreation, production reported all 16 readiness
checks passing or skipped, native `libnvidia-eglcore.so.595.99.02`, writable
render/input devices, and consistent sibling-container paths. It reused NVRTC
13.2.86. Its graphics-driver-volume check was **skipped because native host
graphics packages are used**. This validates restart/reuse and current library
injection; it does **not** validate replacing a stale provisioned graphics volume
with a different driver version. The VM separately adopted and injected its
existing matching 610.57.04 volume into Steam.

## Recommended next changes

1. Add a synthetic multi-frame encode/decode correctness gate to encoder
   certification. A successful probe, high decoded FPS or readiness pass cannot
   detect this failure today. Scope results to GPU, driver and encoder build;
   invalidate them when those change.
2. Use working H.264/HEVC on the affected deployment until a corrected combination
   passes. Do not silently substitute NVENC AV1, which has a separately recorded
   teardown failure. All-keyframe AV1 is a diagnostic control, not a streaming fix.
3. Test CDF inheritance and DPB reconstruction separately in an isolated candidate
   build using this reproduction, then repeat synthetic and Steam tests on both
   environments. Do not infer a universal minimum driver from two samples.
4. Validate a controlled stale-volume-to-new-version transition separately. Make
   module variant and actual library version visible in readiness remediation;
   distinguish native-driver hosts from provisioned-volume hosts.

NVIDIA's [Vulkan driver notes](https://developer.nvidia.com/vulkan-driver) record
a Blackwell AV1 scrambled-output fix in the separate 595.44.02 Vulkan beta branch.
That is context, not proof this is the same bug or that 595.99.02 includes its fix.
The [Vulkan AV1 picture API](https://docs.vulkan.org/refpages/latest/refpages/source/VkVideoEncodeAV1PictureInfoKHR.html)
defines CDF and reference-slot behavior; subsequent probes must respect those
requirements rather than edit emitted headers.
