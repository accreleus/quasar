//! Live external-resolution change support: the force-key-unit event, the one-shot
//! "renegotiation completed" pad probe, and the glue composing them into the resize
//! completion signal. Encoder selection and construction stay in `encoders.rs`.
//!
//! The keyframe half of the adaptive-external-resolution lever (spec D7,
//! `docs/superpowers/specs/2026-08-16-adaptive-external-resolution-design.md`):
//! [`super::scale_stage::ScaleStage::set_size`] re-sets the tail capsfilter's caps, the
//! graph renegotiates lazily, and [`arm_on_next_caps`] fires the forced IDR and the
//! metrics echo once it lands. Its ordering rules (arm before `set_size`, upstream vs
//! downstream pad direction) are load-bearing.

use gstreamer as gst;
use gstreamer::prelude::*;

/// The upstream force-key-unit event that makes an encoder emit an IDR on its next
/// frame. `all-headers=true` is load-bearing: without it the IDR carries no parameter
/// sets (SPS/PPS, VPS/SPS/PPS, or the AV1 sequence header) and a receiver just told of
/// a new frame size has nothing to reconfigure its decoder with. Separate from
/// [`force_idr`] so the event shape is testable without a live encoder.
pub fn force_key_unit_event() -> gst::Event {
    gstreamer_video::UpstreamForceKeyUnitEvent::builder()
        .all_headers(true)
        .build()
}

/// Make `encoder` produce an IDR with headers as soon as possible, so a receiver can
/// decode at a newly negotiated size without a stall. The parsers run
/// `config-interval=-1`, so the re-emitted headers reach the wire.
///
/// The event must go to the encoder's SRC pad: it is an upstream event, and
/// `GstVideoEncoder` intercepts force-key-unit in its src-event handler — pushed at the
/// sink pad it would travel past the encoder to the source. Best-effort, so a refusal
/// logs rather than failing the session.
pub fn force_idr(encoder: &gst::Element) {
    let Some(pad) = encoder.static_pad("src") else {
        tracing::warn!(
            token = "force-idr-no-src-pad",
            "force_idr: encoder has no src pad — no keyframe forced"
        );
        return;
    };
    if pad.send_event(force_key_unit_event()) {
        tracing::info!("force_idr: force-key-unit (all-headers) accepted by the encoder");
    } else {
        tracing::warn!(
            token = "force-idr-refused",
            "force_idr: the encoder refused the force-key-unit event — the receiver will \
             wait for the next scheduled IDR"
        );
    }
}

/// A one-shot [`arm_on_next_caps`] probe that has not fired. Dropping it does NOT
/// remove the probe — it must outlive the call that armed it; use [`Self::disarm`].
pub struct ArmedCapsProbe {
    pad: gst::Pad,
    id: gst::PadProbeId,
}

impl ArmedCapsProbe {
    /// Cancel an armed probe that will never fire (its `set_size` was a no-op).
    /// Otherwise it sits on the pad until teardown and fires on an unrelated
    /// renegotiation.
    pub fn disarm(self) {
        self.pad.remove_probe(self.id);
    }
}

/// Arm a ONE-SHOT probe firing `action(pad, negotiated_size)` on the next CAPS event to
/// reach the encoder's sink pad — the completion signal for a live resolution change.
/// Both things a resize owes hang off it:
///
/// 1. The IDR. `set_size` only re-sets the capsfilter's caps and the graph renegotiates
///    lazily, so a force-key-unit sent the instant `set_size` returns lands on a frame
///    still at the OLD size. From here the upstream event reaches the encoder before it
///    has handled the caps, and so before the first frame at the new size.
/// 2. The metrics echo, which must publish an OBSERVED size. Reading
///    `ScaleStage::current()` right after `set_size` is deterministically one step
///    stale, so a 1080→720 step would echo 1080 and a return-to-launch never at all.
///
/// Arm BEFORE `set_size`, never after: the streaming thread can push the renegotiating
/// buffer between the property set and the `add_probe`, and a probe armed afterwards
/// waits forever — no IDR, and a probe left on the pad.
///
/// If the renegotiation never happens (an encoder whose `set_format` rejects the size, a
/// convert element that cannot scale) no caps event arrives and the probe stays armed for
/// the session. That is deliberate: no keyframe is owed because nothing changed, the echo
/// keeps reporting what is really on the wire, and the failed negotiation surfaces as a
/// GStreamer error on the bus. The cost — a later unrelated renegotiation claiming the
/// stale arming — is harmless, and preferred to a timeout guessing how long a
/// renegotiation may legitimately take on a loaded encoder.
///
/// The closure must capture NO element: a strong clone of `encoder` in a probe on that
/// same encoder's pad is a GObject ref cycle GLib never collects (a per-session VRAM
/// leak). The encoder is recovered from the pad instead.
pub fn arm_on_next_caps<F>(encoder: &gst::Element, action: F) -> Option<ArmedCapsProbe>
where
    F: Fn(&gst::Pad, Option<(i32, i32)>) + Send + Sync + 'static,
{
    let pad = encoder.static_pad("sink")?;
    let id = on_next_caps(&pad, action)?;
    Some(ArmedCapsProbe { pad, id })
}

/// Install a one-shot pad probe running `action(pad, size)` on the next CAPS event, then
/// removing itself. Split out so "fire on the renegotiation, and only once" is testable
/// without an encoder.
fn on_next_caps<F>(pad: &gst::Pad, action: F) -> Option<gst::PadProbeId>
where
    F: Fn(&gst::Pad, Option<(i32, i32)>) + Send + Sync + 'static,
{
    pad.add_probe(gst::PadProbeType::EVENT_DOWNSTREAM, move |pad, info| {
        let Some(gst::PadProbeData::Event(e)) = &info.data else {
            return gst::PadProbeReturn::Ok;
        };
        let gst::EventView::Caps(caps_ev) = e.view() else {
            return gst::PadProbeReturn::Ok;
        };
        let size = caps_ev.caps().structure(0).and_then(|s| {
            let w: i32 = s.get("width").ok()?;
            let h: i32 = s.get("height").ok()?;
            Some((w, h))
        });
        action(pad, size);
        gst::PadProbeReturn::Remove
    })
}

/// The `action` half of a resize arming: force the IDR on the element that owns `pad`.
/// Exposed so the lever's own callback can compose it with the metrics echo.
pub fn force_idr_from_pad(pad: &gst::Pad) {
    match pad.parent_element() {
        Some(enc) => force_idr(&enc),
        // Unparented mid-teardown: nothing left to force.
        None => tracing::warn!(
            token = "resize-completion-pad-orphaned",
            "resize completion: the encoder sink pad lost its parent"
        ),
    }
}

#[cfg(test)]
mod tests {
    use super::{arm_on_next_caps, force_idr, force_key_unit_event, on_next_caps};
    use gstreamer as gst;
    use gstreamer::prelude::*;

    // ---- force_idr: the keyframe half of a live external-resolution change ----

    // Without all-headers the IDR carries no SPS/PPS and a receiver reconfiguring for a
    // new frame size has nothing to rebuild its decoder from.
    #[test]
    fn force_key_unit_event_sets_all_headers() {
        gst::init().unwrap();
        let event = force_key_unit_event();
        assert_eq!(event.type_(), gst::EventType::CustomUpstream);
        let s = event
            .structure()
            .expect("force-key-unit carries a structure");
        assert_eq!(s.name(), "GstForceKeyUnit");
        assert!(
            s.get::<bool>("all-headers").unwrap(),
            "all-headers must be true, got {s}"
        );
    }

    // A live encoder's src pad accepts it. Skips where openh264enc is absent.
    #[test]
    fn force_idr_is_accepted_by_a_live_encoder() {
        gst::init().unwrap();
        if gst::ElementFactory::find("openh264enc").is_none() {
            eprintln!(
                "SKIP force_idr_is_accepted_by_a_live_encoder: openh264enc not in this image"
            );
            return;
        }
        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let convert = gst::ElementFactory::make("videoconvert").build().unwrap();
        let enc = gst::ElementFactory::make("openh264enc").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline.add_many([&src, &convert, &enc, &sink]).unwrap();
        gst::Element::link_many([&src, &convert, &enc, &sink]).unwrap();
        pipeline.set_state(gst::State::Playing).unwrap();
        let _ = pipeline.state(gst::ClockTime::from_seconds(5));

        // `force_idr` is best-effort by contract, so assert on the pad send directly.
        let accepted = enc
            .static_pad("src")
            .unwrap()
            .send_event(force_key_unit_event());
        assert!(accepted, "openh264enc refused the upstream force-key-unit");
        // The helper must not panic on that element, nor on a pad-less one.
        force_idr(&enc);
        force_idr(&gst::ElementFactory::make("fakesink").build().unwrap());

        pipeline.set_state(gst::State::Null).unwrap();
    }

    // The probe must fire ONCE, on the caps event, reporting the negotiated size. Run on
    // an identity element so no encoder's behaviour is involved.
    #[test]
    fn one_shot_fires_once_on_the_caps_event_with_the_negotiated_size() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::{Arc, Mutex};
        gst::init().unwrap();

        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let filter = gst::ElementFactory::make("capsfilter")
            .property(
                "caps",
                gst::Caps::builder("video/x-raw")
                    .field("width", 640i32)
                    .field("height", 360i32)
                    .build(),
            )
            .build()
            .unwrap();
        let identity = gst::ElementFactory::make("identity").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline
            .add_many([&src, &filter, &identity, &sink])
            .unwrap();
        gst::Element::link_many([&src, &filter, &identity, &sink]).unwrap();

        let fires = Arc::new(AtomicU32::new(0));
        let seen: Arc<Mutex<Option<(i32, i32)>>> = Arc::new(Mutex::new(None));
        let (counter, sink_seen) = (fires.clone(), seen.clone());
        let pad = identity.static_pad("sink").unwrap();
        assert!(on_next_caps(&pad, move |_, size| {
            counter.fetch_add(1, Ordering::Relaxed);
            *sink_seen.lock().unwrap() = size;
        })
        .is_some());
        // Not fired yet: nothing has flowed.
        assert_eq!(fires.load(Ordering::Relaxed), 0);

        pipeline.set_state(gst::State::Playing).unwrap();
        let _ = pipeline.state(gst::ClockTime::from_seconds(5));
        for _ in 0..200 {
            if fires.load(Ordering::Relaxed) > 0 {
                break;
            }
            std::thread::sleep(std::time::Duration::from_millis(25));
        }
        // Many buffers have flowed by now.
        std::thread::sleep(std::time::Duration::from_millis(200));
        assert_eq!(
            fires.load(Ordering::Relaxed),
            1,
            "the probe must fire once and then remove itself"
        );
        assert_eq!(*seen.lock().unwrap(), Some((640, 360)));

        pipeline.set_state(gst::State::Null).unwrap();
    }

    // An arming whose resize was a no-op must be cancellable, or it sits on the pad and
    // fires on an unrelated later renegotiation.
    #[test]
    fn an_armed_probe_can_be_disarmed() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        gst::init().unwrap();

        let pipeline = gst::Pipeline::new();
        let src = gst::ElementFactory::make("videotestsrc")
            .property("is-live", true)
            .build()
            .unwrap();
        let identity = gst::ElementFactory::make("identity").build().unwrap();
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()
            .unwrap();
        pipeline.add_many([&src, &identity, &sink]).unwrap();
        gst::Element::link_many([&src, &identity, &sink]).unwrap();

        let fires = Arc::new(AtomicU32::new(0));
        let counter = fires.clone();
        let armed = arm_on_next_caps(&identity, move |_, _| {
            counter.fetch_add(1, Ordering::Relaxed);
        })
        .expect("identity has a sink pad");
        armed.disarm();

        pipeline.set_state(gst::State::Playing).unwrap();
        let _ = pipeline.state(gst::ClockTime::from_seconds(5));
        std::thread::sleep(std::time::Duration::from_millis(300));
        assert_eq!(
            fires.load(Ordering::Relaxed),
            0,
            "a disarmed probe must never fire"
        );

        pipeline.set_state(gst::State::Null).unwrap();
    }

    // A pad-less element must report failure rather than panic, so the caller can fall
    // back to an immediate IDR.
    #[test]
    fn arming_on_a_padless_element_reports_failure() {
        gst::init().unwrap();
        assert!(arm_on_next_caps(
            &gst::ElementFactory::make("videotestsrc").build().unwrap(),
            |_, _| {}
        )
        .is_none());
    }
}
