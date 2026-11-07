//! #1134: a hop that drops full-size packets shrinks the session's packets live, and a
//! path that loses packets of every size does not.

use super::*;
use crate::session::pipeline::codec_chain::payloader_mtu;
use crate::session::pipeline::mtu_shrink::nack_seqs;
use crate::session::pipeline::resize::force_idr;

/// The path between host and peer: what reaches the video transport's `srtpenc` is
/// dropped when `lose(size, n)` says so, `n` counting every packet. -> how many were
/// dropped, and how many over 1100 bytes were sent.
fn lossy_path(host: &Host, lose: fn(usize, u64) -> bool) -> (Arc<AtomicU64>, Arc<AtomicU64>) {
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
    let (dropped, large) = (Arc::new(AtomicU64::new(0)), Arc::new(AtomicU64::new(0)));
    let (count, large_count, seen) = (dropped.clone(), large.clone(), AtomicU64::new(0));
    pad.add_probe(gst::PadProbeType::BUFFER, move |_, info| {
        let size = info.buffer().map_or(0, |b| b.size());
        if size > 1100 {
            large_count.fetch_add(1, Ordering::Relaxed);
        }
        if lose(size, seen.fetch_add(1, Ordering::Relaxed)) {
            count.fetch_add(1, Ordering::Relaxed);
            return gst::PadProbeReturn::Drop;
        }
        gst::PadProbeReturn::Ok
    });
    (dropped, large)
}

/// Sequence numbers the peer NACKed to the host's video send session.
fn nacks(host: &Host) -> Arc<AtomicU64> {
    let rtpbin = host
        .transports
        .video
        .webrtc
        .downcast_ref::<gst::Bin>()
        .and_then(|b| b.by_name("rtpbin"))
        .expect("rtpbin");
    let session = rtpbin.emit_by_name::<glib::Object>("get-internal-session", &[&0u32]);
    let nacks = Arc::new(AtomicU64::new(0));
    let count = nacks.clone();
    session.connect("on-feedback-rtcp", false, move |v| {
        if (v[1].get::<u32>().ok()?, v[2].get::<u32>().ok()?) == (205, 1) {
            let fci = v[5].get::<Option<gst::Buffer>>().ok()??;
            let fci = fci.map_readable().ok()?;
            count.fetch_add(nack_seqs(&fci).count() as u64, Ordering::Relaxed);
        }
        None
    });
    nacks
}

/// A peer whose video jitter buffer NACKs what it misses, as a browser's does.
fn nacking_peer() -> Peer {
    let peer = Peer::new();
    peer.video.webrtc.connect("on-new-transceiver", false, |v| {
        v[1].get::<glib::Object>()
            .ok()?
            .set_property("do-nack", true);
        None
    });
    peer
}

/// A browser that decodes nothing asks for keyframe after keyframe; the test peer decodes
/// nothing and asks for none, so the test asks. Often, because the test pattern's
/// keyframes are a few packets where a game's are dozens.
fn keyframe_storm(encoder: &gst::Element, next: &mut Instant) {
    if Instant::now() >= *next {
        force_idr(encoder);
        *next += Duration::from_millis(20);
    }
}

fn streaming_host(tag: &str) -> (Host, Peer, gst::Element) {
    let mut cfg = host_cfg(false);
    cfg.stream.width = 1280;
    cfg.stream.height = 720;
    cfg.stream.fps = 60;
    cfg.stream.bitrate_kbps = 8000;
    cfg.rtp_mtu = 1200;
    cfg.rtp_mtu_shrink = true;
    let mut host = Host::start_cfg(tag, cfg);
    let pay = host
        .pipeline
        .by_name(super::super::VIDEO_PAYLOADER_NAME)
        .expect("video payloader");
    let mut peer = nacking_peer();
    assert!(
        pump(&mut host, &mut peer, Duration::from_secs(20), |_, p| {
            streaming(p)
        }),
        "the peer never received media: {}",
        describe(&peer)
    );
    (host, peer, pay)
}

#[test]
fn a_hop_that_drops_full_size_packets_shrinks_them_and_video_comes_back() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (mut host, mut peer, pay) = streaming_host("shrink-hop");
    let nacks = nacks(&host);
    let (hop, _) = lossy_path(&host, |size, _| size > 1100);

    let start = Instant::now();
    let mut next = start;
    let stepped = pump(&mut host, &mut peer, Duration::from_secs(10), |h, _| {
        keyframe_storm(&h.encoder, &mut next);
        pay.property::<u32>("mtu") == payloader_mtu(1100)
    });
    let took = start.elapsed();
    assert!(
        stepped,
        "no step to 1100 in 10 s: payloader mtu {}, {} NACKed, {} packets dropped",
        pay.property::<u32>("mtu"),
        nacks.load(Ordering::Relaxed),
        hop.load(Ordering::Relaxed)
    );

    // Retransmissions of packets sent before the step are still full-size; let them go.
    pump(&mut host, &mut peer, Duration::from_secs(1), |h, _| {
        keyframe_storm(&h.encoder, &mut next);
        false
    });
    let (dropped, received) = (hop.load(Ordering::Relaxed), rtp(&peer.video));
    pump(&mut host, &mut peer, Duration::from_secs(2), |h, _| {
        keyframe_storm(&h.encoder, &mut next);
        false
    });
    let lost_after = hop.load(Ordering::Relaxed) - dropped;
    let received_after = rtp(&peer.video) - received;
    eprintln!(
        "MTU shrink: 1200 -> 1100 {took:?} after the hop appeared; {dropped} dropped before, \
         {lost_after} in the 2 s after, peer received {received_after}"
    );
    assert_eq!(lost_after, 0, "the hop still drops packets after the step");
    assert!(
        received_after > 120,
        "the peer received {received_after} packets in 2 s"
    );
    assert_eq!(
        pay.property::<u32>("mtu"),
        payloader_mtu(1100),
        "a path that carries 1100 is not shrunk further"
    );

    // The payloader outlives the transport: a takeover keeps the smaller packets.
    let _next = attach(&mut host);
    assert!(host.replaced.contains(&PcId::Video), "{:?}", host.replaced);
    assert_eq!(pay.property::<u32>("mtu"), payloader_mtu(1100));
}

#[test]
fn a_path_that_loses_every_size_does_not_shrink() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let (mut host, mut peer, pay) = streaming_host("shrink-uniform");
    let nacks = nacks(&host);
    // 30% of every size, spread by a multiplicative hash.
    let (path, large) = lossy_path(&host, |_, n| n.wrapping_mul(2_654_435_761) % 100 < 30);

    let mut next = Instant::now();
    pump(&mut host, &mut peer, Duration::from_secs(8), |h, _| {
        keyframe_storm(&h.encoder, &mut next);
        false
    });
    let (nacks, dropped) = (nacks.load(Ordering::Relaxed), path.load(Ordering::Relaxed));
    let large = large.load(Ordering::Relaxed);
    eprintln!("uniform loss: {dropped} dropped, {large} sent over 1100 bytes, {nacks} NACKed");
    // Twice a window's minimum of large packets every second: the decision had its say.
    assert!(
        nacks > 200 && large > 8 * 40,
        "too little to decide on ({nacks} NACKed, {large} large packets): the test proves nothing"
    );
    assert_eq!(pay.property::<u32>("mtu"), payloader_mtu(1200));
}
