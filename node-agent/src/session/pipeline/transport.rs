//! The transport slot of the media path (`compositor -> encode -> [transport slot]`,
//! CLAUDE.md invariant 3). The encode chain ends at the encoded-frame boundary, the
//! parser's src pad, and a [`Transport`] takes the session from there: packetizing,
//! congestion feedback, its client's input. Nothing upstream of that pad knows which
//! transport runs. WebRTC (`runner::SessionTransports`, over [`super::WebRtcSlot`]) is
//! the only implementation.

use std::sync::Arc;

use anyhow::Result;
use gstreamer as gst;

use super::abr_glue::EncodeResolutionLever;
use super::{OutTx, TraceTx};
use crate::session::input::InputState;
use crate::session::metrics::SessionMetrics;
use crate::session::virtual_input::VirtualDevices;
use crate::session::SessionConfig;

/// A session's encoded media, and what a transport needs to carry it.
pub struct EncodedSession {
    /// The session config, with the effective encoder.
    pub(crate) cfg: SessionConfig,
    /// The encode pipeline, still NULL. A transport adds its video elements here.
    pub(crate) pipeline: gst::Pipeline,
    /// The parser's src pad: encoded video in `cfg.stream.codec`, in the `stream-format`
    /// and `alignment` the transport asks for downstream (`alignment=au`: one access unit
    /// per buffer). PTS in the running time shared with the source (#68), though a unit
    /// may have none (openh264's second); `DELTA_UNIT` unset on a keyframe. Capture time
    /// is the wall clock as a unit leaves here. A configuration change (an external resize,
    /// an encoder restart) arrives as a new CAPS event on this pad, ahead of its keyframe.
    pub(crate) video: gst::Pad,
    /// The video encoder: `resize::force_idr` on it is the keyframe request, and the ABR
    /// writes its bitrate live.
    pub(crate) encoder: gst::Element,
    /// The resolution rung, for the ABR ladder.
    pub(crate) lever: Arc<EncodeResolutionLever>,
    pub(crate) metrics: Arc<SessionMetrics>,
    /// Messages to the client through the control plane's relay.
    pub(crate) out_tx: OutTx,
    /// Where the client's input goes: `input::handle` with these.
    pub(crate) devices: Option<Arc<VirtualDevices>>,
    pub(crate) input_state: Arc<InputState>,
    pub(crate) trace_tx: TraceTx,
    pub(crate) session_id: String,
    /// `QUASAR_DIAG_NO_OBS` unset.
    pub(crate) observe: bool,
    /// The PyroWave feed of a granted WebTransport session, off a tee before the encoder.
    pub(crate) pyrowave_feed: Option<super::pyrowave_branch::Feed>,
}

/// One implementation of the transport slot. Audio is the transport's own: it builds the
/// capture and Opus encode (`audio_branch::add_opus_encode`) in a pipeline of its own
/// (#304), so the stream audio follows its client. The audio watchdog
/// (`audio_health::attach`) is on the WebRTC tail only: a second transport adds its own.
pub trait Transport: Sized {
    /// Take the session at the encoded-frame boundary and build what follows it. Leaves
    /// the encode pipeline READY; the runner takes it to PLAYING.
    fn attach(session: EncodedSession) -> Result<Self>;

    /// Let go of the client on a session's way out. Must run before the source is
    /// stopped: a transport that holds the encode chain's push keeps a source waiting on
    /// that chain from reaching NULL (#535).
    fn release(&self);
}
