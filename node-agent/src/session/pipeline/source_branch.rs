//! The video source sub-chain and the GPU convert stage bridging the raw source caps to
//! each encoder's zero-copy input (ZC-01/ZC-02/ZC-03). Reads the negotiated formats from
//! `super::caps` and the VA element-name prefix from `super::encoders`.

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::virtual_input::VirtualDevices;
use crate::session::{EncoderChoice, SessionConfig};

use super::caps::{
    dmabuf_zerocopy_format, local_dmabuf_transport, raw_video_caps, vulkan_image_transport,
};
use super::encoders::va_device_element_prefix;

/// Self-heal the compositor's Vulkan encode-src ring-slot tiling defect by pinning the
/// ring to the depth [`super::encoders::required_source_ring_depth`] requires. WHICH codec
/// is at risk and why that depth is correct live in that function, not here.
///
/// The pin is HOST-WIDE, not per-session, and must stay that way: the compositor caches
/// the ring depth process-globally (`ring_used()` `OnceLock`, read at the first encode-src
/// allocation), so per-session pinning is unreliable. Once any session's knobs require a
/// pin it covers every Vulkan session for the process lifetime, H.264 and AV1 included;
/// RING=2 costs neither any smoothness. An explicit operator `WOLF_VULKAN_RING` still wins.
fn pin_vulkan_encode_ring() {
    // Ambient env read at compositor-element construction time, independent of the session
    // build's own `EncoderKnobs::from_env()`.
    let knobs = super::encoders::EncoderKnobs::from_env();
    let Some(depth) = super::encoders::required_source_ring_depth(knobs) else {
        return;
    };
    // An EMPTY value is unset, not an override: a compose passthrough
    // (`WOLF_VULKAN_RING: ${WOLF_VULKAN_RING:-}`) lands an empty string when the operator
    // exports nothing, and the compositor's `ring_used()` reads empty as "default".
    if std::env::var("WOLF_VULKAN_RING").is_ok_and(|v| !v.is_empty()) {
        return;
    }
    // MUST be set before the compositor element leaves NULL: its first encode-src
    // allocation, where `ring_used()` caches this, waits for the first frame. `set_var` is
    // process-global and not thread-safe against a concurrent read; the timing (and the
    // repo's pre-element env convention, e.g. GST_REGISTRY) keeps it race-free.
    std::env::set_var("WOLF_VULKAN_RING", depth.to_string());
    tracing::info!(
        "vulkan encode-src ring pinned to {depth} slots (WOLF_VULKAN_RING={depth}) for ALL \
         vulkan sessions on this host (the compositor caches the ring depth process-globally) \
         — see encoders::required_source_ring_depth for why this depth is required. Set \
         WOLF_VULKAN_RING explicitly to override."
    );
}

/// Build the video-source sub-chain into `pipeline` and return its TAIL (the capsfilter
/// pinned to [`raw_video_caps`]) for the caller to link onward — into the encoder, or into
/// the interpipesink on the split path.
///
/// Encoder-aware (ZC-01):
/// - VA: `<src> → caps[RGBx,fps]`. The source pipeline carries system-memory RGBx across
///   the interpipe; the GPU `vapostproc` convert + VA upload lives in the ENCODE pipeline
///   so it is paced by the encoder and shares its VADisplay, instead of racing 60 fps
///   ahead in the decoupled source pipeline and contending the display (the
///   throughput-stall fix).
/// - openh264: `<src> → videoscale → videoconvert → caps[I420,WxH]`.
///
/// With `devices`, `waylanddisplaysrc` is pointed at the virtual mouse + keyboard evdev
/// paths. The gamepad is excluded: libinput ignores joypads.
pub(super) fn build_video_source(
    pipeline: &gst::Pipeline,
    cfg: &SessionConfig,
    devices: Option<&VirtualDevices>,
) -> Result<gst::Element> {
    let src = if cfg.use_test_src {
        gst::ElementFactory::make("videotestsrc")
            .name("video-source")
            .property("is-live", true)
            .property_from_str("pattern", "ball")
            .build()
            .context("videotestsrc not found")?
    } else {
        let el = gst::ElementFactory::make("waylanddisplaysrc")
            .name("video-source")
            .property("render-node", cfg.render_node.as_str())
            .build()
            .context("waylanddisplaysrc not found — check GST_PLUGIN_PATH")?;
        // VK-05: the compositor emits memory:VulkanImage NV12 directly for vulkanh264enc.
        if vulkan_image_transport(cfg) {
            el.set_property("vulkan", true);
            tracing::info!(
                "VK-05: waylanddisplaysrc vulkan=true (emit memory:VulkanImage NV12 for vulkanh264enc)"
            );
            pin_vulkan_encode_ring();
        } else if local_dmabuf_transport(cfg) {
            el.set_property("nv12", true);
            tracing::info!(
                "local display: waylanddisplaysrc nv12=true (prefer display-importable NV12 DMABuf)"
            );
        }
        // libinput's path backend opens these directly (no udev/seat). The gamepad
        // reaches the app through the container's mounted device node instead.
        if let Some(d) = devices {
            el.set_property("mouse", d.mouse_path.to_string_lossy().as_ref());
            el.set_property("keyboard", d.keyboard_path.to_string_lossy().as_ref());
            tracing::info!(
                "compositor input devices: mouse={}, keyboard={}",
                d.mouse_path.display(),
                d.keyboard_path.display()
            );
        }
        el
    };

    // The caps that cross the interpipe boundary. The framerate field is load-bearing:
    // without it waylanddisplaysrc produces 1 fps.
    let tail_caps = raw_video_caps(cfg);
    let tail = gst::ElementFactory::make("capsfilter")
        .property("caps", &tail_caps)
        .build()
        .context("capsfilter not found")?;

    match cfg.encoder {
        // NVENC N-A: the compositor emits memory:CUDAMemory BGRA at the session WxH (it
        // honours the WxH from the tail caps), which crosses the interpipe zero-copy. No
        // videoscale — it cannot handle CUDA memory.
        #[cfg(feature = "cuda")]
        EncoderChoice::Nvenc => {
            pipeline.add_many([&src, &tail])?;
            gst::Element::link_many([&src, &tail])
                .context("failed to link NVENC N-A video source (compositor → CUDAMemory BGRA)")?;
        }
        // VA + NVENC N-B carry system RGBx at WxH across the interpipe; the GPU
        // RGBx→NV12 convert is co-located in the encode pipeline, so it is paced.
        // `videoscale` only pins the size (the compositor emits RGBx natively). The
        // interpipe caps MUST be fully fixed: the encode-side interpipesrc runs
        // `allow-renegotiation=false`. Split from the VA arm so the cuda `Nvenc` arm above
        // does not make this unreachable.
        #[cfg(not(feature = "cuda"))]
        EncoderChoice::Nvenc => {
            let scale = gst::ElementFactory::make("videoscale")
                .build()
                .context("videoscale not found")?;
            pipeline.add_many([&src, &scale, &tail])?;
            gst::Element::link_many([&src, &scale, &tail])
                .context("failed to link hardware video source (compositor → RGBx WxH)")?;
        }
        EncoderChoice::Va if dmabuf_zerocopy_format(cfg).is_some() => {
            // ZC-03 full zero-copy: memory:DMABuf RGB at WxH crosses the interpipe as PRIME
            // fds for the encode-pipeline vapostproc to import. No videoscale (it cannot
            // handle DMABuf).
            pipeline.add_many([&src, &tail])?;
            gst::Element::link_many([&src, &tail])
                .context("failed to link VA dmabuf source (compositor → DMABuf)")?;
        }
        EncoderChoice::Va => {
            let scale = gst::ElementFactory::make("videoscale")
                .build()
                .context("videoscale not found")?;
            pipeline.add_many([&src, &scale, &tail])?;
            gst::Element::link_many([&src, &scale, &tail])
                .context("failed to link hardware video source (compositor → RGBx WxH)")?;
        }
        // VK-05: memory:VulkanImage NV12 at the session WxH, like the other zero-copy
        // arms. No videoconvert/videoscale — neither can negotiate the VulkanImage memory
        // feature, so either would fail negotiation at runtime.
        EncoderChoice::Vulkan => {
            if vulkan_image_transport(cfg) {
                pipeline.add_many([&src, &tail])?;
                gst::Element::link_many([&src, &tail]).context(
                    "failed to link Vulkan video source (compositor → VulkanImage NV12)",
                )?;
            } else if local_dmabuf_transport(cfg) {
                pipeline.add_many([&src, &tail])?;
                gst::Element::link_many([&src, &tail])
                    .context("failed to link local-only DMABuf source (compositor → DMA_DRM)")?;
                tracing::info!(
                    "local-only topology: source emits DMABuf DMA_DRM for direct display import"
                );
            } else {
                let scale = gst::ElementFactory::make("videoscale")
                    .build()
                    .context("videoscale not found")?;
                pipeline.add_many([&src, &scale, &tail])?;
                gst::Element::link_many([&src, &scale, &tail])
                    .context("failed to link local-only Vulkan-host source (compositor → RGBx)")?;
                tracing::info!(
                    "local-only topology: Vulkan encode transport bypassed; source emits system RGBx"
                );
            }
        }
        EncoderChoice::Openh264 => {
            let scale = gst::ElementFactory::make("videoscale")
                .build()
                .context("videoscale not found")?;
            let convert = gst::ElementFactory::make("videoconvert")
                .build()
                .context("videoconvert not found")?;
            pipeline.add_many([&src, &scale, &convert, &tail])?;
            gst::Element::link_many([&src, &scale, &convert, &tail])
                .context("failed to link software video source")?;
        }
    }
    Ok(tail)
}

/// VA postproc candidates for `render_node`, most-specific first: the device-pinned
/// `va{renderDNNN}postproc`, so a multi-GPU host converts on the ASSIGNED card (the SO-07
/// pattern, mirroring [`super::encoders::va_device_element_prefix`]), then `vapostproc`.
fn va_postproc_candidates(render_node: &str) -> Vec<String> {
    let mut candidates = Vec::new();
    if let Some(prefix) = va_device_element_prefix(render_node) {
        candidates.push(format!("{prefix}postproc"));
    }
    candidates.push("vapostproc".to_string());
    candidates
}

/// Build the GPU `vapostproc` (ZC-01): RGBx→NV12 colour-convert + scale + VA upload
/// on the GPU, replacing the CPU `videoconvert`. Device-pinned to the assigned
/// render node when possible (see [`va_postproc_candidates`]).
pub(super) fn make_vapostproc(
    render_node: &str,
    va_ctx: Option<&gst::Context>,
) -> Result<gst::Element> {
    let candidates = va_postproc_candidates(render_node);
    let factory = candidates
        .iter()
        .find(|n| gst::ElementFactory::find(n).is_some())
        .ok_or_else(|| {
            anyhow!(
                "no VA postproc element found (tried {candidates:?}). Most likely \
                 the GPU is not in the container — pass `--device /dev/dri` to \
                 `docker run`; on AMD this also needs mesa-va-drivers in the image. \
                 Verify with `gst-inspect-1.0 vapostproc`."
            )
        })?;
    let pp = gst::ElementFactory::make(factory)
        .build()
        .with_context(|| format!("failed to create {factory}"))?;
    // GW-03 (#260): the shared VA display MUST be injected before any property read or
    // NULL→READY makes the element bind its own. See `make_va_encoder`.
    if let Some(ctx) = va_ctx {
        pp.set_context(ctx);
        tracing::info!("GW-03: injected shared VA display into {factory} before bind");
    }
    // Letterbox instead of stretch on an aspect mismatch. Guarded: properties vary across
    // VA postproc variants and an unknown one panics.
    if pp.find_property("add-borders").is_some() {
        pp.set_property("add-borders", true);
    }
    tracing::info!("VA postproc (GPU convert/scale/upload): {factory} (render node {render_node})");
    Ok(pp)
}

/// The GPU convert stage between the raw-video tail (system RGBx) and the encoder,
/// returned in link order (empty on the Vulkan path). Always co-located with the encoder,
/// so the convert is paced by it and shares its GPU context by ordinary in-pipeline
/// propagation; the interpipe stays system memory.
///
/// A thin view over [`super::scale_stage::build_scale_stage`], which also exposes the tail
/// capsfilter as the live resolution lever. Only the demo pipeline uses this element-list
/// view; the split encode pipeline keeps the whole `ScaleStage` so the runner can retarget.
pub(super) fn build_gpu_convert_stage(
    cfg: &SessionConfig,
    va_ctx: Option<&gst::Context>,
) -> Result<Vec<gst::Element>> {
    Ok(super::scale_stage::build_scale_stage(cfg, va_ctx)?.elements)
}

#[cfg(test)]
mod tests {
    use super::va_postproc_candidates;

    // SO-07: device-pinned to the assigned render node, generic as the single-GPU fallback.
    #[test]
    fn va_postproc_candidates_prefer_device_pinned() {
        assert_eq!(
            va_postproc_candidates("/dev/dri/renderD129"),
            vec!["varenderD129postproc".to_string(), "vapostproc".to_string()]
        );
        // Non-DRM render node (e.g. "software") → only the generic element.
        assert_eq!(
            va_postproc_candidates("software"),
            vec!["vapostproc".to_string()]
        );
    }
}
