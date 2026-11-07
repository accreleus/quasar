//! #1131: no RTP packet the host sends is larger than `QUASAR_RTP_MTU`, measured where
//! `srtpenc` takes it, after every header extension, RTX and RED/ULPFEC, and a live change
//! of it takes effect.

use super::*;
use crate::session::fec::FecMode;
use crate::session::pipeline::codec_chain::{payloader_mtu, set_rtp_mtu};

/// The largest RTP packet, per payload type, reaching the video transport's `srtpenc`.
fn largest_before_srtp(host: &Host) -> Arc<Mutex<std::collections::BTreeMap<u8, usize>>> {
    let webrtc = host.transports.video.webrtc.clone();
    let webrtc = webrtc.downcast::<gst::Bin>().expect("webrtcbin is a bin");
    let srtpenc = webrtc
        .iterate_recurse()
        .into_iter()
        .flatten()
        .find(|e| e.factory().is_some_and(|f| f.name() == "srtpenc"))
        .expect("srtpenc in the video transport");
    let largest = Arc::new(Mutex::new(std::collections::BTreeMap::new()));
    for pad in srtpenc.sink_pads() {
        if !pad.name().starts_with("rtp_sink") {
            continue;
        }
        let largest = largest.clone();
        pad.add_probe(
            gst::PadProbeType::BUFFER | gst::PadProbeType::BUFFER_LIST,
            move |_, info| {
                let record = |b: &gst::BufferRef| {
                    let pt = gstreamer_rtp::RTPBuffer::from_buffer_readable(b)
                        .map(|rtp| rtp.payload_type())
                        .unwrap_or(0);
                    let mut largest = largest.lock().unwrap();
                    let max = largest.entry(pt).or_insert(0);
                    *max = (*max).max(b.size());
                };
                match &info.data {
                    Some(gst::PadProbeData::Buffer(b)) => record(b),
                    Some(gst::PadProbeData::BufferList(l)) => l.iter().for_each(record),
                    _ => {}
                }
                gst::PadProbeReturn::Ok
            },
        );
    }
    largest
}

/// The answer a browser gives an offer with RED/ULPFEC: it takes them. The test peer's
/// own answer takes only H.264, and then the host sends no FEC.
fn accept_red_ulpfec(sdp: String) -> String {
    sdp.replace("RTP/SAVPF 96\r\n", "RTP/SAVPF 96 97 98\r\n")
        .replace(
            "a=rtpmap:96 H264/90000\r\n",
            "a=rtpmap:96 H264/90000\r\na=rtpmap:97 red/90000\r\na=rtpmap:98 ulpfec/90000\r\n",
        )
}

fn keyframes_through(host: &mut Host, peer: &mut Peer) {
    for _ in 0..3 {
        super::super::resize::force_idr(&host.encoder);
        pump(host, peer, Duration::from_millis(700), |_, _| false);
    }
}

#[test]
fn no_rtp_packet_is_larger_than_the_rtp_mtu() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    for fec in [false, true] {
        let mut cfg = host_cfg(false);
        cfg.stream.width = 640;
        cfg.stream.height = 480;
        if fec {
            cfg.fec_percentage = 100;
            cfg.fec_mode = FecMode::Fixed;
        }
        let rtp_mtu = cfg.rtp_mtu;
        let mut host = Host::start_cfg(if fec { "mtu-fec" } else { "mtu" }, cfg);
        if fec {
            host.munge_answer = Some(accept_red_ulpfec);
        }
        let pay = host
            .pipeline
            .by_name(super::super::VIDEO_PAYLOADER_NAME)
            .expect("video payloader");
        assert_eq!(pay.property::<u32>("mtu"), payloader_mtu(rtp_mtu));

        // The peer drops RED it never agreed to, so wait for the transport, not its media.
        let mut peer = Peer::new();
        assert!(
            pump(&mut host, &mut peer, Duration::from_secs(20), |_, p| {
                p.video.dtls_state() == "connected"
            }),
            "fec={fec}: the peer never connected: {}",
            describe(&peer)
        );
        if fec {
            let offer = video_sdp(&host, "offer", 0);
            assert!(
                offer.contains("a=rtpmap:97 red/90000")
                    && offer.contains("a=rtpmap:98 ulpfec/90000"),
                "the munged answer's payload types no longer match the offer"
            );
        }
        let seen = largest_before_srtp(&host);
        keyframes_through(&mut host, &mut peer);

        let largest = seen.lock().unwrap().clone();
        eprintln!("MTU fec={fec} rtp_mtu={rtp_mtu} largest per PT: {largest:?}");
        let max = largest.values().copied().max().unwrap_or(0);
        assert!(
            max <= rtp_mtu as usize,
            "fec={fec}: an RTP packet of {max} bytes is over QUASAR_RTP_MTU={rtp_mtu}: {largest:?}"
        );
        if fec {
            assert!(
                largest.keys().any(|&pt| pt != 96),
                "fec: no RED packet reached srtpenc: {largest:?}"
            );
        }
        // Full packets were sent, or the bound above proves nothing.
        assert!(
            max >= payloader_mtu(rtp_mtu) as usize,
            "fec={fec}: no full-size packet seen (largest {max}): {largest:?}"
        );

        // #1134 shrinks the packets of a live session through the same setter.
        if !fec {
            set_rtp_mtu(&pay, 900);
            seen.lock().unwrap().clear();
            keyframes_through(&mut host, &mut peer);
            let largest = seen.lock().unwrap().clone();
            let max = largest.values().copied().max().unwrap_or(0);
            assert!(
                (payloader_mtu(900) as usize..=900).contains(&max),
                "after a live change to 900 the largest packet is {max}: {largest:?}"
            );
        }
    }
}
