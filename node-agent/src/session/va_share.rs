//! GW-02/GW-03 SPIKE (#260): create ONE `GstVaDisplay` and share it across the P2-07 swap
//! split (per-app **source** pipeline ↔ persistent **encode** pipeline) so the compositor's
//! in-Vulkan-NV12 surfaces (gst-wayland-display PR #37) are valid in the **separate** encode
//! pipeline. The VA analogue of [`super::cuda_share`].
//!
//! Why: PR #37's `va_share` binds each exported NV12 surface to a `GstVaDisplay`, and the VA
//! encoder only *reuses* (vs imports) a surface whose display matches its own. In a **single**
//! pipeline the encoder's display propagates upstream via `GstContext` and the compositor
//! adopts it (baseline: encodes fine, ~27 one-time surfaces, no per-frame growth). Across
//! Quasar's interpipe split there are **two** `GstPipeline`s, so the `GstContext` does **not**
//! propagate — GW-02 measured `vaInitialize=3` distinct displays and the encoder encoded
//! **zero** frames (`vaBeginPicture=0`). The fix (mirroring `cuda_share`, gst-wayland-display's
//! own pattern): the APPLICATION creates the `GstVaDisplay` from the render node and
//! **injects** it via the `gst.va.display.handle` `GstContext` into **both** pipelines before
//! READY, so the compositor and the encoder adopt OURS — one display, surfaces reused.
//!
//! `gstreamer-rs` has no `gstreamer-va` binding, so this is raw FFI against `libgstva-1.0`. The
//! lib is **dlopen'd at runtime** (mirroring PR #37's own dynamic load) rather than linked, so a
//! GPU-less / software build that never takes this path carries no hard dependency on it — a
//! missing lib is a runtime error on the VA-NV12 path only.

use anyhow::{anyhow, Result};
use gstreamer as gst;
use gstreamer::prelude::*;
use std::ffi::CString;
use std::os::raw::{c_char, c_void};

/// The `GstContext` type string VA elements use to share a `GstVaDisplay`
/// (`GST_VA_DISPLAY_HANDLE_CONTEXT_TYPE`). The field inside the context is `"gst-display"`.
const VA_DISPLAY_HANDLE_CONTEXT_TYPE: &str = "gst.va.display.handle";

#[allow(non_camel_case_types)]
type GstVaDisplayPtr = *mut c_void;

/// Resolved entry points from `libgstva-1.0`, loaded once.
struct VaLib {
    /// `gst_va_display_drm_new_from_path(path) -> GstVaDisplay*` (transfer full).
    drm_new_from_path: unsafe extern "C" fn(*const c_char) -> GstVaDisplayPtr,
    /// `gst_context_set_va_display(GstContext*, GstObject* display)` — stamps the display into a
    /// `gst.va.display.handle` context **exactly** how the elements' `gst_context_get_va_display`
    /// reads it back. Using the library's own setter (vs hand-building the `GValue`) is what makes
    /// the encoder accept the display — a hand-built structure was rejected (`No valid GstVaDisplay
    /// from context`) and the encoder fell back to its own display + starved.
    context_set_va_display: unsafe extern "C" fn(*mut gst::ffi::GstContext, GstVaDisplayPtr),
}

impl VaLib {
    fn load() -> Result<Self> {
        // SONAME first (always installed with the gst `va` plugin), dev symlink as fallback.
        for name in ["libgstva-1.0.so.0", "libgstva-1.0.so"] {
            let cname = CString::new(name).unwrap();
            let handle =
                unsafe { libc::dlopen(cname.as_ptr(), libc::RTLD_NOW | libc::RTLD_GLOBAL) };
            if handle.is_null() {
                continue;
            }
            let drm = unsafe { libc::dlsym(handle, c"gst_va_display_drm_new_from_path".as_ptr()) };
            let set_ctx = unsafe { libc::dlsym(handle, c"gst_context_set_va_display".as_ptr()) };
            if drm.is_null() || set_ctx.is_null() {
                unsafe { libc::dlclose(handle) };
                return Err(anyhow!(
                    "{name} loaded but is missing gst_va symbols — incompatible libgstva"
                ));
            }
            // Intentionally leak the dlopen handle: libgstva stays loaded for the PROCESS
            // lifetime (also held by the gst `va` plugin; concurrent sessions may hold VA
            // objects from it). Never dlclose — a teardown-into-unloaded-code hazard, same
            // model as cuda_share. The resolved fn pointers stay valid because the lib never
            // unmaps.
            return Ok(Self {
                // SAFETY: symbols resolved from libgstva-1.0 with the documented C signatures.
                drm_new_from_path: unsafe {
                    std::mem::transmute::<
                        *mut c_void,
                        unsafe extern "C" fn(*const c_char) -> GstVaDisplayPtr,
                    >(drm)
                },
                context_set_va_display: unsafe {
                    std::mem::transmute::<
                        *mut c_void,
                        unsafe extern "C" fn(*mut gst::ffi::GstContext, GstVaDisplayPtr),
                    >(set_ctx)
                },
            });
        }
        Err(anyhow!(
            "could not dlopen libgstva-1.0.so.0 — the gst `va` plugin is not installed in the \
             image (needed for the shared-VA-display path)"
        ))
    }
}

/// An application-owned `GstVaDisplay` for one render node, shared into both pipelines.
/// `Drop` unrefs the display (a refcounted `GstObject`) and closes the lib.
pub struct SharedVaDisplay {
    ptr: GstVaDisplayPtr,
    lib: VaLib,
}

// The pointer is a refcounted GstObject; we only hand it to GStreamer (which refs it) and unref
// once on Drop. Safe to move/share across the runner's threads.
unsafe impl Send for SharedVaDisplay {}
unsafe impl Sync for SharedVaDisplay {}

impl SharedVaDisplay {
    /// Create the shared `GstVaDisplay` for `render_node` (e.g. `/dev/dri/renderD128`).
    pub fn new(render_node: &str) -> Result<Self> {
        let lib = VaLib::load()?;
        let cpath = CString::new(render_node)
            .map_err(|_| anyhow!("render node path has an interior NUL: {render_node:?}"))?;
        let ptr = unsafe { (lib.drm_new_from_path)(cpath.as_ptr()) };
        if ptr.is_null() {
            return Err(anyhow!(
                "gst_va_display_drm_new_from_path({render_node}) returned NULL — no VA driver for \
                 this render node (need --device /dev/dri + mesa-va-drivers)"
            ));
        }
        tracing::info!("GW-03: created shared GstVaDisplay (render node {render_node})");
        Ok(Self { ptr, lib })
    }

    /// Build the `gst.va.display.handle` `GstContext` carrying this `GstVaDisplay`, to
    /// `set_context()` on a pipeline. Built via libgstva's own `gst_context_set_va_display` so the
    /// structure matches what every gst-va element reads back. Build once in the runner and inject
    /// into every pipeline (source(s) + encode). The VA analogue of
    /// [`super::cuda_share::SharedCudaContext::as_gst_context`].
    pub fn as_gst_context(&self) -> gst::Context {
        let context = gst::Context::new(VA_DISPLAY_HANDLE_CONTEXT_TYPE, true);
        // Use libgstva's OWN setter so the structure is byte-exact what the elements'
        // gst_context_get_va_display reads back. A hand-built GValue was rejected ("No valid
        // GstVaDisplay from context") and the encoder fell back to its own display + starved.
        // The fresh context has refcount 1 (writable), which gst_context_set_va_display requires.
        unsafe {
            let ctx_ptr = context.as_ptr() as *mut gst::ffi::GstContext;
            (self.lib.context_set_va_display)(ctx_ptr, self.ptr);
        }
        context
    }
}

/// Install a bus **sync** handler on `pipeline` that answers `GST_MESSAGE_NEED_CONTEXT`
/// of type `gst.va.display.handle` by setting `ctx` (our shared `GstVaDisplay`) directly
/// on the asking element — synchronously, during the element's own NULL→READY context
/// query, so it ADOPTS our display instead of creating its own.
///
/// `Pipeline::set_context()` alone is not enough: a device-pinned VA element
/// (`vapostproc`/`vah264enc`) must get the shared display injected AT ELEMENT CREATION,
/// before its `device-path` read or its own NULL→READY builds a `GstVaFilter` — a later
/// context is rejected with `Can't replace VA display while operating` and the element
/// keeps its own display, so the two pipelines run on different displays and the encoder
/// re-imports (or starves) instead of reusing the compositor's surfaces. Answering
/// NEED_CONTEXT in a sync handler lands the display in time; returning `Pass` still lets
/// the async source-socket watch run.
pub fn install_need_context_handler(pipeline: &gst::Pipeline, ctx: &gst::Context) {
    let Some(bus) = pipeline.bus() else {
        return;
    };
    let ctx = ctx.clone();
    bus.set_sync_handler(move |_bus, msg| {
        if let gst::MessageView::NeedContext(nc) = msg.view() {
            if nc.context_type() == VA_DISPLAY_HANDLE_CONTEXT_TYPE {
                if let Some(src) = msg.src() {
                    if let Some(el) = src.downcast_ref::<gst::Element>() {
                        el.set_context(&ctx);
                        tracing::info!(
                            "GW-03: answered NEED_CONTEXT(gst.va.display.handle) for {}",
                            el.name()
                        );
                    }
                }
            }
        }
        gst::BusSyncReply::Pass
    });
}

impl Drop for SharedVaDisplay {
    fn drop(&mut self) {
        // Drop our ref; any GstContext built from it holds its own, so the display finalizes
        // only once every context/pipeline is gone too. libgstva is NOT unloaded (see
        // VaLib::load), so finalize never jumps into unmapped code.
        unsafe {
            gst::ffi::gst_object_unref(self.ptr as *mut gst::ffi::GstObject);
        }
    }
}
