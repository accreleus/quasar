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

/// The RTP payloader for `codec`, configured for low-latency WebRTC. `rtp_mtu` is the
/// session's `QUASAR_RTP_MTU` ([`set_rtp_mtu`]).
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
/// with RED/ULPFEC (a full-size ULPFEC packet). An RTX packet measured +18 (+11 for RED),
/// below its original. The rest covers ULPFEC's long mask (4). Guarded by
/// `takeover_tests::rtp_mtu::no_rtp_packet_is_larger_than_the_rtp_mtu`.
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
        assert_eq!(payloader_mtu(10), 900 - RTP_SEND_OVERHEAD);
        assert_eq!(payloader_mtu(0), 900 - RTP_SEND_OVERHEAD);
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
            let payloader = build_video_payloader(codec, 1000).expect("payloader");
            assert_eq!(
                payloader.property::<u32>("mtu"),
                payloader_mtu(1000),
                "{}",
                codec.rtp_payloader()
            );
        }
    }

    /// The part of [`RTP_SEND_OVERHEAD`] a payloader writes itself: the TWCC +
    /// abs-capture-time block (4 + 3 + 9 bytes). Everything added later (abs-capture-time
    /// again at egress, RTX, RED/ULPFEC) acts on finished RTP packets, the same for every
    /// codec, and is measured by `takeover_tests::rtp_mtu`.
    const PAYLOADER_EXTENSION_BYTES: usize = 16;

    /// One access unit, keyframe first: what each payloader is fed. Synthetic, because the
    /// test images have no software H.265 or AV1 encoder; the payloaders split on NAL or
    /// OBU boundaries and do not read slice or frame data.
    fn access_unit(codec: Codec) -> (gst::Caps, Vec<u8>) {
        let body = vec![0x55u8; 30_000];
        let mut au = Vec::new();
        match codec {
            Codec::H264 | Codec::H265 => {
                // H.264: an openh264 SPS and PPS, then an IDR slice. H.265: VPS, SPS, PPS,
                // then an IDR_W_RADL slice (NAL header bytes only).
                let nals: Vec<Vec<u8>> = if codec == Codec::H264 {
                    vec![
                        vec![
                            0x67, 0x42, 0xc0, 0x1e, 0x8c, 0x68, 0x0a, 0x03, 0xda, 0x01, 0xe1, 0x10,
                            0x8d, 0x40,
                        ],
                        vec![0x68, 0xce, 0x3c, 0x80],
                        [&[0x65][..], &body].concat(),
                    ]
                } else {
                    vec![
                        vec![0x40, 0x01, 0x0c],
                        vec![0x42, 0x01, 0x01],
                        vec![0x44, 0x01, 0xc0],
                        [&[0x26, 0x01][..], &body].concat(),
                    ]
                };
                for nal in nals {
                    au.extend_from_slice(&[0, 0, 0, 1]);
                    au.extend_from_slice(&nal);
                }
                let caps = gst::Caps::builder(codec.caps_name())
                    .field("stream-format", "byte-stream")
                    .field("alignment", "au")
                    .build();
                (caps, au)
            }
            Codec::Av1 => {
                // A temporal delimiter, then one frame OBU with its LEB128 size.
                au.extend_from_slice(&[0x12, 0x00, 0x32]);
                let mut n = body.len();
                loop {
                    let byte = (n & 0x7f) as u8;
                    n >>= 7;
                    au.push(if n > 0 { byte | 0x80 } else { byte });
                    if n == 0 {
                        break;
                    }
                }
                au.extend_from_slice(&body);
                let caps = gst::Caps::builder("video/x-av1")
                    .field("parsed", true)
                    .field("stream-format", "obu-stream")
                    .field("alignment", "tu")
                    .build();
                (caps, au)
            }
        }
    }

    /// Seat A's per-codec check (#1131): `rtph265pay` and `rtpav1pay` (`RtpBasePay2`) are
    /// other implementations than `rtph264pay`, so each one's own overshoot is measured.
    #[test]
    fn no_payloader_writes_more_than_the_extension_block_past_its_mtu() {
        gst::init().unwrap();
        for codec in [Codec::H264, Codec::H265, Codec::Av1] {
            let name = codec.rtp_payloader();
            if gst::ElementFactory::find(name).is_none() {
                eprintln!("skipping {name}: not in this GStreamer");
                continue;
            }
            let (caps, au) = access_unit(codec);
            let src = gstreamer_app::AppSrc::builder()
                .caps(&caps)
                .format(gst::Format::Time)
                .build();
            let pay = build_video_payloader(codec, 1200).expect("payloader");
            super::super::rtp_ext::attach_abs_capture_time_probe(&pay);
            let rtp_caps = gst::ElementFactory::make("capsfilter")
                .property(
                    "caps",
                    gst::Caps::builder("application/x-rtp")
                        .field("media", "video")
                        .field("encoding-name", codec.rtp_encoding_name())
                        .field("payload", 96i32)
                        .field("clock-rate", 90_000i32)
                        .field(
                            format!("extmap-{}", super::super::ABS_CAPTURE_TIME_EXT_ID),
                            super::super::ABS_CAPTURE_TIME_URI,
                        )
                        .field(
                            format!("extmap-{}", super::super::TWCC_EXT_ID),
                            super::super::RTP_TWCC_URI,
                        )
                        .build(),
                )
                .build()
                .unwrap();
            let sink = gstreamer_app::AppSink::builder().sync(false).build();
            let pipeline = gst::Pipeline::new();
            pipeline
                .add_many([src.upcast_ref(), &pay, &rtp_caps, sink.upcast_ref()])
                .unwrap();
            gst::Element::link_many([src.upcast_ref(), &pay, &rtp_caps, sink.upcast_ref()])
                .unwrap();
            pipeline.set_state(gst::State::Playing).unwrap();
            for i in 0..3u64 {
                let mut buffer = gst::Buffer::from_slice(au.clone());
                let b = buffer.get_mut().unwrap();
                b.set_pts(gst::ClockTime::from_mseconds(i * 16));
                if i > 0 {
                    b.set_flags(gst::BufferFlags::DELTA_UNIT);
                }
                src.push_buffer(buffer).unwrap();
            }
            src.end_of_stream().unwrap();
            let mut sizes = Vec::new();
            while let Ok(sample) = sink.pull_sample() {
                sizes.push(sample.buffer().map_or(0, |b| b.size()));
            }
            pipeline.set_state(gst::State::Null).unwrap();
            let max = sizes.iter().copied().max().unwrap_or(0);
            let mtu = payloader_mtu(1200) as usize;
            eprintln!(
                "{name}: {} packets, largest {max} at mtu {mtu}",
                sizes.len()
            );
            assert!(
                (mtu..=mtu + PAYLOADER_EXTENSION_BYTES).contains(&max),
                "{name}: largest packet {max} at mtu {mtu}"
            );
        }
    }
}
