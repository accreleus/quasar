//! Reference recovery control of the patched Vulkan and NVENC encoders (#1348): ask for an
//! anchor, ask for the next frame to code from one, read which frames did. The encoder half
//! is `gstrefrecovery.h` in `deploy/patches/vulkan/*-ref-recovery.patch`; the names and
//! field types here must match it. The WebTransport repair loop is `webtransport::media`.

use gstreamer as gst;

/// Encoder property: anchors held, 0 = off. Present only on a patched encoder.
pub(crate) const NUM_ANCHORS: &str = "num-anchors";

/// The `num-anchors` the agent gives encoder `factory`, 0 unless `on`
/// (`QUASAR_WT_REF_RECOVERY`): the most the level leaves at 1080p60, the smallest of the common
/// sizes (H.264 2, HEVC 4; `deploy/patches/vulkan/README.md`). An encoder that clamps lower
/// holds fewer than the agent mirrors: a wrong mirror costs a key frame, never a wrong
/// reference. AV1 takes HEVC's 4: its levels bound no reference. NVENC H.264 stays 0 (key
/// frame repair) until a real-Chrome hardware row proves its gaps-flag-0 recovery; NVENC AV1
/// has no long-term references.
pub(crate) fn anchors_for(factory: &str, on: bool) -> u32 {
    if !on {
        return 0;
    }
    let vulkan = factory.starts_with("vulkan");
    let nvenc = factory.starts_with("nv");
    match ["h264", "h265", "av1"]
        .into_iter()
        .find(|c| factory.contains(c))
    {
        Some("h264") if vulkan => 2,
        Some("h265") if vulkan || nvenc => 4,
        Some("av1") if vulkan => 4,
        _ => 0,
    }
}
const ANCHOR: &str = "quasar-ref-anchor";
const RECOVER: &str = "quasar-ref-recover";
const FRAME: &str = "quasar-ref-frame";

/// Keep the next inter frame as an anchor. Send to the encoder's src pad.
pub(crate) fn anchor_event() -> gst::Event {
    gst::event::CustomUpstream::new(gst::Structure::new_empty(ANCHOR))
}

/// Code the next frame from the anchor whose PTS is `anchor_pts` only, and forget every
/// newer anchor; the encoder sends a key frame instead when it holds no such anchor.
/// `anchor_pts` is the one the anchor's [`RefFrame`] report carried: the encoder's
/// output pts, which on H.264 and HEVC is not the input buffer's (GstVideoEncoder's
/// min-pts offset). Send to the encoder's src pad.
pub(crate) fn recover_event(anchor_pts: gst::ClockTime) -> gst::Event {
    gst::event::CustomUpstream::new(
        gst::Structure::builder(RECOVER)
            .field("pts", anchor_pts.nseconds())
            .build(),
    )
}

/// What the encoder reported for one frame, just before that frame's buffer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub(crate) struct RefFrame {
    pub pts: gst::ClockTime,
    /// The frame is held as an anchor.
    pub anchor: bool,
    /// The frame codes from this anchor only.
    pub recover_from: Option<gst::ClockTime>,
}

/// The encoder's report, if `event` is one.
pub(crate) fn ref_frame(event: &gst::EventRef) -> Option<RefFrame> {
    let gst::EventView::CustomDownstream(custom) = event.view() else {
        return None;
    };
    let s = custom.structure().filter(|s| s.name() == FRAME)?;
    Some(RefFrame {
        pts: gst::ClockTime::from_nseconds(s.get::<u64>("pts").ok()?),
        anchor: s.get::<bool>("anchor").ok()?,
        recover_from: s
            .get::<u64>("recover-from")
            .ok()
            .map(gst::ClockTime::from_nseconds),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use gstreamer::prelude::*;

    #[test]
    fn anchors_follow_the_codec_and_nvenc_h264_and_av1_stay_off() {
        for (factory, k) in [
            ("vulkanh264enc", 2),
            ("vulkanh265enc", 4),
            ("vulkanav1enc", 4),
            ("nvh264enc", 0),
            ("nvcudah264enc", 0),
            ("nvautogpuh265enc", 4),
            ("nvh265enc", 4),
            ("nvav1enc", 0),
            ("vah264enc", 0),
            ("vah265enc", 0),
            ("openh264enc", 0),
        ] {
            assert_eq!(anchors_for(factory, true), k, "{factory}");
            assert_eq!(anchors_for(factory, false), 0, "{factory}, switch off");
        }
    }

    // The C side reads `pts` with gst_structure_get_uint64: any other type is ignored there.
    #[test]
    fn recover_names_the_anchor_as_a_u64_pts() {
        gst::init().unwrap();
        let e = recover_event(gst::ClockTime::from_mseconds(250));
        assert_eq!(e.type_(), gst::EventType::CustomUpstream);
        let s = e.structure().unwrap();
        assert_eq!(s.name(), "quasar-ref-recover");
        assert_eq!(s.get::<u64>("pts").unwrap(), 250_000_000);
        let a = anchor_event();
        assert_eq!(a.type_(), gst::EventType::CustomUpstream);
        assert_eq!(a.structure().unwrap().name(), "quasar-ref-anchor");
    }

    // Built the way gst_ref_recovery_report() builds it.
    #[test]
    fn reads_the_encoders_report() {
        gst::init().unwrap();
        let anchor = gst::event::CustomDownstream::new(
            gst::Structure::builder("quasar-ref-frame")
                .field("pts", 16_666_666u64)
                .field("anchor", true)
                .build(),
        );
        assert_eq!(
            ref_frame(&anchor),
            Some(RefFrame {
                pts: gst::ClockTime::from_nseconds(16_666_666),
                anchor: true,
                recover_from: None
            })
        );
        let recovery = gst::event::CustomDownstream::new(
            gst::Structure::builder("quasar-ref-frame")
                .field("pts", 50_000_000u64)
                .field("anchor", false)
                .field("recover-from", 16_666_666u64)
                .build(),
        );
        assert_eq!(
            ref_frame(&recovery).unwrap().recover_from,
            Some(gst::ClockTime::from_nseconds(16_666_666))
        );
        let other = gst::event::CustomDownstream::new(gst::Structure::new_empty("other"));
        assert_eq!(ref_frame(&other), None);
        assert_eq!(ref_frame(&anchor_event()), None);
    }

    /// One frame of the capture script: what the harness sends before frame `at`.
    enum Step {
        Anchor,
        /// Recover from the anchor taken at this input frame.
        From(usize),
        /// Recover from the newest anchor the encoder reported.
        FromNewest,
        /// Recover from the frame taken at this input frame, which the encoder no
        /// longer holds as an anchor: it must send a key frame.
        Gone(usize),
        /// Force a key frame (GstForceKeyUnit, as the agent's `force_idr`).
        Key,
    }

    /// Input frames the harness's checker treats as lost, inclusive ranges. Every
    /// recovery below follows a loss of the frames since its anchor's chain.
    const LOST: [(usize, usize); 8] = [
        (21, 34),
        (41, 49),
        (59, 65),
        (67, 69),
        (72, 79),
        (81, 84),
        (101, 104),
        // anchor 112 lost, 14 frames before the recovery that forgets it
        (109, 125),
    ];
    const SCRIPT: [(usize, Step); 23] = [
        (10, Step::Anchor),
        (20, Step::Anchor),
        (30, Step::Anchor),
        // forgets 30, the anchor newer than 20
        (35, Step::From(20)),
        (40, Step::Anchor),
        (50, Step::From(40)),
        // 10 is still held (35 forgot only 30); forgets 20 and 40
        (55, Step::From(10)),
        (58, Step::Anchor),
        (63, Step::From(58)),
        // 63 itself lost: the same anchor again
        (66, Step::From(58)),
        (70, Step::From(58)),
        // on H.264 the anchor is 71: a recovery frame cannot be one there
        (70, Step::Anchor),
        (80, Step::FromNewest),
        // 30 was forgotten at 35
        (85, Step::Gone(30)),
        // a fourth anchor evicts the oldest of three
        (88, Step::Anchor),
        (92, Step::Anchor),
        (96, Step::Anchor),
        (100, Step::Anchor),
        (105, Step::Gone(88)),
        (108, Step::Anchor),
        (112, Step::Anchor),
        // forgets 112, older than the last eight frames
        (126, Step::From(108)),
        (130, Step::Key),
    ];
    const FRAMES: usize = 136;

    /// How the encoder under test holds anchors.
    #[derive(Clone, Copy)]
    struct Model {
        /// num-anchors in effect; 0 holds none, so every recovery is a key frame.
        k: usize,
        /// A key frame `gop` frames after the last one.
        gop: usize,
        /// The H.264 anchor waits.
        h264: bool,
        /// NVENC H.264's long-term indices: an index whose anchor a recovery forgot
        /// is retired until the next key frame (`gstreamer-nvenc-ref-recovery.patch`).
        slots: Option<usize>,
    }

    /// What the encoder must make of [`SCRIPT`], by input frame: the anchors, the
    /// recoveries (frame, anchor) and the key frames. An anchor request waits out a
    /// key frame and, on H.264, a recovery frame or the frame after an anchor; a
    /// recovery from an anchor not held is a key frame; a key frame drops every
    /// anchor (`gstrefrecovery.h`). The checker requires exactly this.
    fn expected(m: Model) -> serde_json::Value {
        let (mut anchors, mut recoveries, mut keys) = (Vec::new(), Vec::new(), vec![0]);
        let (mut held, mut retired, mut pending, mut last_key) = (Vec::new(), 0, false, 0);
        for at in 1..FRAMES {
            let (mut key, mut asked) = (at - last_key >= m.gop, None);
            for (_, step) in SCRIPT.iter().filter(|(i, _)| *i == at) {
                match step {
                    Step::Anchor => pending = true,
                    Step::Key => key = true,
                    Step::From(i) | Step::Gone(i) => asked = Some(Some(*i)),
                    Step::FromNewest => asked = Some(anchors.last().copied()),
                }
            }
            let mut from = None;
            if let (Some(target), false) = (asked, key) {
                match target.filter(|a| held.contains(a)) {
                    Some(a) => {
                        let before = held.len();
                        held.retain(|h| *h <= a);
                        if m.slots.is_some() {
                            retired += before - held.len();
                        }
                        from = Some(a);
                    }
                    None => key = true,
                }
            }
            if key {
                keys.push(at);
                (held, retired, last_key) = (Vec::new(), 0, at);
                continue;
            }
            if let Some(a) = from {
                recoveries.push((at, a));
            }
            let waits = m.h264 && (from.is_some() || anchors.last() == Some(&(at - 1)));
            if pending && m.k == 0 {
                pending = false;
            } else if pending && !waits {
                let free = m.slots.is_none_or(|s| held.len() + retired < s);
                if held.len() >= m.k || !free {
                    held.remove(0);
                }
                held.push(at);
                anchors.push(at);
                pending = false;
            }
        }
        serde_json::json!({ "anchors": anchors, "recoveries": recoveries, "keys": keys })
    }

    #[test]
    fn expected_follows_the_script() {
        let m = |k, gop, h264, slots| Model {
            k,
            gop,
            h264,
            slots,
        };
        let recoveries = |newest: usize| {
            serde_json::json!([
                [35, 20],
                [50, 40],
                [55, 10],
                [63, 58],
                [66, 58],
                [70, 58],
                [80, newest],
                [126, 108]
            ])
        };
        let tail = [88, 92, 96, 100, 108, 112];
        for model in [m(3, 1000, false, None), m(3, 1000, true, Some(8))] {
            let newest = if model.h264 { 71 } else { 70 };
            let mut want_anchors = vec![10, 20, 30, 40, 58, newest];
            want_anchors.extend(tail);
            assert_eq!(
                expected(model),
                serde_json::json!({
                    "anchors": want_anchors,
                    "recoveries": recoveries(newest),
                    "keys": [0, 85, 105, 130],
                })
            );
        }
        // nothing held: every recovery a key frame
        assert_eq!(
            expected(m(0, 1000, false, None)),
            serde_json::json!({
                "anchors": [],
                "recoveries": [],
                "keys": [0, 35, 50, 55, 63, 66, 70, 80, 85, 105, 126, 130],
            })
        );
        // three indices, two anchors: 35 retires 30's index, so 40 replaces 10 (held
        // 20) and 55 finds 10 gone
        assert_eq!(
            expected(m(2, 1000, true, Some(3))),
            serde_json::json!({
                "anchors": [10, 20, 30, 40, 58, 71, 88, 92, 96, 100, 108, 112],
                "recoveries": [[35, 20], [50, 40], [63, 58], [66, 58], [70, 58], [80, 71], [126, 108]],
                "keys": [0, 55, 85, 105, 130],
            })
        );
        // the agent's GOP: the key frame at 60 drops 58
        assert_eq!(
            expected(m(3, 60, true, Some(8))),
            serde_json::json!({
                "anchors": [10, 20, 30, 40, 58, 71, 88, 92, 96, 100, 108, 112],
                "recoveries": [[35, 20], [50, 40], [55, 10], [80, 71], [126, 108]],
                "keys": [0, 60, 63, 66, 70, 85, 105, 130],
            })
        );
    }

    /// The encoder keeps no input buffer of a frame it holds as a reference: the chain
    /// picture and the anchors code from the DPB. Holding them pinned K + 1 slots of a fixed
    /// ring in front of the encoder, and after a resolution step `vulkanscale`'s 4-slot ring
    /// ran dry and the session failed (`QUASAR_WT_REF_RECOVERY=on`, 0.6.5).
    /// `$QUASAR_REF_RECOVERY_CODEC` is `h264` (default), `h265` or `av1`.
    #[test]
    #[ignore = "needs a Vulkan encode GPU and the patched encoders"]
    fn anchors_hold_no_input_buffer() {
        use std::collections::VecDeque;
        use std::sync::{Arc, Mutex};

        let codec = std::env::var("QUASAR_REF_RECOVERY_CODEC").unwrap_or("h264".into());
        let (enc_name, caps) = match codec.as_str() {
            "h264" => ("vulkanh264enc", "video/x-h264,profile=constrained-baseline"),
            "h265" => ("vulkanh265enc", "video/x-h265,profile=main"),
            "av1" => ("vulkanav1enc", "video/x-av1"),
            other => panic!("QUASAR_REF_RECOVERY_CODEC={other}"),
        };
        let k = anchors_for(enc_name, true);
        gst::init().unwrap();
        let pipeline = gst::parse::launch(&format!(
            "videotestsrc is-live=true pattern=smpte horizontal-speed=3 num-buffers=40 \
             ! video/x-raw,format=NV12,width=640,height=360,framerate=60/1 ! vulkanupload \
             ! {enc_name} name=enc num-anchors={k} idr-period=1000 ! {caps} ! fakesink"
        ))
        .unwrap()
        .downcast::<gst::Pipeline>()
        .unwrap();
        let enc = pipeline.by_name("enc").unwrap();
        assert!(
            enc.find_property(NUM_ANCHORS).is_some(),
            "{enc_name} is not patched"
        );

        // Before each frame the encoder has coded every earlier one (live: no output
        // delay). Count the earlier inputs someone besides this test still holds, leaving out
        // the newest (its encode operation may keep it until the next). An anchor is asked
        // for at every frame; H.264 takes one every second frame.
        let inputs: Arc<Mutex<VecDeque<gst::Buffer>>> = Arc::default();
        let kept = Arc::new(Mutex::new(0));
        let anchors = Arc::new(Mutex::new(0));
        {
            let (inputs, kept) = (inputs.clone(), kept.clone());
            enc.static_pad("sink").unwrap().add_probe(
                gst::PadProbeType::BUFFER,
                move |pad, info| {
                    let mut held = inputs.lock().unwrap();
                    let now = held
                        .iter()
                        .rev()
                        .skip(1)
                        .filter(|b| !b.is_writable())
                        .count();
                    let mut kept = kept.lock().unwrap();
                    *kept = (*kept).max(now);
                    held.push_back(info.buffer().unwrap().to_owned());
                    if held.len() > 12 {
                        held.pop_front();
                    }
                    let src = pad.parent_element().unwrap().static_pad("src").unwrap();
                    assert!(src.send_event(anchor_event()));
                    gst::PadProbeReturn::Ok
                },
            );
        }
        {
            let anchors = anchors.clone();
            enc.static_pad("src").unwrap().add_probe(
                gst::PadProbeType::EVENT_DOWNSTREAM,
                move |_, info| {
                    if let Some(gst::PadProbeData::Event(e)) = &info.data {
                        if ref_frame(e).is_some_and(|r| r.anchor) {
                            *anchors.lock().unwrap() += 1;
                        }
                    }
                    gst::PadProbeReturn::Ok
                },
            );
        }

        pipeline.set_state(gst::State::Playing).unwrap();
        let bus = pipeline.bus().unwrap();
        let msg = bus
            .timed_pop_filtered(
                gst::ClockTime::from_seconds(30),
                &[gst::MessageType::Eos, gst::MessageType::Error],
            )
            .expect("no EOS in 30 s");
        assert!(matches!(msg.view(), gst::MessageView::Eos(_)), "{msg:?}");
        let kept = *kept.lock().unwrap();
        let anchors = *anchors.lock().unwrap();
        pipeline.set_state(gst::State::Null).unwrap();
        assert!(
            anchors >= 2,
            "only {anchors} anchors reported: the test proves nothing"
        );
        assert_eq!(
            kept, 0,
            "{enc_name} with {k} anchors keeps {kept} older input buffers"
        );
    }

    /// Hardware capture for `scripts/harness/run-ref-recovery.sh`: encodes a moving test
    /// pattern with `num-anchors=3`, sends [`SCRIPT`], and writes the access units, the
    /// raw input and the encoder's reports to `$QUASAR_REF_RECOVERY_OUT`.
    /// `$QUASAR_REF_RECOVERY_ENCODER` is `vulkan` (default) or `nvenc`; `_ANCHORS`
    /// (default 3), `_GOP` (1000), `_SIZE` (640x360) and `_SLOTS` (NVENC H.264's
    /// long-term indices at that size, when fewer than the script uses) set the run.
    #[test]
    #[ignore = "needs an encode GPU: scripts/harness/run-ref-recovery.sh"]
    fn capture_for_harness() {
        use std::io::Write;
        use std::sync::{Arc, Mutex};

        let out = std::path::PathBuf::from(std::env::var("QUASAR_REF_RECOVERY_OUT").unwrap());
        let codec = std::env::var("QUASAR_REF_RECOVERY_CODEC").unwrap();
        let encoder = std::env::var("QUASAR_REF_RECOVERY_ENCODER").unwrap_or("vulkan".into());
        let env = |name: &str, default: &str| {
            std::env::var(format!("QUASAR_REF_RECOVERY_{name}")).unwrap_or(default.into())
        };
        let k: usize = env("ANCHORS", "3").parse().unwrap();
        let gop: usize = env("GOP", "1000").parse().unwrap();
        let size = env("SIZE", "640x360");
        let (width, height) = size.split_once('x').unwrap();
        let slots = std::env::var("QUASAR_REF_RECOVERY_SLOTS")
            .ok()
            .map(|s| s.parse().unwrap());
        let (enc_name, caps, parser, out_caps) = match codec.as_str() {
            "h264" => (
                "vulkanh264enc",
                "video/x-h264,profile=constrained-baseline",
                "h264parse",
                "video/x-h264,stream-format=byte-stream,alignment=au",
            ),
            "h265" => (
                "vulkanh265enc",
                "video/x-h265,profile=main",
                "h265parse",
                "video/x-h265,stream-format=byte-stream,alignment=au",
            ),
            "av1" => (
                "vulkanav1enc",
                "video/x-av1",
                "av1parse",
                "video/x-av1,stream-format=obu-stream,alignment=tu",
            ),
            other => panic!("QUASAR_REF_RECOVERY_CODEC={other}"),
        };
        // the NVENC encoder settings the agent uses (encoders.rs), less bitrate
        let (upload, enc_name, gop_prop) = match encoder.as_str() {
            "vulkan" => (
                "vulkanupload !",
                enc_name.to_string(),
                format!("idr-period={gop}"),
            ),
            "nvenc" => (
                "",
                // the CUDA-mode elements are nv<codec>enc on GStreamer 1.28
                enc_name.replace("vulkan", "nv"),
                format!(
                    "gop-size={gop} preset=p4 tune=ultra-low-latency rc-mode=cbr rc-lookahead=0"
                ),
            ),
            other => panic!("QUASAR_REF_RECOVERY_ENCODER={other}"),
        };
        // NVENC API 12.0 has no AV1 long-term references (gstreamer-nvenc-ref-recovery.patch)
        let model = Model {
            k: if encoder == "nvenc" && codec == "av1" {
                0
            } else {
                k
            },
            gop,
            h264: codec == "h264",
            slots,
        };
        let held = model.k > 0;
        gst::init().unwrap();
        let desc = format!(
            "videotestsrc is-live=true pattern=smpte horizontal-speed=3 num-buffers={FRAMES} \
             ! video/x-raw,format=NV12,width={width},height={height},framerate=60/1 ! tee name=t \
             t. ! queue ! appsink name=raw sync=false \
             t. ! queue ! {upload} {enc_name} name=enc num-anchors={k} {gop_prop} \
             ! {caps} ! {parser} name=parse config-interval=-1 ! {out_caps} \
             ! appsink name=enc_out sync=false"
        );
        // av1parse has no config-interval
        let desc = if codec == "av1" {
            desc.replace(" config-interval=-1", "")
        } else {
            desc
        };
        let pipeline = gst::parse::launch(&desc)
            .unwrap()
            .downcast::<gst::Pipeline>()
            .unwrap();
        let enc = pipeline.by_name("enc").unwrap();
        assert!(
            enc.find_property(NUM_ANCHORS).is_some(),
            "{enc_name} is not patched"
        );

        let reports: Arc<Mutex<Vec<RefFrame>>> = Arc::default();
        let input_pts: Arc<Mutex<Vec<gst::ClockTime>>> = Arc::default();
        {
            let reports = reports.clone();
            pipeline
                .by_name("parse")
                .unwrap()
                .static_pad("src")
                .unwrap()
                .add_probe(gst::PadProbeType::EVENT_DOWNSTREAM, move |_, info| {
                    if let Some(gst::PadProbeData::Event(e)) = &info.data {
                        if let Some(r) = ref_frame(e) {
                            reports.lock().unwrap().push(r);
                        }
                    }
                    gst::PadProbeReturn::Ok
                });
        }
        {
            let (reports, input_pts) = (reports.clone(), input_pts.clone());
            enc.static_pad("sink").unwrap().add_probe(
                gst::PadProbeType::BUFFER,
                move |pad, info| {
                    let Some(pts) = info.buffer().and_then(|b| b.pts()) else {
                        return gst::PadProbeReturn::Ok;
                    };
                    let mut seen = input_pts.lock().unwrap();
                    let at = seen.len();
                    seen.push(pts);
                    let src = pad.parent_element().unwrap().static_pad("src").unwrap();
                    for (_, step) in SCRIPT.iter().filter(|(i, _)| *i == at) {
                        let event = match step {
                            Step::Anchor => anchor_event(),
                            Step::Key => gstreamer_video::UpstreamForceKeyUnitEvent::builder()
                                .all_headers(true)
                                .build(),
                            // no anchor reports to name one by: any pts is not held
                            Step::From(i) | Step::Gone(i) if !held => recover_event(seen[*i]),
                            Step::FromNewest if !held => recover_event(seen[at - 1]),
                            Step::From(i) | Step::Gone(i) => {
                                // the pts the anchor's report carried, not its input
                                // pts: the H.264 and HEVC encoders offset theirs
                                let n = SCRIPT
                                    .iter()
                                    .filter(|(a, s)| matches!(s, Step::Anchor) && a < i)
                                    .count();
                                let r = reports.lock().unwrap();
                                recover_event(r.iter().filter(|r| r.anchor).nth(n).unwrap().pts)
                            }
                            Step::FromNewest => {
                                let r = reports.lock().unwrap();
                                recover_event(r.iter().rev().find(|r| r.anchor).unwrap().pts)
                            }
                        };
                        assert!(src.send_event(event));
                    }
                    gst::PadProbeReturn::Ok
                },
            );
        }

        pipeline.set_state(gst::State::Playing).unwrap();
        let pull = |name: &str| -> Vec<gst::Sample> {
            let sink = pipeline
                .by_name(name)
                .unwrap()
                .downcast::<gstreamer_app::AppSink>()
                .unwrap();
            std::iter::from_fn(|| sink.pull_sample().ok()).collect()
        };
        let (au, raw) = std::thread::scope(|s| {
            let raw = s.spawn(|| pull("raw"));
            (pull("enc_out"), raw.join().unwrap())
        });
        pipeline.set_state(gst::State::Null).unwrap();
        assert_eq!(au.len(), FRAMES, "one access unit per input frame");

        std::fs::create_dir_all(&out).unwrap();
        let mut bin = std::fs::File::create(out.join(format!("{codec}.bin"))).unwrap();
        let mut frames = Vec::new();
        for s in &au {
            let b = s.buffer().unwrap();
            let map = b.map_readable().unwrap();
            bin.write_all(&(map.len() as u32).to_le_bytes()).unwrap();
            bin.write_all(&map).unwrap();
            frames.push(serde_json::json!({
                "pts": b.pts().unwrap().nseconds(),
                "key": !b.flags().contains(gst::BufferFlags::DELTA_UNIT),
            }));
        }
        let mut yuv = std::fs::File::create(out.join(format!("{codec}.nv12"))).unwrap();
        for s in &raw {
            yuv.write_all(&s.buffer().unwrap().map_readable().unwrap())
                .unwrap();
        }
        let reports: Vec<_> = reports
            .lock()
            .unwrap()
            .iter()
            .map(|r| {
                serde_json::json!({
                    "pts": r.pts.nseconds(),
                    "anchor": r.anchor,
                    "recover_from": r.recover_from.map(|p| p.nseconds()),
                })
            })
            .collect();
        let meta = serde_json::json!({
            "codec": codec,
            "encoder": encoder,
            "num_anchors": k,
            "gop": gop,
            "width": width.parse::<u32>().unwrap(),
            "height": height.parse::<u32>().unwrap(),
            "input_pts": input_pts.lock().unwrap().iter().map(|p| p.nseconds()).collect::<Vec<_>>(),
            "lost": LOST,
            "expect": expected(model),
            "frames": frames,
            "reports": reports,
        });
        std::fs::write(
            out.join(format!("{codec}.json")),
            serde_json::to_vec_pretty(&meta).unwrap(),
        )
        .unwrap();
    }
}
