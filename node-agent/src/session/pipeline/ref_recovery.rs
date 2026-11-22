//! Reference recovery control of the patched Vulkan encoders (#1348): ask for an anchor,
//! ask for the next frame to code from one, read which frames did. The encoder half is
//! `gstrefrecovery.h` in `deploy/patches/vulkan/*-ref-recovery.patch`; the names and
//! field types here must match it.
// The WebTransport repair loop that uses these lands with the next #1348 unit.
#![allow(dead_code)]

use gstreamer as gst;

/// Encoder property: anchors held, 0 = off. Present only on a patched encoder.
pub(crate) const NUM_ANCHORS: &str = "num-anchors";
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
    }

    /// Input frames the harness's checker treats as lost, inclusive ranges. Every
    /// recovery below follows a loss of the frames since its anchor's chain.
    const LOST: [(usize, usize); 7] = [
        (21, 34),
        (41, 49),
        (59, 65),
        (67, 69),
        (72, 79),
        (81, 84),
        (101, 104),
    ];
    const SCRIPT: [(usize, Step); 19] = [
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
    ];
    const FRAMES: usize = 110;

    /// What the encoder must make of [`SCRIPT`], by input frame: the anchors, the
    /// recoveries (frame, anchor) and the key frames. An anchor request waits out a
    /// key frame and, on H.264, a recovery frame or the frame after an anchor
    /// (`gstrefrecovery.h`). The checker requires exactly this.
    fn expected(codec: &str) -> serde_json::Value {
        let (mut anchors, mut recoveries, mut keys) = (Vec::new(), Vec::new(), vec![0]);
        let mut pending = false;
        for at in 1..FRAMES {
            let (mut from, mut key) = (None, false);
            for (_, step) in SCRIPT.iter().filter(|(i, _)| *i == at) {
                match step {
                    Step::Anchor => pending = true,
                    Step::From(i) => from = Some(*i),
                    Step::FromNewest => from = anchors.last().copied(),
                    Step::Gone(_) => key = true,
                }
            }
            if key {
                keys.push(at);
                continue;
            }
            if let Some(a) = from {
                recoveries.push((at, a));
            }
            let waits =
                codec == "h264" && (from.is_some() || anchors.last() == Some(&(at - 1)));
            if pending && !waits {
                anchors.push(at);
                pending = false;
            }
        }
        serde_json::json!({ "anchors": anchors, "recoveries": recoveries, "keys": keys })
    }

    #[test]
    fn expected_follows_the_script() {
        let recoveries = |newest: usize| {
            serde_json::json!([[35, 20], [50, 40], [55, 10], [63, 58], [66, 58], [70, 58], [80, newest]])
        };
        for codec in ["av1", "h265"] {
            assert_eq!(
                expected(codec),
                serde_json::json!({
                    "anchors": [10, 20, 30, 40, 58, 70, 88, 92, 96, 100],
                    "recoveries": recoveries(70),
                    "keys": [0, 85, 105],
                })
            );
        }
        assert_eq!(
            expected("h264"),
            serde_json::json!({
                "anchors": [10, 20, 30, 40, 58, 71, 88, 92, 96, 100],
                "recoveries": recoveries(71),
                "keys": [0, 85, 105],
            })
        );
    }

    /// Hardware capture for `scripts/harness/run-ref-recovery.sh`: encodes a moving test
    /// pattern with `num-anchors=3`, sends [`SCRIPT`], and writes the access units, the
    /// raw input and the encoder's reports to `$QUASAR_REF_RECOVERY_OUT`.
    #[test]
    #[ignore = "needs a Vulkan encode GPU: scripts/harness/run-ref-recovery.sh"]
    fn capture_for_harness() {
        use std::io::Write;
        use std::sync::{Arc, Mutex};

        let out = std::path::PathBuf::from(std::env::var("QUASAR_REF_RECOVERY_OUT").unwrap());
        let codec = std::env::var("QUASAR_REF_RECOVERY_CODEC").unwrap();
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
        gst::init().unwrap();
        let desc = format!(
            "videotestsrc is-live=true pattern=smpte horizontal-speed=3 num-buffers={FRAMES} \
             ! video/x-raw,format=NV12,width=640,height=360,framerate=60/1 ! tee name=t \
             t. ! queue ! appsink name=raw sync=false \
             t. ! queue ! vulkanupload ! {enc_name} name=enc num-anchors=3 idr-period=1000 \
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
            "width": 640,
            "height": 360,
            "input_pts": input_pts.lock().unwrap().iter().map(|p| p.nseconds()).collect::<Vec<_>>(),
            "lost": LOST,
            "expect": expected(&codec),
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
