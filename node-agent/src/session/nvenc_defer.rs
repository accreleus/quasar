//! #489 experiment — deferred NVENC encode-pipeline teardown.
//!
//! #489 is an NVIDIA driver use-after-free (`libnvcuvid+0x19fc1`, spans the 595 and 610
//! branches, no driver pin escapes it): destroying one session's NVENC encoder while
//! ANOTHER session's is live/starting corrupts process-global NVENC state and SIGSEGVs
//! the whole node-agent, taking every session on the host with it.
//!
//! Never destroy an NVENC encoder while another is live. On session end the encode
//! pipeline is **parked** (left alive, no state transition, no driver call) and the real
//! `set_state(NULL)` is deferred until the host has ZERO sessions holding an encode lease.
//!
//! Mechanics: every encoded session takes an [`EncodeLease`] before its encoder opens and
//! drops it when the runner returns; teardown goes through [`finish_encode`], which NULLs
//! immediately (knob off / non-NVENC) or parks; the last lease to drop drains the parked
//! list **while holding the registry mutex**, so a session starting up (`EncodeLease::acquire`
//! blocks on the same mutex) cannot open an encoder mid-drain.
//!
//! **The parked list is not bounded by the host's encode slots** — it grows with how many
//! sessions end while another is still live, unboundedly under a continuous session stream
//! (measured: 5 sequential sessions parked 5 pipelines, +959 MiB, on the devbox RTX 5090).
//! A host that never reaches zero live encoders never drains. Production use needs a cap on
//! the parked list; refusing new launches is the only safe response to hitting it, since
//! draining under a live encoder is exactly the crash this avoids.
//!
//! On by default for NVENC sessions — the alternative default is a host-wide SIGSEGV nobody
//! opted into protection from. `QUASAR_NVENC_DEFER_TEARDOWN=0` opts out.
//!
//! Scope is NVENC only: that's where the bug is proven (`libnvcuvid` via
//! `libnvidia-encode`), not a claim VA or Vulkan are immune — if either turns out to share
//! the corrupted state, this module's gate is where to widen it.

use std::sync::{Mutex, OnceLock};

use gstreamer as gst;
use gstreamer::prelude::ElementExt;

use super::EncoderChoice;

/// Whether deferred teardown applies to this session: NVENC, and not opted out
/// (`QUASAR_NVENC_DEFER_TEARDOWN=0`). Defaults ON.
pub(crate) fn deferral_enabled(encoder: EncoderChoice) -> bool {
    if encoder != EncoderChoice::Nvenc {
        return false;
    }
    !matches!(
        std::env::var("QUASAR_NVENC_DEFER_TEARDOWN").ok().as_deref(),
        Some("0") | Some("false") | Some("no")
    )
}

#[derive(Default)]
struct Registry {
    /// Sessions currently holding an encode lease (encoder open or about to be).
    live: usize,
    /// Ended sessions' encode pipelines, teardown deferred until `live == 0`.
    parked: Vec<gst::Pipeline>,
}

fn registry() -> &'static Mutex<Registry> {
    static REGISTRY: OnceLock<Mutex<Registry>> = OnceLock::new();
    REGISTRY.get_or_init(|| Mutex::new(Registry::default()))
}

/// Held for the whole encoding lifetime of a session. While any lease is alive,
/// no parked encode pipeline is destroyed.
pub(crate) struct EncodeLease {
    active: bool,
}

impl EncodeLease {
    /// Take a lease if `enabled`; otherwise a no-op token.
    pub(crate) fn acquire(enabled: bool) -> Self {
        if !enabled {
            return Self { active: false };
        }
        let mut reg = registry().lock().expect("nvenc defer registry poisoned");
        reg.live += 1;
        tracing::info!(
            live = reg.live,
            parked = reg.parked.len(),
            "#489 defer: encode lease acquired"
        );
        Self { active: true }
    }
}

impl Drop for EncodeLease {
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let mut reg = registry().lock().expect("nvenc defer registry poisoned");
        reg.live = reg.live.saturating_sub(1);
        if reg.live > 0 {
            tracing::info!(
                live = reg.live,
                parked = reg.parked.len(),
                "#489 defer: encode lease released; host still encoding, teardown stays deferred"
            );
            return;
        }
        let parked = std::mem::take(&mut reg.parked);
        if parked.is_empty() {
            return;
        }
        tracing::info!(
            count = parked.len(),
            "#489 defer: host idle — draining parked encode pipelines"
        );
        // Still holding the registry lock: a session starting up blocks in
        // `acquire` until every parked encoder is destroyed, so no NVENC
        // encoder can be opened while these are being torn down.
        for pipe in &parked {
            let _ = pipe.set_state(gst::State::Null);
        }
        drop(parked);
        tracing::info!("#489 defer: parked encode pipelines destroyed");
    }
}

/// End-of-life for a session's encode pipeline. With deferral off this is
/// exactly the previous `set_state(NULL)`; with deferral on the pipeline is
/// parked (untouched, alive) for the drain that follows the last lease.
///
/// Idempotent: the runner NULLs the encode pipeline from many exit paths, and
/// parking the same pipeline twice must not double-register it.
pub(crate) fn finish_encode(pipeline: &gst::Pipeline, defer: bool) {
    if !defer {
        let _ = pipeline.set_state(gst::State::Null);
        return;
    }
    let mut reg = registry().lock().expect("nvenc defer registry poisoned");
    if reg.parked.iter().any(|p| same_object(p, pipeline)) {
        return;
    }
    reg.parked.push(pipeline.clone());
    tracing::info!(
        parked = reg.parked.len(),
        live = reg.live,
        "#489 defer: encode pipeline PARKED instead of destroyed (NVENC teardown deferred \
         until the host has no live encoders)"
    );
}

/// Pointer identity of two `GstPipeline`s (a `gst::Pipeline` is a refcounted
/// handle, so `clone()` gives another handle to the same C object).
fn same_object(a: &gst::Pipeline, b: &gst::Pipeline) -> bool {
    use glib::translate::ToGlibPtr;
    let pa: *mut gst::ffi::GstPipeline = a.to_glib_none().0;
    let pb: *mut gst::ffi::GstPipeline = b.to_glib_none().0;
    std::ptr::eq(pa, pb)
}

#[cfg(test)]
mod tests {
    use super::*;
    use gstreamer::prelude::ElementExtManual;

    fn pipeline(name: &str) -> gst::Pipeline {
        gst::init().unwrap();
        gst::Pipeline::with_name(name)
    }

    /// The registry is process-global, so these tests must not interleave.
    fn serialize() -> std::sync::MutexGuard<'static, ()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(()))
            .lock()
            .unwrap_or_else(|e| e.into_inner())
    }

    #[test]
    fn deferral_requires_nvenc_and_the_knob() {
        // The knob is process-global env; assert the encoder gate alone, which
        // is the part that holds regardless of the ambient environment.
        assert!(!deferral_enabled(EncoderChoice::Va));
        assert!(!deferral_enabled(EncoderChoice::Openh264));
        assert!(!deferral_enabled(EncoderChoice::Vulkan));
    }

    #[test]
    fn finish_encode_without_deferral_nulls_immediately() {
        let _guard = serialize();
        let p = pipeline("defer-test-immediate");
        finish_encode(&p, false);
        assert_eq!(p.current_state(), gst::State::Null);
        // and nothing was parked
        assert!(registry().lock().unwrap().parked.is_empty());
    }

    #[test]
    fn parking_is_idempotent_and_drains_when_the_last_lease_drops() {
        let _guard = serialize();
        let p = pipeline("defer-test-parked");
        let lease = EncodeLease::acquire(true);
        finish_encode(&p, true);
        finish_encode(&p, true);
        assert_eq!(
            registry().lock().unwrap().parked.len(),
            1,
            "parking the same pipeline twice must register it once"
        );
        drop(lease);
        assert!(
            registry().lock().unwrap().parked.is_empty(),
            "the last lease to drop must drain the parked list"
        );
        assert_eq!(p.current_state(), gst::State::Null);
    }

    #[test]
    fn a_second_live_lease_keeps_the_teardown_deferred() {
        let _guard = serialize();
        let p = pipeline("defer-test-two-leases");
        let a = EncodeLease::acquire(true);
        let b = EncodeLease::acquire(true);
        finish_encode(&p, true);
        drop(a);
        assert_eq!(
            registry().lock().unwrap().parked.len(),
            1,
            "a still-encoding session must keep the parked pipeline alive"
        );
        drop(b);
        assert!(registry().lock().unwrap().parked.is_empty());
    }
}
