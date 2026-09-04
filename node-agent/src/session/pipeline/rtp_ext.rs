//! g2g: abs-capture-time RTP header extension writer.
//!
//! Writes a UQ32.32 NTP-64 wall-clock timestamp into every RTP video packet as a
//! one-byte-header extension (id 6, 8 bytes; the extmap id lives in
//! [`super::ABS_CAPTURE_TIME_EXT_ID`], shared with the payloader-caps setup). The SDP
//! extmap line is declared in the payloader caps; the payloader registration records the
//! capture instant, and a post-rtpbin auxiliary sender restores the extension after TWCC
//! rewrites the RFC5285 block and before SRTP. The browser can then compute end-to-end
//! delay with no pixel overlay, on any encoder.

use glib::subclass::prelude::ObjectSubclassIsExt;
use gstreamer as gst;
use gstreamer::prelude::*;
use gstreamer_rtp::prelude::*;
use gstreamer_rtp::RTPBuffer;

use super::ABS_CAPTURE_TIME_EXT_ID;

mod imp {
    use std::collections::VecDeque;
    use std::sync::Mutex;

    use gstreamer_rtp::subclass::prelude::*;

    use super::*;

    #[derive(Default)]
    pub struct AbsCaptureTime {
        frames: Mutex<VecDeque<(u32, u64)>>,
    }

    #[glib::object_subclass]
    impl ObjectSubclass for AbsCaptureTime {
        const NAME: &'static str = "QuasarAbsCaptureTimeExtension";
        type Type = super::AbsCaptureTimeExtension;
        type ParentType = gstreamer_rtp::RTPHeaderExtension;
    }

    impl ObjectImpl for AbsCaptureTime {}
    impl GstObjectImpl for AbsCaptureTime {}
    impl ElementImpl for AbsCaptureTime {}

    impl RTPHeaderExtensionImpl for AbsCaptureTime {
        const URI: &'static str = super::super::ABS_CAPTURE_TIME_URI;

        fn supported_flags(&self) -> gstreamer_rtp::RTPHeaderExtensionFlags {
            gstreamer_rtp::RTPHeaderExtensionFlags::ONE_BYTE
        }

        fn max_size(&self, _input: &gst::BufferRef) -> usize {
            8
        }

        fn write(
            &self,
            _input: &gst::BufferRef,
            _write_flags: gstreamer_rtp::RTPHeaderExtensionFlags,
            output: &gst::BufferRef,
            output_data: &mut [u8],
        ) -> Result<usize, gst::LoggableError> {
            if output_data.len() < 8 {
                return Err(gst::loggable_error!(
                    gst::CAT_RUST,
                    "abs-capture-time output too small"
                ));
            }
            let rtp = RTPBuffer::from_buffer_readable(output).map_err(|_| {
                gst::loggable_error!(gst::CAT_RUST, "abs-capture-time output is not RTP")
            })?;
            let rtp_timestamp = rtp.timestamp();
            let ntp64 = self.capture_time_for(rtp_timestamp)?;
            output_data[..8].copy_from_slice(&ntp64.to_be_bytes());
            Ok(8)
        }
    }

    impl AbsCaptureTime {
        fn capture_time_for(&self, rtp_timestamp: u32) -> Result<u64, gst::LoggableError> {
            let mut frames = self.frames.lock().map_err(|_| {
                gst::loggable_error!(gst::CAT_RUST, "abs-capture-time state poisoned")
            })?;
            if let Some((_, ntp64)) = frames
                .iter()
                .find(|(timestamp, _)| *timestamp == rtp_timestamp)
            {
                return Ok(*ntp64);
            }
            let ntp64 = ntp64_now();
            frames.push_back((rtp_timestamp, ntp64));
            // >8 s at 60 fps, beyond the bounded RTP pacing queue.
            if frames.len() > 512 {
                frames.pop_front();
            }
            Ok(ntp64)
        }

        pub(super) fn recorded_capture_time(&self, rtp_timestamp: u32) -> Option<u64> {
            self.frames
                .lock()
                .ok()?
                .iter()
                .find_map(|(timestamp, ntp64)| (*timestamp == rtp_timestamp).then_some(*ntp64))
        }
    }
}

glib::wrapper! {
    pub struct AbsCaptureTimeExtension(ObjectSubclass<imp::AbsCaptureTime>)
        @extends gstreamer_rtp::RTPHeaderExtension, gst::Element, gst::Object;
}

/// Register abs-capture-time with `pay` — whichever video payloader the session's codec
/// resolved to (`rtph264pay`/`rtph265pay`/`rtpav1pay`, see `Codec::rtp_payloader`) — so
/// downstream RTP helpers know the URI/id association and preserve it through
/// RTX/FEC/WebRTC transport. Codec-agnostic by construction: the caller always passes the
/// one payloader the session actually built, never a hardcoded element.
pub(super) fn attach_abs_capture_time_probe(pay: &gst::Element) -> AbsCaptureTimeExtension {
    let extension: AbsCaptureTimeExtension = glib::Object::builder().build();
    extension.set_id(ABS_CAPTURE_TIME_EXT_ID);
    pay.emit_by_name::<()>("add-extension", &[&extension]);
    let pay_name = pay
        .factory()
        .map(|f| f.name().to_string())
        .unwrap_or_else(|| pay.name().to_string());
    tracing::info!(
        "g2g abs-capture-time RTPHeaderExtension installed on {pay_name} (extmap-{ABS_CAPTURE_TIME_EXT_ID}, always-on)"
    );
    extension
}

/// Stamp abs-capture-time after `rtpbin` inserts TWCC and immediately before the transport
/// send bin applies SRTP (the stable post-RTP seam GStreamer 1.26+ exposes). `rtpbin`
/// rebuilds the RFC5285 block while adding TWCC, so the payloader registration is kept for
/// SDP/capture-time recording and the extension is restored here.
///
/// `QUASAR_LATENCY_PROBE` reuses this seam, the last point before SRTP and the socket, as
/// the host-stage probe's S4 sink: on an AU's marker packet, now minus the NTP-64 the
/// extension recorded for that RTP timestamp is the unit's whole time through `rtpbin`,
/// TWCC insertion and the pacer. No extra probe, one extra clock read per frame. Design:
/// `docs/superpowers/specs/2026-08-18-latency-probe-design.md`.
pub(super) fn attach_abs_capture_time_egress_probe(
    webrtc: &gst::Element,
    capture_times: AbsCaptureTimeExtension,
    latency_probe: Option<std::sync::Arc<crate::session::metrics::SessionMetrics>>,
) {
    use std::sync::atomic::{AtomicBool, Ordering};

    webrtc.connect("request-post-rtp-aux-sender", false, move |_values| {
        let identity = match gst::ElementFactory::make("identity").build() {
            Ok(identity) => identity,
            Err(err) => {
                tracing::warn!(
                    token = "abs-capture-time-identity-missing","abs-capture-time egress identity unavailable: {err}");
                return None;
            }
        };
        let Some(src_pad) = identity.static_pad("src") else {
            tracing::warn!(
                token = "abs-capture-time-no-src-pad","abs-capture-time egress identity has no src pad");
            return None;
        };
        let capture_times = capture_times.clone();
        let latency_probe = latency_probe.clone();
        let probe_state = std::sync::Arc::new(std::sync::Mutex::new(EgressProbeState::default()));
        src_pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
            let Some(buffer) = info.buffer_mut() else {
                return gst::PadProbeReturn::Ok;
            };
            let buffer = buffer.make_mut();
            let Ok(mut rtp) = RTPBuffer::from_buffer_writable(buffer) else {
                return gst::PadProbeReturn::Ok;
            };
            let ts = rtp.timestamp();
            let recorded = capture_times.imp().recorded_capture_time(ts);

            // Host-stage latency probe (S3 + S4). Runs BEFORE the payload-type gate below
            // and must NOT use it: that gate is right for stamping (only PT 96 carries
            // extmap-6) but wrong as a probe filter, because with `QUASAR_FEC_PERCENTAGE`
            // armed RED/ULPFEC repacks the video onto a PT webrtcbin allocates during
            // negotiation, so no marker packet would ever reach the probe and S3/S4 would
            // be silently empty for the whole run.
            //
            // The discriminator instead needs no negotiated value: a timestamp the video
            // payloader's own extension recorded, carrying the marker bit, seen for the
            // FIRST time. First-time-only is what makes it safe against RTX, which
            // retransmits the original timestamp and marker and would close a second pair.
            if let (Some(metrics), Some(ntp64)) = (latency_probe.as_ref(), recorded) {
                if rtp.is_marker() {
                    let mut st = probe_state.lock().unwrap_or_else(|e| e.into_inner());
                    if st.close(ts) {
                        // One call on the pacer thread: S3 = encoder src → here (closed
                        // against the FIFO); S4 = payloader → here, from the NTP-64 the
                        // extension recorded on this AU's first packet.
                        metrics.probe_record_send(
                            std::time::Instant::now(),
                            ntp64_delta_ms(ntp64, ntp64_now()),
                        );
                    }
                } else if st_should_warn_silent(&probe_state) {
                    tracing::warn!(
                        token = "probe-no-marker-at-egress",
                        "latency probe: armed, but no marker packet has reached the egress seam \
                         in over 2 s — probe_enc_out_to_send_* and probe_pay_to_send_* will be \
                         empty. The other stages are unaffected."
                    );
                }
            }

            // Only the video stream owns extmap-6, any codec (pt=96 always, `codec_chain.rs`);
            // audio shares the bundled DTLS transport on a different RTP session/payload type.
            if rtp.payload_type() != 96 {
                return gst::PadProbeReturn::Ok;
            }
            let Some(ntp64) = recorded else {
                static MISSING_LOGGED: AtomicBool = AtomicBool::new(false);
                if !MISSING_LOGGED.swap(true, Ordering::Relaxed) {
                    tracing::warn!(
                        token = "abs-capture-time-ts-mismatch","abs-capture-time egress stamp missed its recorded RTP timestamp");
                }
                return gst::PadProbeReturn::Ok;
            };
            let bytes = ntp64.to_be_bytes();
            match rtp.add_extension_onebyte_header(ABS_CAPTURE_TIME_EXT_ID as u8, &bytes) {
                Ok(()) => {
                    static STAMPED_LOGGED: AtomicBool = AtomicBool::new(false);
                    if !STAMPED_LOGGED.swap(true, Ordering::Relaxed) {
                        let twcc = rtp.extension_onebyte_header(5, 0).is_some();
                        tracing::info!(
                            "g2g abs-capture-time stamped after rtpbin before SRTP (extmap-{ABS_CAPTURE_TIME_EXT_ID}, 8 bytes, twcc_preserved={twcc})"
                        );
                    }
                }
                Err(err) => {
                    static ERR_LOGGED: AtomicBool = AtomicBool::new(false);
                    if !ERR_LOGGED.swap(true, Ordering::Relaxed) {
                        tracing::warn!(
                            token = "abs-capture-time-stamp-failed","abs-capture-time egress stamp failed: {err}");
                    }
                }
            }
            gst::PadProbeReturn::Ok
        });
        // MUST be handed back FLOATING, like the ABR aux sender: webrtcbin ref_sinks the
        // return on bin-add, and a sunk strong handle leaks one ref per session (the
        // identity, and through its pad probe the abs-capture-time extension).
        unsafe {
            gst::glib::gobject_ffi::g_object_force_floating(
                identity.as_ptr() as *mut gst::glib::gobject_ffi::GObject
            );
        }
        Some(identity.to_value())
    });
}

/// Verify once that the stamped extension survives the fixed RTP capsfilter and reaches
/// the `webrtcbin` input. One log line per process, not a per-packet trace.
pub(super) fn attach_abs_capture_time_verification_probe(rtp_capsfilter: &gst::Element) {
    use std::sync::atomic::{AtomicBool, Ordering};

    let Some(src_pad) = rtp_capsfilter.static_pad("src") else {
        tracing::warn!(
            token = "rtp-capsfilter-no-src-pad",
            "RTP capsfilter has no src pad — abs-capture-time verification unavailable"
        );
        return;
    };
    static VERIFIED: AtomicBool = AtomicBool::new(false);
    src_pad.add_probe(gst::PadProbeType::BUFFER, move |_pad, info| {
        if VERIFIED.load(Ordering::Relaxed) {
            return gst::PadProbeReturn::Ok;
        }
        let Some(buffer) = info.buffer() else {
            return gst::PadProbeReturn::Ok;
        };
        let found = RTPBuffer::from_buffer_readable(buffer)
            .ok()
            .and_then(|rtp| {
                rtp.extension_onebyte_header(ABS_CAPTURE_TIME_EXT_ID as u8, 0)
                    .map(|data| data.len())
            });
        if !VERIFIED.swap(true, Ordering::Relaxed) {
            match found {
                Some(8) => tracing::info!(
                    "g2g abs-capture-time verified at webrtcbin input (extmap-{ABS_CAPTURE_TIME_EXT_ID}, 8 bytes)"
                ),
                Some(len) => tracing::warn!(
                    token = "abs-capture-time-malformed",
                    "g2g abs-capture-time malformed at webrtcbin input (extmap-{ABS_CAPTURE_TIME_EXT_ID}, {len} bytes)"
                ),
                None => tracing::warn!(
                    token = "abs-capture-time-absent",
                    "g2g abs-capture-time absent at webrtcbin input (extmap-{ABS_CAPTURE_TIME_EXT_ID})"
                ),
            }
        }
        gst::PadProbeReturn::Ok
    });
}

/// Per-session state for the latency probe's egress close.
struct EgressProbeState {
    /// RTP timestamps already closed, newest last. Bounded: RTX only retransmits recent
    /// packets, so a short memory is enough to reject a duplicate close.
    closed: std::collections::VecDeque<u32>,
    /// With `ever_closed`, detects "armed but nothing is arriving", which would otherwise
    /// present as two silently absent stages.
    first_seen: Option<std::time::Instant>,
    ever_closed: bool,
    warned_silent: bool,
}

impl Default for EgressProbeState {
    fn default() -> Self {
        Self {
            closed: std::collections::VecDeque::with_capacity(EGRESS_CLOSED_MEMORY),
            first_seen: None,
            ever_closed: false,
            warned_silent: false,
        }
    }
}

/// Recently-closed RTP timestamps remembered for RTX de-duplication. 64 access units is
/// ~1 s at 60 fps, beyond any retransmission window.
const EGRESS_CLOSED_MEMORY: usize = 64;

impl EgressProbeState {
    /// Claim this access unit. `false` if already closed — an RTX retransmission of a
    /// frame whose pair was already taken.
    fn close(&mut self, ts: u32) -> bool {
        if self.closed.contains(&ts) {
            return false;
        }
        self.closed.push_back(ts);
        if self.closed.len() > EGRESS_CLOSED_MEMORY {
            self.closed.pop_front();
        }
        self.ever_closed = true;
        true
    }
}

/// True exactly once, when the probe has been receiving packets for over two seconds
/// without a single marker packet ever closing a pair.
fn st_should_warn_silent(state: &std::sync::Mutex<EgressProbeState>) -> bool {
    let mut st = state.lock().unwrap_or_else(|e| e.into_inner());
    let now = std::time::Instant::now();
    let first = *st.first_seen.get_or_insert(now);
    if st.ever_closed || st.warned_silent {
        return false;
    }
    if now.saturating_duration_since(first) < std::time::Duration::from_secs(2) {
        return false;
    }
    st.warned_silent = true;
    true
}

/// Elapsed milliseconds between two UQ32.32 NTP-64 timestamps. Saturates at 0 on a
/// non-monotonic pair (a `CLOCK_REALTIME` step between the reads) rather than wrapping.
fn ntp64_delta_ms(earlier: u64, later: u64) -> f64 {
    if later <= earlier {
        return 0.0;
    }
    // 1 UQ32 fraction unit = 1/2^32 s ⇒ ms = ticks * 1000 / 2^32.
    (later - earlier) as f64 * 1000.0 / 4_294_967_296.0
}

/// Wall-clock time as a UQ32.32 NTP-64 timestamp: upper 32 bits whole NTP seconds (epoch
/// 1900-01-01, 2,208,988,800 s before the Unix epoch), lower 32 bits the sub-second
/// fraction in 1/2^32 s units.
fn ntp64_now() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    let unix = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default();
    let ntp_secs = unix.as_secs().wrapping_add(2_208_988_800);
    // nanos < 1_000_000_000, so `nanos << 32` fits in u64.
    let frac = ((unix.subsec_nanos() as u64) << 32) / 1_000_000_000;
    (ntp_secs << 32) | frac
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::session::Codec;

    fn init() {
        gst::init().unwrap();
    }

    /// Regression for #95: abs-capture-time was only ever exercised against
    /// `rtph264pay`, so h265/av1 sessions silently carried no capture timestamps.
    /// `attach_abs_capture_time_probe` takes whatever payloader the codec resolved
    /// to — this locks that dispatch in for all three.
    #[test]
    fn attaches_to_every_codecs_payloader() {
        init();
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            let factory_name = codec.rtp_payloader();
            let Ok(pay) = gst::ElementFactory::make(factory_name).build() else {
                // rtpav1pay ships in gst-plugins-rs (`rsrtp`); skip rather than fail if
                // this GStreamer install doesn't have it registered.
                eprintln!("skipping {factory_name}: not available in this GStreamer install");
                continue;
            };
            attach_abs_capture_time_probe(&pay);

            let extensions = pay.property::<gst::Array>("extensions");
            let attached = extensions.as_slice();
            assert_eq!(
                attached.len(),
                1,
                "{factory_name} should carry exactly one registered extension"
            );
            let ext = attached[0]
                .get::<gstreamer_rtp::RTPHeaderExtension>()
                .unwrap_or_else(|_| {
                    panic!("{factory_name}'s registered extension is not an RTPHeaderExtension")
                });
            assert_eq!(
                ext.id(),
                ABS_CAPTURE_TIME_EXT_ID,
                "{factory_name} extmap id"
            );
            assert_eq!(
                ext.uri().as_deref(),
                Some(super::super::ABS_CAPTURE_TIME_URI),
                "{factory_name} extension URI"
            );
        }
    }
}
