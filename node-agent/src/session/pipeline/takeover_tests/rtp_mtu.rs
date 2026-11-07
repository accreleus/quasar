//! #1131: no RTP packet the host sends is larger than `QUASAR_RTP_MTU`, measured where
//! `srtpenc` takes it, after every header extension, RTX and RED/ULPFEC, and a live change
//! of it takes effect.

use std::collections::BTreeMap;

use super::*;
use crate::session::fec::FecMode;
use crate::session::pipeline::codec_chain::{payloader_mtu, set_rtp_mtu};

/// The largest packet of one kind, and which packet it was.
#[derive(Clone, Copy, Debug)]
struct Largest {
    size: usize,
    seq: u16,
    ssrc: u32,
}

/// Keyed by payload type; a RED packet by `red/<the PT it carries>`, so "97/98" is RED
/// carrying ULPFEC.
type Seen = Arc<Mutex<BTreeMap<String, Largest>>>;

/// Watch what reaches the video transport's `srtpenc`. -> what was seen, the latest
/// `media` packet of at least `full` bytes (still in `rtprtxsend`'s short history), and the
/// pad to send a retransmission request upstream from.
fn largest_before_srtp(
    host: &Host,
    red_pt: Option<u8>,
    media: &'static str,
    full: usize,
) -> (Seen, Arc<Mutex<Option<Largest>>>, gst::Pad) {
    let webrtc = host.transports.video.webrtc.clone();
    let webrtc = webrtc.downcast::<gst::Bin>().expect("webrtcbin is a bin");
    let srtpenc = webrtc
        .iterate_recurse()
        .into_iter()
        .flatten()
        .find(|e| e.factory().is_some_and(|f| f.name() == "srtpenc"))
        .expect("srtpenc in the video transport");
    let pad = srtpenc
        .sink_pads()
        .into_iter()
        .find(|p| p.name().starts_with("rtp_sink"))
        .expect("srtpenc has an RTP input");
    let seen: Seen = Arc::new(Mutex::new(BTreeMap::new()));
    let latest = Arc::new(Mutex::new(None));
    let (record_into, latest_into) = (seen.clone(), latest.clone());
    pad.add_probe(
        gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
        move |_, info| {
            let record = |b: &gst::BufferRef| {
                let Ok(rtp) = gstreamer_rtp::RTPBuffer::from_buffer_readable(b) else {
                    return;
                };
                let pt = rtp.payload_type();
                let key = match (Some(pt) == red_pt, rtp.payload()) {
                    // A primary-only RED block header: F=0, then the PT it carries.
                    (true, Ok(p)) if !p.is_empty() => format!("{pt}/{}", p[0] & 0x7f),
                    _ => pt.to_string(),
                };
                let this = Largest {
                    size: b.size(),
                    seq: rtp.seq(),
                    ssrc: rtp.ssrc(),
                };
                if key == media && this.size >= full {
                    *latest_into.lock().unwrap() = Some(this);
                }
                let mut seen = record_into.lock().unwrap();
                let at = seen.entry(key).or_insert(this);
                if this.size >= at.size {
                    *at = this;
                }
            };
            match &info.data {
                Some(gst::PadProbeData::Buffer(b)) => record(b),
                Some(gst::PadProbeData::BufferList(l)) => l.iter().for_each(record),
                _ => {}
            }
            gst::PadProbeReturn::Ok
        },
    );
    (seen, latest, pad)
}

/// What a NACK becomes inside `rtpbin`: the request `rtprtxsend` answers with an RTX
/// packet. Sent from where `srtpenc` reads, so it travels up the whole send path.
fn request_retransmission(srtp_input: &gst::Pad, of: Largest) {
    let request = gst::Structure::builder("GstRTPRetransmissionRequest")
        .field("seqnum", u32::from(of.seq))
        .field("ssrc", of.ssrc)
        .build();
    let upstream = srtp_input.peer().expect("srtpenc's input is linked");
    upstream.send_event(gst::event::CustomUpstream::new(request));
}

// The answers a browser gives these offers. The test peer's own answer takes only H.264,
// and then the host sends no RTX and no FEC. The payload types are the host's offer's
// (checked in the test).

fn accept_rtx(sdp: String) -> String {
    sdp.replace("RTP/SAVPF 96\r\n", "RTP/SAVPF 96 97\r\n")
        .replace(
            "a=rtpmap:96 H264/90000\r\n",
            "a=rtpmap:96 H264/90000\r\na=rtpmap:97 rtx/90000\r\na=fmtp:97 apt=96\r\n",
        )
}

fn accept_red_ulpfec_rtx(sdp: String) -> String {
    sdp.replace("RTP/SAVPF 96\r\n", "RTP/SAVPF 96 97 98 99 100\r\n")
        .replace(
            "a=rtpmap:96 H264/90000\r\n",
            "a=rtpmap:96 H264/90000\r\na=rtpmap:97 red/90000\r\na=rtpmap:98 ulpfec/90000\r\n\
             a=rtpmap:99 rtx/90000\r\na=fmtp:99 apt=96\r\na=rtpmap:100 rtx/90000\r\n\
             a=fmtp:100 apt=97\r\n",
        )
}

fn keyframes_through(host: &mut Host, peer: &mut Peer) {
    for _ in 0..3 {
        super::super::resize::force_idr(&host.encoder);
        pump(host, peer, Duration::from_millis(700), |_, _| false);
    }
}

fn largest_of(seen: &BTreeMap<String, Largest>) -> usize {
    seen.values().map(|l| l.size).max().unwrap_or(0)
}

#[test]
fn no_rtp_packet_is_larger_than_the_rtp_mtu() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    // The default, the default with FEC, and the floor with FEC.
    let floor = *crate::session::RTP_MTU_RANGE.start();
    for (fec, rtp_mtu) in [(false, 1200), (true, 1200), (true, floor)] {
        let what = format!("fec={fec} rtp_mtu={rtp_mtu}");
        let mut cfg = host_cfg(false);
        cfg.stream.width = 640;
        cfg.stream.height = 480;
        cfg.rtp_mtu = rtp_mtu;
        if fec {
            cfg.fec_percentage = 100;
            cfg.fec_mode = FecMode::Fixed;
        }
        let mut host = Host::start_cfg(&format!("mtu-{fec}-{rtp_mtu}"), cfg);
        host.munge_answer = Some(if fec {
            accept_red_ulpfec_rtx
        } else {
            accept_rtx
        });
        let pay = host
            .pipeline
            .by_name(super::super::VIDEO_PAYLOADER_NAME)
            .expect("video payloader");
        assert_eq!(pay.property::<u32>("mtu"), payloader_mtu(rtp_mtu), "{what}");

        // The peer drops RED and RTX it never agreed to, so wait for the transport, not
        // its media.
        let mut peer = Peer::new();
        assert!(
            pump(&mut host, &mut peer, Duration::from_secs(20), |_, p| {
                p.video.dtls_state() == "connected"
            }),
            "{what}: the peer never connected: {}",
            describe(&peer)
        );
        let offer = video_sdp(&host, "offer", 0);
        let offered: &[&str] = if fec {
            &[
                "a=rtpmap:97 red/90000",
                "a=rtpmap:98 ulpfec/90000",
                "a=fmtp:99 apt=96",
                "a=fmtp:100 apt=97",
            ]
        } else {
            &["a=fmtp:97 apt=96"]
        };
        assert!(
            offered.iter().all(|l| offer.contains(l)),
            "{what}: the munged answer's payload types no longer match the offer:\n{offer}"
        );

        let media = if fec { "97/96" } else { "96" };
        let full = payloader_mtu(rtp_mtu) as usize;
        let (seen, latest, srtp_input) = largest_before_srtp(&host, fec.then_some(97), media, full);
        keyframes_through(&mut host, &mut peer);

        // A retransmission of a full-size media packet from the keyframe just sent.
        super::super::resize::force_idr(&host.encoder);
        pump(&mut host, &mut peer, Duration::from_millis(150), |_, _| {
            false
        });
        let of = latest.lock().unwrap().unwrap_or_else(|| {
            panic!(
                "{what}: no full-size {media} packet: {:?}",
                seen.lock().unwrap()
            )
        });
        request_retransmission(&srtp_input, of);
        pump(&mut host, &mut peer, Duration::from_millis(500), |_, _| {
            false
        });

        let largest = seen.lock().unwrap().clone();
        eprintln!("MTU {what}: {largest:?}");
        let max = largest_of(&largest);
        assert!(
            max <= rtp_mtu as usize,
            "{what}: an RTP packet of {max} bytes is over QUASAR_RTP_MTU: {largest:?}"
        );
        let rtx: &[&str] = if fec { &["99", "100"] } else { &["97"] };
        assert!(
            rtx.iter().any(|pt| largest.contains_key(*pt)),
            "{what}: no RTX packet reached srtpenc: {largest:?}"
        );
        if fec {
            let fec_size = largest.get("97/98").map_or(0, |l| l.size);
            assert!(
                fec_size >= payloader_mtu(rtp_mtu) as usize,
                "{what}: no full-size ULPFEC packet (largest {fec_size}): {largest:?}"
            );
        }

        // #1134 shrinks the packets of a live session through the same setter.
        if !fec {
            set_rtp_mtu(&pay, 1000);
            seen.lock().unwrap().clear();
            keyframes_through(&mut host, &mut peer);
            let largest = seen.lock().unwrap().clone();
            let max = largest_of(&largest);
            assert!(
                (payloader_mtu(1000) as usize..=1000).contains(&max),
                "after a live change to 1000 the largest packet is {max}: {largest:?}"
            );
        }
    }
}
