//! Codec-dispatched bitstream chain: the encoder-output capsfilter → parser → RTP
//! payloader triplet. This module owns everything from the encoder src caps up to,
//! but not including, the RTP caps — those stay in the builders (they differ between
//! the demo and encode paths), taking their encoding-name from
//! [`Codec::rtp_encoding_name`].
//!
//! Per spec §4.3:
//! |         | h264                                               | h265                                                | av1                              |
//! |---------|----------------------------------------------------|-----------------------------------------------------|----------------------------------|
//! | enc caps| `video/x-h264,profile=`                             | `video/x-h265,profile=main`                         | `video/x-av1` (no profile field) |
//! | parser  | `h264parse`                                        | `h265parse`                                         | `av1parse`                       |
//! | payload | `rtph264pay config-interval=-1 aggregate-mode=zero-latency pt=96` | `rtph265pay config-interval=-1 aggregate-mode=none pt=96` | `rtpav1pay pt=96` (RtpBasePay2)  |

use anyhow::{Context, Result};
use gstreamer as gst;
use gstreamer::prelude::*;

use crate::session::Codec;

/// The encoder-output capsfilter, bitstream parser, and RTP payloader for a codec.
/// The caller adds all three to the pipeline and links them between the encoder and
/// the RTP capsfilter.
pub(super) struct BitstreamChain {
    /// `video/x-{codec}[,profile=…]`. Drives the hardware encoder's output profile
    /// (VA/NVENC/Vulkan expose no `profile` property, so downstream caps select it)
    /// and, for h264/h265, lands in the SDP offer — so it must equal the real
    /// bitstream.
    pub encoder_capsfilter: gst::Element,
    pub parser: gst::Element,
    /// Configured for low-latency WebRTC.
    pub payloader: gst::Element,
}

/// Build the [`BitstreamChain`] for `codec`. `profile` is the caps `profile` string
/// (from [`super::caps::caps_profile`]) — `Some` for h264/h265, `None` for av1
/// (no profile caps field).
/// `rtp_mtu` is the session's `QUASAR_RTP_MTU` ([`set_rtp_mtu`]).
pub(super) fn build_bitstream_chain(
    codec: Codec,
    profile: Option<&str>,
    rtp_mtu: u32,
) -> Result<BitstreamChain> {
    // Pin the media type and, for h264/h265, the profile, so the encoder emits exactly
    // what the SDP advertises (the T8 constraint).
    let mut caps_builder = gst::Caps::builder(codec.caps_name());
    if let Some(p) = profile {
        caps_builder = caps_builder.field("profile", p);
    }
    let encoder_caps = caps_builder.build();
    let encoder_capsfilter = gst::ElementFactory::make("capsfilter")
        .property("caps", &encoder_caps)
        .build()
        .context("capsfilter not found")?;

    let parser = gst::ElementFactory::make(codec.parser_element())
        .build()
        .with_context(|| format!("{} not found", codec.parser_element()))?;

    // h264/h265 want `config-interval=-1` (parameter sets in-band with every IDR).
    // rtpav1pay is RtpBasePay2-based and exposes neither that nor aggregate-mode, so
    // every set below is guarded on the property existing.
    let payloader = gst::ElementFactory::make(codec.rtp_payloader())
        .name(super::VIDEO_PAYLOADER_NAME)
        .build()
        .with_context(|| {
            format!(
                "{} not found — is the RTP payloader plugin in the image? \
                 (rtpav1pay ships in gst-plugins-rs `rsrtp`)",
                codec.rtp_payloader()
            )
        })?;
    // pt=96 for every codec: one codec per session, send-only, so the PT-96 gates and
    // the TWCC/abs-capture-time extmap recipe carry over unchanged.
    set_prop_u32(&payloader, "pt", 96);
    set_rtp_mtu(&payloader, rtp_mtu);
    set_prop_i32(&payloader, "config-interval", -1);
    // H.265 must NOT aggregate: `zero-latency` wraps VPS/SPS/PPS in an H.265
    // Aggregation Packet (NAL 48) that shipping Chrome's depacketizer mishandles, so
    // the decoder never initializes (framesDecoded=0, PLI storm — root-caused live).
    // `none` emits single NAL units and adds no latency. H.264's STAP-A path is
    // decade-proven in Chrome and keeps zero-latency aggregation.
    let aggregate_mode = if codec == Codec::H265 {
        "none"
    } else {
        "zero-latency"
    };
    set_prop_from_str(&payloader, "aggregate-mode", aggregate_mode);

    Ok(BitstreamChain {
        encoder_capsfilter,
        parser,
        payloader,
    })
}

/// Bytes the send path adds to a packet after the payloader's `mtu`, which counts no
/// header extension. Measured at `srtpenc` with a 1100-byte payloader `mtu`: +28 without
/// FEC (abs-capture-time, written by the payloader and again at egress, plus TWCC), +39
/// with RED/ULPFEC. The rest covers RTX's 2-byte sequence number and ULPFEC's long mask
/// (4). Guarded by `takeover_tests::rtp_mtu::no_rtp_packet_is_larger_than_the_rtp_mtu`.
const RTP_SEND_OVERHEAD: u32 = 48;

/// The payloader `mtu` that keeps every packet on the wire within `rtp_mtu` (#1131).
/// `rtp_mtu` is clamped to `QUASAR_RTP_MTU`'s range, so a computed value (#1134) can
/// never underflow or exceed a 1500-byte path.
pub(super) fn payloader_mtu(rtp_mtu: u32) -> u32 {
    let range = crate::session::RTP_MTU_RANGE;
    rtp_mtu.clamp(*range.start(), *range.end()) - RTP_SEND_OVERHEAD
}

/// The one place a payloader's `mtu` is set, at build time and live: the payloaders read
/// it per buffer, so a change applies from the next packet.
pub(super) fn set_rtp_mtu(payloader: &gst::Element, rtp_mtu: u32) {
    set_prop_u32(payloader, "mtu", payloader_mtu(rtp_mtu));
}

fn set_prop_u32(el: &gst::Element, name: &str, v: u32) {
    if el.find_property(name).is_some() {
        el.set_property(name, v);
    }
}

fn set_prop_i32(el: &gst::Element, name: &str, v: i32) {
    if el.find_property(name).is_some() {
        el.set_property(name, v);
    }
}

fn set_prop_from_str(el: &gst::Element, name: &str, v: &str) {
    if el.find_property(name).is_some() {
        el.set_property_from_str(name, v);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_payloader_mtu_is_clamped_to_the_knobs_range() {
        assert_eq!(payloader_mtu(1200), 1152);
        assert_eq!(payloader_mtu(10), 576 - RTP_SEND_OVERHEAD);
        assert_eq!(payloader_mtu(0), 576 - RTP_SEND_OVERHEAD);
        assert_eq!(payloader_mtu(9000), 1436 - RTP_SEND_OVERHEAD);
    }

    #[test]
    fn every_video_payloader_gets_the_rtp_mtu_budget() {
        gst::init().unwrap();
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            if gst::ElementFactory::find(codec.rtp_payloader()).is_none() {
                // rtpav1pay ships in gst-plugins-rs (`rsrtp`).
                eprintln!("skipping {}: not in this GStreamer", codec.rtp_payloader());
                continue;
            }
            let chain = build_bitstream_chain(codec, None, 1000).expect("chain");
            assert_eq!(
                chain.payloader.property::<u32>("mtu"),
                payloader_mtu(1000),
                "{}",
                codec.rtp_payloader()
            );
        }
    }
}
