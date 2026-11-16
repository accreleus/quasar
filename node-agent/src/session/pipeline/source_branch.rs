//! The video source sub-chain and the GPU convert stage bridging the raw source caps to
//! each encoder's zero-copy input (ZC-01/ZC-02/ZC-03). Reads the negotiated formats from
//! `super::caps` and the VA element-name prefix from `super::encoders`.

use anyhow::{anyhow, Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::container::ContainerSpec;
use crate::session::virtual_input::VirtualDevices;
use crate::session::{EncoderChoice, SessionConfig};

use super::caps::{dmabuf_zerocopy_format, source_caps, vulkan_image_transport};
use super::encoders::va_device_element_prefix;

/// The compositor's Vulkan encode-src ring depth (its `ring-depth` property) for this host:
/// an operator's `WOLF_VULKAN_RING` if set and non-empty, else what
/// [`super::encoders::required_source_ring_depth`] requires (the reason lives there).
/// `None` keeps the element's default. Empty is unset: compose passes it through as
/// `${WOLF_VULKAN_RING:-}`.
pub(crate) fn source_ring_depth() -> Option<u32> {
    match std::env::var("WOLF_VULKAN_RING") {
        Ok(v) if !v.is_empty() => match v.trim().parse::<u32>() {
            Ok(depth) => Some(depth),
            Err(_) => {
                tracing::warn!(
                    token = "vulkan-ring-depth-unparsed",
                    value = v,
                    "WOLF_VULKAN_RING is not a number; using the codec default"
                );
                super::encoders::required_source_ring_depth(
                    super::encoders::EncoderKnobs::from_env(),
                )
            }
        },
        _ => super::encoders::required_source_ring_depth(super::encoders::EncoderKnobs::from_env()),
    }
}

/// `depth` clamped into the range `pspec` accepts (the compositor takes 1..=4 and raises
/// anything below 2 to 2), so an operator value outside it cannot panic the setter.
fn clamp_ring_depth(depth: u32, pspec: &gst::glib::ParamSpec) -> u32 {
    match pspec.downcast_ref::<gst::glib::ParamSpecUInt>() {
        Some(range) => depth.clamp(range.minimum(), range.maximum()),
        None => depth,
    }
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
pub(crate) fn build_video_source(
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
            // LINEAR first, OPTIMAL where the driver refuses it (NVIDIA): radv corrupts the
            // tiled encode-src copy on RDNA2/RDNA4 (#281).
            if el.find_property("linear-encode-src").is_some() {
                el.set_property("linear-encode-src", true);
            }
            if let (Some(depth), Some(pspec)) =
                (source_ring_depth(), el.find_property("ring-depth"))
            {
                let depth = clamp_ring_depth(depth, &pspec);
                el.set_property("ring-depth", depth);
                tracing::info!(depth, "vulkan encode-src ring depth");
            }
        }
        // A swap builds its compositor from this same session cfg, not the new app's spec,
        // so the session keeps its keymap (amendment 49).
        if let Some((layout, variant)) = keyboard_layout(cfg.container.as_ref()) {
            // A compositor without both properties keeps US rather than failing the session.
            if keyboard_properties_present(|p| el.find_property(p).is_some()) {
                el.set_property("keyboard-layout", layout);
                el.set_property("keyboard-variant", variant);
                tracing::info!(layout, variant, "compositor keyboard layout");
            } else {
                tracing::warn!(
                    token = "compositor-keyboard-layout-unsupported",
                    layout,
                    variant,
                    "waylanddisplaysrc lacks keyboard-layout or keyboard-variant; the session keeps the US keymap"
                );
            }
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
    let tail_caps = source_caps(cfg);
    let tail = gst::ElementFactory::make("capsfilter")
        .name("source-caps")
        .property("caps", &tail_caps)
        .build()
        .context("capsfilter not found")?;
    // The encode pipeline cannot see this colorimetry in its own caps: it asks (#1104).
    if let Some(pad) = tail.static_pad("src") {
        super::pyrowave_branch::answer_colorimetry_queries(&pad);
    }

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
            pipeline.add_many([&src, &tail])?;
            gst::Element::link_many([&src, &tail])
                .context("failed to link Vulkan video source (compositor → VulkanImage NV12)")?;
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
pub(crate) fn build_gpu_convert_stage(
    cfg: &SessionConfig,
    va_ctx: Option<&gst::Context>,
) -> Result<Vec<gst::Element>> {
    Ok(super::scale_stage::build_scale_stage(cfg, va_ctx)?.elements)
}

/// Both keymap properties are there; a compositor with only one keeps US.
fn keyboard_properties_present(has: impl Fn(&str) -> bool) -> bool {
    has("keyboard-layout") && has("keyboard-variant")
}

/// The session's keyboard layout and variant from the app's env (amendment 49), or `None`
/// for the compositor's US keymap. The agent has already settled a pair that is not a real
/// layout to US (`session::keymap::settle`); the compositor also falls back on one that does
/// not compile.
pub(crate) fn keyboard_layout(container: Option<&ContainerSpec>) -> Option<(&str, &str)> {
    let env = &container?.env;
    let layout = env.get("XKB_DEFAULT_LAYOUT").filter(|l| !l.is_empty())?;
    let variant = env.get("XKB_DEFAULT_VARIANT").map_or("", String::as_str);
    Some((layout, variant))
}

#[cfg(test)]
mod tests {
    use super::{
        clamp_ring_depth, keyboard_layout, keyboard_properties_present, va_postproc_candidates,
    };
    use gstreamer as gst;

    #[test]
    fn keyboard_layout_needs_both_compositor_properties() {
        let with = |names: &'static [&'static str]| move |p: &str| names.contains(&p);
        assert!(keyboard_properties_present(with(&[
            "keyboard-layout",
            "keyboard-variant"
        ])));
        assert!(!keyboard_properties_present(with(&["keyboard-layout"])));
        assert!(!keyboard_properties_present(with(&["keyboard-variant"])));
        assert!(!keyboard_properties_present(with(&[])));
    }
    use crate::session::container::ContainerSpec;

    #[test]
    fn keyboard_layout_comes_from_the_app_env() {
        let spec = |pairs: &[(&str, &str)]| ContainerSpec {
            env: pairs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
            ..Default::default()
        };
        let de = spec(&[
            ("XKB_DEFAULT_LAYOUT", "de"),
            ("XKB_DEFAULT_VARIANT", "nodeadkeys"),
        ]);
        assert_eq!(keyboard_layout(Some(&de)), Some(("de", "nodeadkeys")));
        let no_variant = spec(&[("XKB_DEFAULT_LAYOUT", "fr")]);
        assert_eq!(keyboard_layout(Some(&no_variant)), Some(("fr", "")));
        // Unset or empty: the compositor's US keymap, as before amendment 49.
        let empty = spec(&[("XKB_DEFAULT_LAYOUT", ""), ("XKB_DEFAULT_VARIANT", "x")]);
        assert_eq!(keyboard_layout(Some(&empty)), None);
        assert_eq!(keyboard_layout(Some(&spec(&[]))), None);
        assert_eq!(keyboard_layout(None), None);
    }

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

    // An operator WOLF_VULKAN_RING outside the compositor's ring-depth range must not panic
    // the setter: it lands on the nearest bound.
    #[test]
    fn ring_depth_is_clamped_into_the_property_range() {
        let pspec = gst::glib::ParamSpecUInt::builder("ring-depth")
            .minimum(1)
            .maximum(4)
            .default_value(4)
            .build();
        assert_eq!(clamp_ring_depth(8, &pspec), 4);
        assert_eq!(clamp_ring_depth(0, &pspec), 1);
        assert_eq!(clamp_ring_depth(2, &pspec), 2);
    }
}
