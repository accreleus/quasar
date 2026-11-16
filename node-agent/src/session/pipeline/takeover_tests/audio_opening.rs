//! #1441: a peer that connects late must not get one stale audio packet ahead of a gap.

use super::*;

/// RTP timestamp ticks in 100 ms at 48 kHz: ten Opus packets.
const MAX_STEP: u32 = 4_800;

/// `(seq, timestamp)` of each audio RTP packet the host's ICE sink sends. SRTP leaves the
/// RTP header in the clear; RTCP (`byte 1 & 0x7f` is 72..=76) is skipped by its type.
fn record_sent_audio(host: &Host) -> Arc<Mutex<Vec<(u16, u32)>>> {
    let sent = Arc::new(Mutex::new(Vec::new()));
    let webrtc = host
        .transports
        .audio_webrtc
        .clone()
        .expect("audio PC")
        .downcast::<gst::Bin>()
        .expect("webrtcbin is a bin");
    let probed = Arc::new(AtomicBool::new(false));
    let attach = {
        let (sent, probed) = (sent.clone(), probed.clone());
        move |element: &gst::Element| {
            if element.factory().is_none_or(|f| f.name() != "nicesink")
                || probed.swap(true, Ordering::SeqCst)
            {
                return;
            }
            let sent = sent.clone();
            element.static_pad("sink").unwrap().add_probe(
                gst::PadProbeType::BUFFER,
                move |_, info| {
                    let Some(buffer) = info.buffer() else {
                        return gst::PadProbeReturn::Ok;
                    };
                    let map = buffer.map_readable().unwrap();
                    if map.len() >= 12 && map[0] >> 6 == 2 && map[1] & 0x7f == 111 {
                        let seq = u16::from_be_bytes([map[2], map[3]]);
                        let ts = u32::from_be_bytes([map[4], map[5], map[6], map[7]]);
                        sent.lock().unwrap().push((seq, ts));
                    }
                    gst::PadProbeReturn::Ok
                },
            );
        }
    };
    let on_added = attach.clone();
    webrtc.connect_deep_element_added(move |_, _, element| on_added(element));
    // The offer, and its transport, may already exist.
    for element in webrtc.iterate_recurse().into_iter().flatten() {
        attach(&element);
    }
    sent
}

#[test]
fn a_late_peer_gets_continuous_audio_from_its_first_packet() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut host = Host::start("audio-opening");
    let sent = record_sent_audio(&host);
    // The page answers 1.5 s after the offers: well past the send queue's 200 ms.
    std::thread::sleep(Duration::from_millis(1_500));
    let mut peer = Peer::new();
    let up = pump(&mut host, &mut peer, Duration::from_secs(20), |_, p| {
        rtp(&p.audio) > 30
    });
    assert!(up, "the peer never received audio: {}", describe(&peer));
    let sent = sent.lock().unwrap().clone();
    assert!(sent.len() > 30, "only {} audio packets sent", sent.len());
    for (i, pair) in sent.windows(2).enumerate().take(30) {
        let ((s0, t0), (s1, t1)) = (pair[0], pair[1]);
        assert!(
            t1.wrapping_sub(t0) <= MAX_STEP,
            "audio packet {i} (seq {s0}) is {} ms ahead of the next (seq {s1}, {} sequence \
             numbers on): the opening packet is stale",
            t1.wrapping_sub(t0) / 48,
            s1.wrapping_sub(s0)
        );
    }
}
