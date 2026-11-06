//! The transport seam (#1028): a transport that is not WebRTC takes the production encode
//! chain at the encoded-frame boundary, and nothing of RTP or WebRTC is built for it.

use super::*;
use crate::session::pipeline::{EncodedSession, Transport};

/// Each access unit the transport received: its PTS, and whether it is a keyframe.
type Units = Arc<Mutex<Vec<(Option<gst::ClockTime>, bool)>>>;

struct FakeTransport {
    units: Units,
    caps: gst::Pad,
    encoder: gst::Element,
    released: AtomicBool,
}

impl Transport for FakeTransport {
    fn attach(session: EncodedSession) -> anyhow::Result<Self> {
        let sink = gst::ElementFactory::make("fakesink")
            .property("sync", false)
            .build()?;
        session.pipeline.add(&sink)?;
        let sink_pad = sink.static_pad("sink").expect("fakesink sink");
        session.video.link(&sink_pad)?;
        let units = Units::default();
        let seen = units.clone();
        sink_pad.add_probe(gst::PadProbeType::BUFFER, move |_, info| {
            if let Some(buffer) = info.buffer() {
                let keyframe = !buffer.flags().contains(gst::BufferFlags::DELTA_UNIT);
                seen.lock()
                    .unwrap_or_else(|e| e.into_inner())
                    .push((buffer.pts(), keyframe));
            }
            gst::PadProbeReturn::Ok
        });
        session.pipeline.set_state(gst::State::Ready)?;
        Ok(Self {
            units,
            caps: sink_pad,
            encoder: session.encoder,
            released: AtomicBool::new(false),
        })
    }

    fn release(&self) {
        self.released.store(true, Ordering::SeqCst);
    }
}

fn wait_for(what: &str, mut done: impl FnMut() -> bool) {
    let deadline = Instant::now() + Duration::from_secs(10);
    while !done() {
        assert!(Instant::now() < deadline, "timed out waiting for {what}");
        std::thread::sleep(Duration::from_millis(20));
    }
}

#[test]
fn a_second_transport_takes_access_units_without_rtp_or_webrtc() {
    if !live_webrtc_available() {
        return;
    }
    let _serial = serial();
    let mut cfg = host_cfg(false);
    let resolved = resolve_effective_encoder(&mut cfg).expect("resolve encoder");
    let sink = format!("seam-{}", std::process::id());
    let source = build_source_pipeline(&cfg, None, &sink).expect("source pipeline");
    let (sig_tx, _sig_rx) = mpsc::unbounded_channel();
    let (trace_tx, _trace_rx) = mpsc::channel(256);
    let trace = DiagnosticEventTx::new(
        trace_tx,
        Arc::new(AtomicU64::new(0)),
        Arc::new(AtomicU64::new(0)),
    );
    let EncodePipeline {
        pipeline,
        transport,
        interpipesrc: _interpipesrc,
        resolution_lever: _lever,
    } = build_encode_pipeline::<FakeTransport>(
        &cfg,
        sig_tx,
        None,
        Arc::new(crate::session::input::InputState::new()),
        &sink,
        Arc::new(SessionMetrics::new("smooth", cfg.stream.fps as u32)),
        None,
        trace,
        "seam".to_string(),
        &resolved.factory,
    )
    .expect("encode pipeline");

    // Nothing of RTP or WebRTC exists unless the transport builds it.
    for element in pipeline.iterate_recurse().into_iter().flatten() {
        let factory = element.factory().map(|f| f.name().to_string());
        let klass = element.factory().map(|f| {
            f.metadata(gst::ELEMENT_METADATA_KLASS)
                .unwrap_or("")
                .to_string()
        });
        assert!(
            !klass.as_deref().unwrap_or("").contains("Payloader")
                && factory.as_deref() != Some("webrtcbin"),
            "the encode chain built {factory:?} for a transport that is not WebRTC"
        );
    }

    let clock = gst::SystemClock::obtain();
    let base = clock.time();
    for p in [&source, &pipeline] {
        p.set_start_time(None::<gst::ClockTime>);
        p.use_clock(Some(&clock));
        p.set_base_time(base);
    }
    source
        .set_state(gst::State::Playing)
        .expect("source PLAYING");
    pipeline
        .set_state(gst::State::Playing)
        .expect("encode PLAYING");

    wait_for("ten access units", || {
        transport.units.lock().unwrap().len() >= 10
    });
    let caps = transport.caps.current_caps().expect("negotiated caps");
    assert_eq!(caps.structure(0).unwrap().name(), "video/x-h264");
    let keyframes = |units: &Units| units.lock().unwrap().iter().filter(|u| u.1).count();
    {
        let units = transport.units.lock().unwrap();
        assert!(units[0].1, "the first access unit is a keyframe");
        assert!(units[1..].iter().any(|u| !u.1), "later units are deltas");
        assert!(units[0].0.is_some(), "the keyframe has a PTS");
        // openh264's second unit carries none; the rest do.
        let pts: Vec<_> = units.iter().filter_map(|u| u.0).collect();
        assert!(
            pts.len() + 1 >= units.len(),
            "units without a PTS: {units:?}"
        );
        assert!(pts.windows(2).all(|w| w[0] < w[1]), "PTS rises: {pts:?}");
    }

    // The keyframe request is the encoder's, whatever the transport.
    let before = keyframes(&transport.units);
    super::super::resize::force_idr(&transport.encoder);
    wait_for("the requested keyframe", || {
        keyframes(&transport.units) > before
    });

    transport.release();
    assert!(transport.released.load(Ordering::SeqCst));
    let _ = pipeline.set_state(gst::State::Null);
    let _ = source.set_state(gst::State::Null);
}
