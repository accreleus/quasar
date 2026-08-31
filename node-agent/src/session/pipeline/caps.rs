//! Caps + format helpers for the session media pipeline: the caps that cross the
//! interpipe boundary and feed the encoders, the H.264 profile resolution, and the
//! DMABuf/NV12 zero-copy `drm-format` negotiation. Element-graph construction stays
//! in [`super`].

use anyhow::{anyhow, Result};
use gstreamer as gst;

use crate::messages::VideoTopology;
use crate::session::{Codec, EncoderChoice, SessionConfig};

/// The `profile` caps-field the encoder-output capsfilter advertises, or `None` when
/// the codec carries no profile field.
/// - h264 → [`h264_caps_profile`] (illegal profile strings are a hard error).
/// - h265 → always `main`; `h264_profile` is ignored. This pin is load-bearing:
///   `vulkanh265enc` also advertises `main-444`, which its own `H265ProfileMap`
///   cannot map, and negotiating it fails in the driver at `new_sequence`.
/// - av1 → `None` (seq profile 0 is implicit; `av1parse` negotiates stream-format).
///
/// Vendor×codec matrix: one of four sites edited together when a vendor or codec is
/// added, cross-referenced from `encoders::encoder_candidates` and pinned by
/// `encoders::matrix_tests`, which calls this directly (hence `pub(crate)`).
pub(crate) fn caps_profile(
    codec: Codec,
    requested: &str,
    encoder: EncoderChoice,
) -> Result<Option<&'static str>> {
    match codec {
        Codec::H264 => h264_caps_profile(requested, encoder).map(Some),
        Codec::H265 => Ok(Some("main")),
        Codec::Av1 => Ok(None),
    }
}

/// VulkanImage is an encoder transport contract, not a local-display format: the
/// producer's images are VIDEO_ENCODE_SRC layout and its ring sync is designed for
/// the Vulkan encoder. Local-only has no encoder, and forcing those images through
/// vulkandownload races consumer reads against slot reuse on NVIDIA, eventually
/// losing the device.
pub(crate) fn vulkan_image_transport(cfg: &SessionConfig) -> bool {
    uses_vulkan_images(cfg.encoder, cfg.video_topology)
}

/// Encoder-free local display uses ordinary DRM PRIME buffers: unlike the Vulkan
/// encode ring, DMABuf sync is designed for cross-pipeline display import, and
/// waylandsink advertises DMA_DRM directly. Knob: `QUASAR_EXPERIMENTAL_LOCAL_DMABUF`.
pub(crate) fn local_dmabuf_transport(cfg: &SessionConfig) -> bool {
    cfg.video_topology == VideoTopology::LocalOnly
        && !cfg.use_test_src
        && matches!(
            std::env::var("QUASAR_EXPERIMENTAL_LOCAL_DMABUF")
                .ok()
                .as_deref(),
            Some("1") | Some("true") | Some("TRUE")
        )
}

fn uses_vulkan_images(encoder: EncoderChoice, topology: VideoTopology) -> bool {
    encoder == EncoderChoice::Vulkan && topology != VideoTopology::LocalOnly
}

/// The H.264 `profile` caps-field for `requested` on `encoder` — the EFFECTIVE
/// profile, never the merely-requested one. The capsfilter between the encoder and
/// `h264parse` carries it and webrtcbin copies it into the SDP offer's
/// `profile-level-id`; if it disagreed with the real bitstream, Chrome's receiver
/// answers `m=video 0` and no frames ever arrive.
///
/// VA/NVENC/Vulkan pass all three through (they expose no `profile` property and are
/// driven by downstream caps). openh264enc is capped at `constrained-baseline`: it
/// will negotiate `main`/`high` caps but is a baseline-class encoder, so downgrade
/// rather than ship a mislabelled stream, and let the caller warn.
///
/// An unrecognised profile string is an error, not a silent coercion: the
/// `QUASAR_H264_PROFILE` env path wants a hard check.
pub(crate) fn h264_caps_profile(requested: &str, encoder: EncoderChoice) -> Result<&'static str> {
    let canonical = match requested {
        "constrained-baseline" => "constrained-baseline",
        "main" => "main",
        "high" => "high",
        other => {
            return Err(anyhow!(
                "unknown H.264 profile '{other}' (legal: constrained-baseline, main, high)"
            ))
        }
    };
    Ok(match encoder {
        EncoderChoice::Openh264 => "constrained-baseline",
        // These genuinely produce each profile. Browser sessions are clamped to
        // constrained-baseline UPSTREAM of this (H.264 High does not decode in Chrome
        // on any encoder — `.claude/rules/webrtc-testing.md`); this only reports what
        // the encoder can emit.
        EncoderChoice::Va | EncoderChoice::Nvenc | EncoderChoice::Vulkan => canonical,
    })
}

/// The raw-video caps crossing the interpipe boundary, at the session's WxH@fps.
/// Both the source's output caps and the encode-side interpipesrc caps are pinned to
/// this, so the encoder sees byte-identical caps across a swap and never
/// renegotiates. The caps must be FULLY FIXED — the encode-side `interpipesrc` runs
/// `allow-renegotiation=false`, and an unfixed width trips a downstream
/// display-ratio assertion so negotiation never completes.
///
/// ZC-01: encoder-aware per path; see [`raw_video_caps_for`].
pub(super) fn raw_video_caps(cfg: &SessionConfig) -> gst::Caps {
    if local_dmabuf_transport(cfg) {
        return gst::Caps::builder("video/x-raw")
            .features(["memory:DMABuf"])
            .field("format", "DMA_DRM")
            .field("width", cfg.stream.width)
            .field("height", cfg.stream.height)
            .field("framerate", gst::Fraction::new(cfg.stream.fps, 1))
            .build();
    }
    if cfg.encoder == EncoderChoice::Vulkan && !vulkan_image_transport(cfg) {
        return gst::Caps::builder("video/x-raw")
            .field("format", "RGBx")
            .field("width", cfg.stream.width)
            .field("height", cfg.stream.height)
            .field("framerate", gst::Fraction::new(cfg.stream.fps, 1))
            .build();
    }
    let dmabuf_drm_format = dmabuf_zerocopy_format(cfg);
    raw_video_caps_for(
        cfg.encoder,
        cfg.stream.width,
        cfg.stream.height,
        cfg.stream.fps,
        dmabuf_drm_format.as_deref(),
    )
}

/// ZC-03: the DMABuf `drm-format` (`fourcc:modifier`) the compositor should emit so
/// `vapostproc` can import it zero-copy; `None` falls the VA path back to ZC-01
/// system RGBx. Deterministic (same element caps), so [`raw_video_caps`] and
/// [`super::build_video_source`] agree.
pub(super) fn dmabuf_zerocopy_format(cfg: &SessionConfig) -> Option<String> {
    if cfg.dmabuf_zerocopy() {
        vapostproc_dmabuf_drm_format()
    } else {
        None
    }
}

/// Query `vapostproc`'s sink pad for an importable `memory:DMABuf` `drm-format`,
/// preferring an 8-bit RGB one with an explicit modifier (e.g.
/// `AR24:0x0200000000401a01`). This modifier negotiation is what makes
/// compositor→encode zero-copy work on VA: the compositor advertises the same render
/// modifiers, so it renders straight into an importable layout with no intermediate
/// convert/upload. `None` (⇒ ZC-01 fallback) if `vapostproc` is absent or advertises
/// nothing usable.
fn vapostproc_dmabuf_drm_format() -> Option<String> {
    let factory = gst::ElementFactory::find("vapostproc")?;
    // 8-bit RGB fourccs the compositor renders AND vapostproc imports: ARGB/ABGR first
    // (the compositor's GLES output order), then the alpha-less X variants.
    const PREFER: [&str; 8] = [
        "AR24", "AB24", "BA24", "RA24", "XR24", "XB24", "RX24", "BX24",
    ];
    let mut found: Vec<String> = Vec::new();
    for tmpl in factory.static_pad_templates() {
        if tmpl.direction() != gst::PadDirection::Sink {
            continue;
        }
        let caps = tmpl.caps();
        for i in 0..caps.size() {
            let Some(feats) = caps.features(i) else {
                continue;
            };
            if !feats.contains("memory:DMABuf") {
                continue;
            }
            let Some(s) = caps.structure(i) else { continue };
            if let Ok(list) = s.get::<gst::List>("drm-format") {
                for v in list.iter() {
                    if let Ok(f) = v.get::<String>() {
                        found.push(f);
                    }
                }
            } else if let Ok(f) = s.get::<String>("drm-format") {
                found.push(f);
            }
        }
    }
    // Pick the first preferred fourcc that carries an explicit modifier (`fourcc:0x…`).
    for p in PREFER {
        if let Some(f) = found.iter().find(|f| f.starts_with(p) && f.contains(':')) {
            tracing::info!("ZC-03: vapostproc-importable DMABuf drm-format chosen: {f}");
            return Some(f.clone());
        }
    }
    tracing::warn!(
        token = "dmabuf-no-importable-format",
        "ZC-03: vapostproc advertises no importable RGB DMABuf drm-format (found {found:?}) — \
         falling back to the ZC-01 system path"
    );
    None
}

/// [`raw_video_caps`] factored over its inputs so the per-encoder format is testable
/// without a [`SessionConfig`]. VA/NVENC-N-B/software pin a fully-fixed system format
/// at WxH; ZC-03 pins `memory:DMABuf` DMA_DRM at the negotiated modifier.
///
/// Vendor×codec matrix: one of four sites edited together when a vendor or codec is
/// added (cross-reference in `encoders::encoder_candidates`). Keyed on
/// [`EncoderChoice`] ONLY — the raw interpipe format does not vary by codec — so
/// `matrix_tests` exercises it per pair to catch a future codec-dependent branch.
/// `pub(super)` because that test calls it directly.
pub(super) fn raw_video_caps_for(
    encoder: EncoderChoice,
    width: i32,
    height: i32,
    fps: i32,
    dmabuf_drm_format: Option<&str>,
) -> gst::Caps {
    let framerate = gst::Fraction::new(fps, 1);
    // ZC-03 full zero-copy VA: the compositor emits memory:DMABuf at WxH in a
    // vapostproc-importable modifier and vapostproc converts it to NV12 VAMemory with
    // no system hop. DMABuf survives the interpipe as PRIME fds, needing no shared GPU
    // context (unlike VAMemory).
    if let Some(drm_format) = dmabuf_drm_format {
        return gst::Caps::builder("video/x-raw")
            .features(["memory:DMABuf"])
            .field("format", "DMA_DRM")
            .field("drm-format", drm_format)
            .field("width", width)
            .field("height", height)
            .field("framerate", framerate)
            .build();
    }
    // VA and NVENC N-B: system-memory RGBx across the interpipe, with the GPU convert
    // co-located in the encode pipeline (vapostproc, or cudaupload→cudaconvert).
    let system_rgbx = || {
        gst::Caps::builder("video/x-raw")
            .field("format", "RGBx")
            .field("width", width)
            .field("height", height)
            .field("framerate", framerate)
            .build()
    };
    // Arms are split rather than `Va | Nvenc` so the cuda Nvenc arm doesn't shadow the
    // fallback: no unreachable pattern under either feature.
    match encoder {
        // NVENC N-A, full zero-copy: the compositor emits memory:CUDAMemory BGRA across
        // the interpipe and the encode-pipeline cudaconvert makes CUDAMemory NV12. Valid
        // across a swap because both pipelines share one app-owned GstCudaContext
        // (session::cuda_share); capturing the compositor's context off the bus does not
        // work, it never posts one.
        #[cfg(feature = "cuda")]
        EncoderChoice::Nvenc => gst::Caps::builder("video/x-raw")
            .features(["memory:CUDAMemory"])
            .field("format", "BGRA")
            .field("width", width)
            .field("height", height)
            .field("framerate", framerate)
            .build(),
        #[cfg(not(feature = "cuda"))]
        EncoderChoice::Nvenc => system_rgbx(),
        EncoderChoice::Va => system_rgbx(),
        // VK-05: the compositor emits memory:VulkanImage NV12 directly
        // (waylanddisplaysrc's `vulkan` property) and the encoder ingests it with no
        // convert. The framerate field is load-bearing: waylanddisplaysrc produces 1 fps
        // unless downstream caps ask for a rate.
        EncoderChoice::Vulkan => gst::Caps::builder("video/x-raw")
            .features(["memory:VulkanImage"])
            .field("format", "NV12")
            .field("width", width)
            .field("height", height)
            .field("framerate", framerate)
            .build(),
        // System-memory I420: the software encoder input.
        EncoderChoice::Openh264 => gst::Caps::builder("video/x-raw")
            .field("format", "I420")
            .field("width", width)
            .field("height", height)
            .field("framerate", framerate)
            .build(),
    }
}

/// The VA encoder's zero-copy input caps. The encode-pipeline `vapostproc` produces
/// them from the interpipe's system RGBx and the VA encoder ingests the surface
/// zero-copy; both elements are in the same pipeline, so the VADisplay propagates by
/// normal in-pipeline GstContext and needs no cross-pipeline sharing.
pub(super) fn va_encoder_input_caps(width: i32, height: i32, fps: i32) -> gst::Caps {
    gst::Caps::builder("video/x-raw")
        .features(["memory:VAMemory"])
        .field("format", "NV12")
        .field("width", width)
        .field("height", height)
        .field("framerate", gst::Fraction::new(fps, 1))
        .build()
}

/// The NVENC encoder's zero-copy input caps (ZC-02). The encode-pipeline
/// `cudaconvert` produces them from the interpipe's CUDAMemory BGRA and the encoder
/// ingests the surface zero-copy; both share the cross-interpipe GstCudaContext.
pub(super) fn cuda_encoder_input_caps(width: i32, height: i32, fps: i32) -> gst::Caps {
    gst::Caps::builder("video/x-raw")
        .features(["memory:CUDAMemory"])
        .field("format", "NV12")
        .field("width", width)
        .field("height", height)
        .field("framerate", gst::Fraction::new(fps, 1))
        .build()
}

/// The caps the ENCODER INPUT must carry for `choice` at an arbitrary external stream
/// size and `fps` — the caps half of the adaptive-external-resolution lever
/// (`docs/superpowers/specs/2026-08-16-adaptive-external-resolution-design.md` D7).
/// The scale stage's capsfilter is re-set to this at runtime to change the encoded
/// frame size without touching the interpipe boundary, the source tail caps, or
/// `webrtcbin`, so the one-offer invariant holds.
///
/// Not `raw_video_caps_for`: that describes the interpipe boundary, pinned at the
/// launch size forever. This is the encoder's input, downstream of the scale stage,
/// and the only size in the graph that moves.
pub(super) fn encoder_input_caps_for(
    choice: EncoderChoice,
    width: i32,
    height: i32,
    fps: i32,
) -> gst::Caps {
    match choice {
        EncoderChoice::Va => va_encoder_input_caps(width, height, fps),
        EncoderChoice::Nvenc => cuda_encoder_input_caps(width, height, fps),
        EncoderChoice::Vulkan => gst::Caps::builder("video/x-raw")
            .features(["memory:VulkanImage"])
            .field("format", "NV12")
            .field("width", width)
            .field("height", height)
            .field("framerate", gst::Fraction::new(fps, 1))
            .build(),
        EncoderChoice::Openh264 => gst::Caps::builder("video/x-raw")
            .field("format", "I420")
            .field("width", width)
            .field("height", height)
            .field("framerate", gst::Fraction::new(fps, 1))
            .build(),
    }
}

#[cfg(test)]
mod tests {
    use super::{
        caps_profile, cuda_encoder_input_caps, encoder_input_caps_for, h264_caps_profile,
        raw_video_caps, raw_video_caps_for, uses_vulkan_images, va_encoder_input_caps,
    };
    use crate::messages::VideoTopology;
    use crate::session::{Codec, EncoderChoice, SessionConfig, StreamParams};
    use gstreamer as gst;

    // ---- caps_profile: the all-codec generalization of h264_caps_profile ----

    #[test]
    fn caps_profile_h264_matches_h264_caps_profile() {
        assert_eq!(
            caps_profile(Codec::H264, "main", EncoderChoice::Va).unwrap(),
            Some("main")
        );
        assert_eq!(
            caps_profile(Codec::H264, "high", EncoderChoice::Va).unwrap(),
            Some("high")
        );
        // openh264 clamps every profile to constrained-baseline.
        assert_eq!(
            caps_profile(Codec::H264, "high", EncoderChoice::Openh264).unwrap(),
            Some("constrained-baseline")
        );
        assert!(caps_profile(Codec::H264, "ultra", EncoderChoice::Va).is_err());
    }

    // h265 is always Main regardless of the requested profile or the vendor, and never
    // errors on the (h264-only) profile string.
    #[test]
    fn caps_profile_h265_is_always_main() {
        for enc in [
            EncoderChoice::Va,
            EncoderChoice::Nvenc,
            EncoderChoice::Vulkan,
        ] {
            assert_eq!(
                caps_profile(Codec::H265, "constrained-baseline", enc).unwrap(),
                Some("main")
            );
            assert_eq!(
                caps_profile(Codec::H265, "ignored-junk", enc).unwrap(),
                Some("main")
            );
        }
    }

    #[test]
    fn caps_profile_av1_is_none() {
        for enc in [EncoderChoice::Va, EncoderChoice::Nvenc] {
            assert_eq!(caps_profile(Codec::Av1, "main", enc).unwrap(), None);
            assert_eq!(caps_profile(Codec::Av1, "whatever", enc).unwrap(), None);
        }
    }

    #[test]
    fn vulkan_images_are_encoder_transport_not_local_only_transport() {
        assert!(uses_vulkan_images(
            EncoderChoice::Vulkan,
            VideoTopology::StreamOnly
        ));
        assert!(uses_vulkan_images(
            EncoderChoice::Vulkan,
            VideoTopology::DualOutput
        ));
        assert!(!uses_vulkan_images(
            EncoderChoice::Vulkan,
            VideoTopology::LocalOnly
        ));
        assert!(!uses_vulkan_images(
            EncoderChoice::Va,
            VideoTopology::StreamOnly
        ));
    }

    // ZC-02 N-B: the interpipe stays SYSTEM memory (cudaupload+cudaconvert are
    // co-located in the encode pipeline), so no cross-pipeline CUDA context sharing.
    #[cfg(not(feature = "cuda"))]
    #[test]
    fn raw_video_caps_nvenc_is_system_rgbx_at_wxh() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(EncoderChoice::Nvenc, 1920, 1080, 60, None);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.name(), "video/x-raw");
        assert_eq!(s.get::<String>("format").unwrap(), "RGBx");
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        assert_eq!(s.get::<i32>("height").unwrap(), 1080);
        if let Some(feats) = caps.features(0) {
            assert!(!feats.contains("memory:CUDAMemory"));
        }
    }

    // ZC-02 N-A: full zero-copy — the shared GstCudaContext makes the compositor's
    // CUDAMemory BGRA surface valid across the interpipe.
    #[cfg(feature = "cuda")]
    #[test]
    fn raw_video_caps_nvenc_is_cudamemory_bgra_at_wxh() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(EncoderChoice::Nvenc, 1920, 1080, 60, None);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.name(), "video/x-raw");
        assert_eq!(s.get::<String>("format").unwrap(), "BGRA");
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        let feats = caps.features(0).expect("caps features");
        assert!(
            feats.contains("memory:CUDAMemory"),
            "N-A interpipe must carry memory:CUDAMemory, got {caps}"
        );
    }

    #[test]
    fn cuda_encoder_input_caps_is_cudamemory_nv12() {
        gstreamer::init().unwrap();
        let caps = cuda_encoder_input_caps(1280, 720, 60);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.get::<String>("format").unwrap(), "NV12");
        assert_eq!(s.get::<i32>("width").unwrap(), 1280);
        let feats = caps.features(0).expect("caps features");
        assert!(feats.contains("memory:CUDAMemory"));
    }

    // VK-05: memory:VulkanImage NV12 at a fully-fixed WxH@fps. The framerate must be
    // present — waylanddisplaysrc produces 1 fps if nothing downstream asks.
    #[test]
    fn raw_video_caps_vulkan_is_vulkanimage_nv12_at_wxh() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(EncoderChoice::Vulkan, 1920, 1080, 60, None);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.name(), "video/x-raw");
        assert_eq!(s.get::<String>("format").unwrap(), "NV12");
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        assert_eq!(s.get::<i32>("height").unwrap(), 1080);
        let feats = caps.features(0).expect("caps features");
        assert!(
            feats.contains("memory:VulkanImage"),
            "Vulkan interpipe must carry memory:VulkanImage, got {caps}"
        );
    }

    // CM-08: `raw_video_caps` must carry memory:VulkanImage NV12 for a DualOutput vulkan
    // session (so the console leg's bridge picks `vulkandownload`) and must DEMOTE a
    // LocalOnly one to system RGBx (no encoder ring). Driven through the wrapper, not
    // `raw_video_caps_for`, so it pins the demotion decision a regression would flip.
    #[test]
    fn cm08_vulkan_dualoutput_is_vulkanimage_localonly_is_system_rgbx() {
        gstreamer::init().unwrap();

        let dual = raw_video_caps(&vulkan_cfg(VideoTopology::DualOutput));
        assert!(
            dual.features(0)
                .is_some_and(|f| f.contains("memory:VulkanImage")),
            "DualOutput vulkan must carry memory:VulkanImage (drives vulkandownload), got {dual}"
        );
        assert_eq!(
            dual.structure(0).unwrap().get::<String>("format").unwrap(),
            "NV12"
        );

        let local = raw_video_caps(&vulkan_cfg(VideoTopology::LocalOnly));
        assert_eq!(
            local.structure(0).unwrap().get::<String>("format").unwrap(),
            "RGBx",
            "LocalOnly vulkan must demote to system RGBx, got {local}"
        );
        assert!(
            local
                .features(0)
                .is_none_or(|f| !f.contains("memory:VulkanImage")),
            "LocalOnly vulkan must be system memory, not a VulkanImage, got {local}"
        );
    }

    /// A real `SessionConfig` for the Vulkan encoder at `topology`.
    /// `use_test_src`/experimental-dmabuf envs stay unset, so the plain paths run.
    fn vulkan_cfg(topology: VideoTopology) -> SessionConfig {
        let mut settings = crate::session::settings::RuntimeSettings::baseline();
        settings.encoder = EncoderChoice::Vulkan;
        let stream = StreamParams {
            width: 1920,
            height: 1080,
            fps: 60,
            bitrate_kbps: 8000,
            h264_profile: "constrained-baseline".to_string(),
            codec: crate::session::Codec::H264,
            abr_floor_kbps: 0,
            mic: false,
        };
        let mut cfg = SessionConfig::for_assignment_with(&settings, stream, None);
        cfg.video_topology = topology;
        cfg
    }

    // `vulkanh264enc`'s src pad template advertises all three profiles.
    #[test]
    fn vulkan_passes_all_three_profiles_through() {
        assert_eq!(
            h264_caps_profile("constrained-baseline", EncoderChoice::Vulkan).unwrap(),
            "constrained-baseline"
        );
        assert_eq!(
            h264_caps_profile("main", EncoderChoice::Vulkan).unwrap(),
            "main"
        );
        assert_eq!(
            h264_caps_profile("high", EncoderChoice::Vulkan).unwrap(),
            "high"
        );
    }

    // ZC-01: the VA path carries system RGBx at a fully-fixed WxH (the vapostproc
    // convert lives in the encode pipeline). The size MUST be pinned — the encode
    // interpipesrc runs `allow-renegotiation=false`.
    #[test]
    fn raw_video_caps_va_is_system_rgbx_at_wxh() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(EncoderChoice::Va, 1920, 1080, 60, None);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.name(), "video/x-raw");
        assert_eq!(s.get::<String>("format").unwrap(), "RGBx");
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        assert_eq!(s.get::<i32>("height").unwrap(), 1080);
        if let Some(feats) = caps.features(0) {
            assert!(!feats.contains("memory:VAMemory"));
        }
    }

    // ZC-03: with a negotiated importable drm-format the caps carry memory:DMABuf
    // (DMA_DRM + that modifier), so there is no system hop.
    #[test]
    fn raw_video_caps_va_zerocopy_is_dmabuf_drm_at_wxh() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(
            EncoderChoice::Va,
            1920,
            1080,
            60,
            Some("AR24:0x0200000000401a01"),
        );
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.name(), "video/x-raw");
        assert_eq!(s.get::<String>("format").unwrap(), "DMA_DRM");
        assert_eq!(
            s.get::<String>("drm-format").unwrap(),
            "AR24:0x0200000000401a01"
        );
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        let feats = caps.features(0).expect("caps features");
        assert!(
            feats.contains("memory:DMABuf"),
            "ZC-03 interpipe must carry memory:DMABuf, got {caps}"
        );
    }

    #[test]
    fn va_encoder_input_caps_is_vamemory_nv12() {
        gstreamer::init().unwrap();
        let caps = va_encoder_input_caps(1920, 1080, 60);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.get::<String>("format").unwrap(), "NV12");
        assert_eq!(s.get::<i32>("width").unwrap(), 1920);
        assert_eq!(s.get::<i32>("height").unwrap(), 1080);
        let feats = caps.features(0).expect("caps features");
        assert!(
            feats.contains("memory:VAMemory"),
            "encoder input must carry the memory:VAMemory feature, got {caps}"
        );
    }

    #[test]
    fn raw_video_caps_openh264_is_system_i420() {
        gstreamer::init().unwrap();
        let caps = raw_video_caps_for(EncoderChoice::Openh264, 1280, 720, 60, None);
        let s = caps.structure(0).expect("a structure");
        assert_eq!(s.get::<String>("format").unwrap(), "I420");
        if let Some(feats) = caps.features(0) {
            assert!(!feats.contains("memory:VAMemory"));
        }
    }

    // The AMD VCN encoder genuinely produces each profile (verified against h264parse).
    #[test]
    fn va_passes_all_three_profiles_through() {
        assert_eq!(
            h264_caps_profile("constrained-baseline", EncoderChoice::Va).unwrap(),
            "constrained-baseline"
        );
        assert_eq!(
            h264_caps_profile("main", EncoderChoice::Va).unwrap(),
            "main"
        );
        assert_eq!(
            h264_caps_profile("high", EncoderChoice::Va).unwrap(),
            "high"
        );
    }

    // Every legal profile resolves to constrained-baseline, so the caps always match
    // openh264enc's safe output.
    #[test]
    fn openh264_caps_at_constrained_baseline() {
        assert_eq!(
            h264_caps_profile("constrained-baseline", EncoderChoice::Openh264).unwrap(),
            "constrained-baseline"
        );
        assert_eq!(
            h264_caps_profile("main", EncoderChoice::Openh264).unwrap(),
            "constrained-baseline"
        );
        assert_eq!(
            h264_caps_profile("high", EncoderChoice::Openh264).unwrap(),
            "constrained-baseline"
        );
    }

    // The caller warns on a downgrade, and detects it by comparing result to request.
    #[test]
    fn downgrade_is_detectable() {
        assert_ne!(
            h264_caps_profile("main", EncoderChoice::Openh264).unwrap(),
            "main"
        );
        assert_eq!(
            h264_caps_profile("main", EncoderChoice::Va).unwrap(),
            "main"
        );
    }

    #[test]
    fn unknown_profile_is_error() {
        assert!(h264_caps_profile("ultra", EncoderChoice::Va).is_err());
        assert!(h264_caps_profile("Constrained-Baseline", EncoderChoice::Va).is_err()); // case-sensitive
        assert!(h264_caps_profile("", EncoderChoice::Openh264).is_err());
        assert!(h264_caps_profile("baseline", EncoderChoice::Va).is_err()); // not a contract value
    }

    // ---- encoder_input_caps_for: the adaptive-external-resolution caps lever ----

    /// `format`, `width`, `height`, fps numerator and the memory feature.
    fn parts(caps: &gst::Caps) -> (String, i32, i32, i32, String) {
        let s = caps.structure(0).expect("caps have a structure");
        let fps = s.get::<gst::Fraction>("framerate").expect("framerate");
        (
            s.get::<String>("format").expect("format"),
            s.get::<i32>("width").expect("width"),
            s.get::<i32>("height").expect("height"),
            fps.numer(),
            caps.features(0).map(|f| f.to_string()).unwrap_or_default(),
        )
    }

    // Each arm carries its own memory feature + pixel format and honours the requested
    // WxH — the whole point of the lever.
    #[test]
    fn encoder_input_caps_carry_the_right_memory_feature_per_encoder() {
        let (fmt, w, h, fps, feat) =
            parts(&encoder_input_caps_for(EncoderChoice::Va, 1280, 720, 60));
        assert_eq!((fmt.as_str(), w, h, fps), ("NV12", 1280, 720, 60));
        assert!(feat.contains("memory:VAMemory"), "VA feature, got {feat}");

        let (fmt, w, h, _, feat) =
            parts(&encoder_input_caps_for(EncoderChoice::Nvenc, 1600, 900, 60));
        assert_eq!((fmt.as_str(), w, h), ("NV12", 1600, 900));
        assert!(
            feat.contains("memory:CUDAMemory"),
            "CUDA feature, got {feat}"
        );

        let (fmt, w, h, _, feat) = parts(&encoder_input_caps_for(
            EncoderChoice::Vulkan,
            2560,
            1440,
            60,
        ));
        assert_eq!((fmt.as_str(), w, h), ("NV12", 2560, 1440));
        assert!(
            feat.contains("memory:VulkanImage"),
            "Vulkan feature, got {feat}"
        );

        let (fmt, w, h, fps, feat) = parts(&encoder_input_caps_for(
            EncoderChoice::Openh264,
            640,
            360,
            30,
        ));
        assert_eq!((fmt.as_str(), w, h, fps), ("I420", 640, 360, 30));
        // No explicit feature ⇒ the implicit ANY/system one, never a GPU-memory feature.
        assert!(
            !feat.contains("VAMemory") && !feat.contains("CUDAMemory") && !feat.contains("Vulkan"),
            "software input is system memory, got {feat}"
        );
    }

    // At the launch size this must be byte-identical to the per-encoder helpers, so the
    // lever cannot regress the existing zero-copy caps.
    #[test]
    fn encoder_input_caps_match_the_legacy_helpers_at_launch_size() {
        assert_eq!(
            encoder_input_caps_for(EncoderChoice::Va, 1920, 1080, 60),
            va_encoder_input_caps(1920, 1080, 60)
        );
        assert_eq!(
            encoder_input_caps_for(EncoderChoice::Nvenc, 1920, 1080, 60),
            cuda_encoder_input_caps(1920, 1080, 60)
        );
        // Vulkan/openh264 match the interpipe caps: no convert on either path.
        assert_eq!(
            encoder_input_caps_for(EncoderChoice::Vulkan, 1920, 1080, 60),
            raw_video_caps_for(EncoderChoice::Vulkan, 1920, 1080, 60, None)
        );
        assert_eq!(
            encoder_input_caps_for(EncoderChoice::Openh264, 1920, 1080, 60),
            raw_video_caps_for(EncoderChoice::Openh264, 1920, 1080, 60, None)
        );
    }
}
