//! ZC-02 N-A (full zero-copy NVENC): create ONE `GstCudaContext` and share it across the
//! P2-07 swap split (per-app **source** pipeline ↔ persistent **encode** pipeline) so the
//! compositor's `memory:CUDAMemory` BGRA surfaces are valid across the interpipe boundary.
//!
//! Why: the two pipelines are separate `GstPipeline`s with separate buses, and
//! `gst_cuda_ensure_element_context`'s context discovery only searches within one
//! pipeline. Capturing the compositor's context off a `HAVE_CONTEXT` bus message
//! does not work — `waylanddisplaysrc` never posts one, only answers in-pipe
//! `NEED_CONTEXT`/`GST_QUERY_CONTEXT`. The fix (gst-wayland-display's own pattern):
//! the APPLICATION creates the `GstCudaContext` and **injects** it into both
//! pipelines via `set_context()` before READY, so every CUDA element (compositor,
//! cudaconvert, nvcudah264enc) adopts OURS instead of making its own.
//!
//! `gstreamer-rs` has no `gstreamer-cuda` binding, so this is raw FFI against libgstcuda-1.0
//! (linked via the `cuda` feature + build.rs). Only compiled with `--features cuda`.
//!
//! The context is a PROCESS-GLOBAL singleton per CUDA device ([`shared_cuda_context`]), not
//! per-session — see that function's doc for the leak that forced this.

use std::collections::HashMap;
use std::os::raw::c_int;
use std::sync::{Mutex, OnceLock};

use anyhow::{anyhow, Result};
use glib::translate::IntoGlib;
use gstreamer as gst;

/// The `GstContext` type string CUDA elements use to share a `GstCudaContext`
/// (`GST_CUDA_CONTEXT_TYPE`). The field name inside the context structure is the same.
pub(crate) const CUDA_CONTEXT_TYPE: &str = "gst.cuda.context";

#[allow(non_camel_case_types)]
type GstCudaContextPtr = *mut std::ffi::c_void;

mod ffi {
    use super::GstCudaContextPtr;
    use glib::ffi::{gboolean, GType};
    use std::os::raw::c_int;

    extern "C" {
        /// Load the nvcodec dynamic CUDA layer. Must be called before any other gst_cuda_*.
        pub fn gst_cuda_load_library() -> gboolean;
        /// Create a NEW GstCudaContext on `device_id` (a GstObject; refcounted). NULL on fail.
        pub fn gst_cuda_context_new(device_id: c_int) -> GstCudaContextPtr;
        /// The runtime GType of GstCudaContext — needed to store it in a GValue with the
        /// exact type so `gst_cuda_handle_set_context` accepts it.
        pub fn gst_cuda_context_get_type() -> GType;
    }
}

/// Process-global CUDA context cache: ONE `GstCudaContext` per CUDA device for the lifetime
/// of the node-agent, handed out as a (cheaply cloneable, refcounted) `gst::Context`.
///
/// Why a singleton and not per-session: gst-wayland-display leaked one reference to the
/// injected `GstCudaContext` per session, so a per-session context was never finalized —
/// ~500 MiB VRAM + one cuda-EvtHandlr driver thread leaked per session (Tower, 2026-07-25).
/// Root cause, fixed as the 10th vendored gwd patch
/// (`deploy/patches/vulkan/gst-wayland-display-cuda-pool-config-leak.patch`, see its README):
/// `CUDABufferPool::get_updated_size()` never freed the **(transfer full)** `GstStructure`
/// from `gst_buffer_pool_get_config()`, whose `cuda-stream` field re-refs the context. Not a
/// missing teardown hook (`CUDAContext::drop`/`GsCUDABuf::drop` both verifiably ran), so no
/// per-element Vulkan-style `clear()` would help; the fix pairs with a borrowed-caps
/// over-unref fix in the same gwd function.
///
/// The singleton stays regardless of the gwd fix: one context per device per process is
/// standard CUDA practice (nvcodec itself shares one across elements) and it saves ~70 ms of
/// session start. With the gwd patch in the image it is leak-**free** rather than
/// leak-**bounded**. Keyed per device for future multi-GPU routing (#273).
pub(crate) fn shared_cuda_context(device_id: i32) -> Result<gst::Context> {
    // #489 experiment knob (default: unchanged, process-global sharing).
    // `QUASAR_CUDA_CONTEXT_SHARED=0` hands every session its OWN GstCudaContext —
    // still shared between that session's own source+encode pipelines (the ZC-02
    // cross-interpipe CUDAMemory contract holds), only cross-session sharing goes
    // away. Tests whether the libnvcuvid UAF that kills the agent when one
    // session's NVENC encoder is destroyed while another is live is scoped to the
    // shared context. Costs ~70ms of session start and a second context's VRAM.
    if matches!(
        std::env::var("QUASAR_CUDA_CONTEXT_SHARED").ok().as_deref(),
        Some("0") | Some("false") | Some("no")
    ) {
        let owned = SharedCudaContext::new(device_id)?;
        tracing::info!(
            "#489: QUASAR_CUDA_CONTEXT_SHARED=0 — using a PER-SESSION GstCudaContext \
             (device {device_id}), not the process-global one"
        );
        return Ok(owned.as_gst_context());
    }
    static CACHE: OnceLock<Mutex<HashMap<i32, gst::Context>>> = OnceLock::new();
    let mut cache = CACHE
        .get_or_init(|| Mutex::new(HashMap::new()))
        .lock()
        .expect("cuda context cache poisoned");
    if let Some(ctx) = cache.get(&device_id) {
        tracing::debug!("ZC-02: reusing process-global GstCudaContext (device {device_id})");
        return Ok(ctx.clone());
    }
    // Creation failure is NOT cached — a transient driver problem may clear by next session.
    let owned = SharedCudaContext::new(device_id)?;
    let ctx = owned.as_gst_context();
    // `ctx` holds the only lasting ref to the GstCudaContext; `owned` drops here. Caching the
    // gst::Context keeps the device context alive for the process lifetime.
    cache.insert(device_id, ctx.clone());
    Ok(ctx)
}

/// An application-owned `GstCudaContext`, shared into both pipelines. `Drop` unrefs it.
struct SharedCudaContext {
    ptr: GstCudaContextPtr,
    device_id: i32,
}

// The pointer is a refcounted GstObject; we only hand it to GStreamer (which refs it) and
// unref once on Drop. Safe to move/share across the runner's threads.
unsafe impl Send for SharedCudaContext {}
unsafe impl Sync for SharedCudaContext {}

impl SharedCudaContext {
    /// Create the shared context for `device_id` (0 on the single-GPU box).
    fn new(device_id: i32) -> Result<Self> {
        unsafe {
            if ffi::gst_cuda_load_library() == glib::ffi::GFALSE {
                return Err(anyhow!(
                    "gst_cuda_load_library() failed — CUDA driver/plugin missing (need \
                     --gpus all / NVIDIA_DRIVER_CAPABILITIES=all and the nvcodec image)"
                ));
            }
            let ptr = ffi::gst_cuda_context_new(device_id as c_int);
            if ptr.is_null() {
                return Err(anyhow!("gst_cuda_context_new({device_id}) returned NULL"));
            }
            tracing::info!("ZC-02 N-A: created shared GstCudaContext (device {device_id})");
            Ok(Self { ptr, device_id })
        }
    }

    /// Build the `gst.cuda.context` `GstContext` carrying this `GstCudaContext` (+ device id),
    /// to `set_context()` on a pipeline. The object field MUST carry the exact
    /// `GstCudaContext` GType so the elements' `gst_cuda_handle_set_context` accepts it.
    /// Build once in the runner and inject into every pipeline (source(s) + encode).
    fn as_gst_context(&self) -> gst::Context {
        let mut context = gst::Context::new(CUDA_CONTEXT_TYPE, true);
        {
            let ctx = context
                .get_mut()
                .expect("freshly-created context is writable");
            let structure = ctx.structure_mut();
            let sptr = structure.as_mut_ptr();
            unsafe {
                use glib::gobject_ffi;
                // field "gst.cuda.context" = the GstCudaContext object (typed as its GType)
                let cuda_gtype = ffi::gst_cuda_context_get_type();
                let mut obj_val: gobject_ffi::GValue = std::mem::zeroed();
                gobject_ffi::g_value_init(&mut obj_val, cuda_gtype);
                gobject_ffi::g_value_set_object(
                    &mut obj_val,
                    self.ptr as *mut gobject_ffi::GObject,
                );
                gst::ffi::gst_structure_set_value(sptr, c"gst.cuda.context".as_ptr(), &obj_val);
                gobject_ffi::g_value_unset(&mut obj_val);
                // field "cuda-device-id" = gint
                let mut id_val: gobject_ffi::GValue = std::mem::zeroed();
                gobject_ffi::g_value_init(&mut id_val, glib::Type::I32.into_glib());
                gobject_ffi::g_value_set_int(&mut id_val, self.device_id as c_int);
                gst::ffi::gst_structure_set_value(sptr, c"cuda-device-id".as_ptr(), &id_val);
                gobject_ffi::g_value_unset(&mut id_val);
            }
        }
        context
    }
}

impl Drop for SharedCudaContext {
    fn drop(&mut self) {
        unsafe {
            gst::ffi::gst_object_unref(self.ptr as *mut gst::ffi::GstObject);
        }
    }
}
