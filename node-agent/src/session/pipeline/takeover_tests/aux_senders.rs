//! The elements the host hands `webrtcbin` from its two aux-sender signals die with the
//! session. `twenty_takeovers_leak_nothing` cannot see one that does not: a leaked aux
//! sender keeps no `webrtcbin`, pipeline child or thread alive.

use super::*;

/// The video transport's `rtpgccbwe` (`request-aux-sender`, `abr_glue.rs`) and its
/// abs-capture-time `identity` (`request-post-rtp-aux-sender`, `rtp_ext.rs`).
fn aux_senders(host: &Host, transport: &str) -> Vec<(String, glib::WeakRef<gst::Element>)> {
    let webrtc = host.transports.video.webrtc.clone();
    let webrtc = webrtc.downcast::<gst::Bin>().expect("webrtcbin is a bin");
    ["identity", "rtpgccbwe"]
        .into_iter()
        .map(|factory| {
            let made: Vec<_> = webrtc
                .iterate_recurse()
                .into_iter()
                .flatten()
                .filter(|e| e.factory().is_some_and(|f| f.name() == factory))
                .collect();
            // Exactly one, or the check below proves nothing.
            assert_eq!(made.len(), 1, "{factory} in the {transport} transport");
            let name = format!("{factory} of the {transport} transport");
            (name, made[0].downgrade())
        })
        .collect()
}

#[test]
fn the_aux_senders_of_every_transport_die_with_the_session() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    for session in 0..3 {
        let mut host = Host::start(&format!("aux-{session}"));
        let first = first_attach(&mut host);
        let mut held = aux_senders(&host, "launch");
        let second = attach(&mut host);
        first.stop();
        assert_eq!(host.replaced.len(), 2, "the takeover replaced no transport");
        let replacement = aux_senders(&host, "replacement");
        // "Every transport": the takeover built a pair of its own, not the launch pair again.
        for ((name, new), (_, old)) in replacement.iter().zip(&held) {
            assert_ne!(new.upgrade(), old.upgrade(), "{name} is the launch one");
        }
        held.extend(replacement);
        drop((first, second));
        // The runner's `MetricsHookGuard`: the drain hook holds the `rtpgccbwe`.
        host.metrics.clear_hooks();
        drop(host);
        let alive = || {
            held.iter()
                .filter(|(_, element)| element.upgrade().is_some())
                .map(|(name, _)| name.as_str())
                .collect::<Vec<_>>()
        };
        let deadline = Instant::now() + Duration::from_secs(2);
        while !alive().is_empty() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(20));
        }
        assert_eq!(
            alive(),
            Vec::<&str>::new(),
            "session {session}: these outlived it"
        );
    }
}
