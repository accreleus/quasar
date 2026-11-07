//! Codec-dispatched bitstream chain: the encoder-output capsfilter → parser pair, which
//! ends the encode chain at the encoded-frame boundary, plus the WebRTC transport's RTP
//! payloader for the same codec. The RTP caps stay in the builders (they differ between
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

/// The encoder-output capsfilter and bitstream parser for a codec. The caller adds both
/// to the pipeline after the encoder; the parser's src pad is the encoded-frame boundary.
pub(super) struct BitstreamChain {
    /// `video/x-{codec}[,profile=…]`. Drives the hardware encoder's output profile
    /// (VA/NVENC/Vulkan expose no `profile` property, so downstream caps select it)
    /// and, for h264/h265, lands in the SDP offer — so it must equal the real
    /// bitstream.
    pub encoder_capsfilter: gst::Element,
    pub parser: gst::Element,
}

/// Build the [`BitstreamChain`] for `codec`. `profile` is the caps `profile` string
/// (from [`super::caps::caps_profile`]) — `Some` for h264/h265, `None` for av1
/// (no profile caps field).
pub(super) fn build_bitstream_chain(codec: Codec, profile: Option<&str>) -> Result<BitstreamChain> {
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

    Ok(BitstreamChain {
        encoder_capsfilter,
        parser,
    })
}

/// The RTP payloader for `codec`, configured for low-latency WebRTC.
/// `rtp_mtu` is the session's `QUASAR_RTP_MTU` ([`set_rtp_mtu`]).
pub(super) fn build_video_payloader(codec: Codec, rtp_mtu: u32) -> Result<gst::Element> {
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
    Ok(payloader)
}

/// Bytes the send path adds to a packet after the payloader's `mtu`, which counts no
/// header extension. Measured at `srtpenc` with a 1100-byte payloader `mtu`: +28 without
/// FEC (abs-capture-time, written by the payloader and again at egress, plus TWCC), +39
/// with RED/ULPFEC. The rest covers RTX's 2-byte sequence number and ULPFEC's long mask
/// (4). Guarded by `takeover_tests::rtp_mtu::no_rtp_packet_is_larger_than_the_rtp_mtu`.
const RTP_SEND_OVERHEAD: u32 = 48;

/// The payloader `mtu` that keeps every packet on the wire within `rtp_mtu` (#1131).
pub(super) fn payloader_mtu(rtp_mtu: u32) -> u32 {
    rtp_mtu - RTP_SEND_OVERHEAD
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
    fn every_video_payloader_gets_the_rtp_mtu_budget() {
        gst::init().unwrap();
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            if gst::ElementFactory::find(codec.rtp_payloader()).is_none() {
                // rtpav1pay ships in gst-plugins-rs (`rsrtp`).
                eprintln!("skipping {}: not in this GStreamer", codec.rtp_payloader());
                continue;
            }
            let payloader = build_video_payloader(codec, 1000).expect("payloader");
            assert_eq!(
                payloader.property::<u32>("mtu"),
                payloader_mtu(1000),
                "{}",
                codec.rtp_payloader()
            );
        }
    }
}
